# gha-runner-tui

A terminal dashboard and SSH-friendly CLI for ephemeral GitHub Actions Docker runners, with one systemd loop per profile and a configurable host-wide job gate (one job by default).

## Build and local validation

Run from `src` (the Go module directory):

```bash
go build ./cmd/gha-runner-tui
go build ./cmd/gha-ephemeral-loop
go test ./internal/tui -v
go test ./...
go vet ./...
GOOS=linux GOARCH=amd64 go build ./...
```

Tests use fake command runners, temporary files, injected clocks/locks and local httptest servers, not real Docker, GitHub or systemd. Native tests and a Linux cross-build do **not** validate the external busy-runner DELETE contract or real Linux/systemd signal delivery. Both remain pending deployment gates; this is not a fully automatic deployment tool.

## Paths and configuration

Defaults:

```text
/etc/gha-runner-tui/config.yaml
/etc/gha-runner-tui/profiles/*.yaml
/var/lib/gha-runner-tui/state/*.json
/var/log/gha-runner-tui/<profile>/
/etc/systemd/system/gha-<profile>.service
/usr/local/bin/gha-ephemeral-loop
```

The global YAML config selects paths, GitHub API/default credentials, and workflow Docker access. A profile selects its target, image, labels, resources, credentials and loop settings. For example, these are **profile fragments**, not complete profiles:

```yaml
github:
  token_env: GITHUB_TOKEN
  env_file: /etc/gha-runner-tui/model-router.env
runner:
  ephemeral: true
  labels: [self-hosted, linux, x64, docker]
  watch_repositories: [example/model-router]
loop:
  poll_interval_seconds: 30
  idle_timeout_seconds: 180
```

- Repository profiles default to watching their own `owner/repo`. An explicit watch list may contain only that repository.
- Organization profiles require `runner.watch_repositories` entries in `owner/repo` form. Credentials need Actions: read for every watched repository, plus runner administration permissions for the target.
- Watch must include the repository producing the workflow; runner labels must cover all that job's `runs-on` labels, case-insensitively. `self-hosted` is implicit for matching. Empty job labels and nonmatching GitHub-hosted labels are ignored.
- Poll defaults to 30 seconds, clamps to a minimum of 10 seconds, with ±20% jitter. Idle timeout defaults to 180 seconds and clamps to a minimum of 60 seconds. Legacy `interval_seconds` is accepted in YAML but does not control new polling.
- New profiles are always ephemeral. Loop-only checks enforce ephemeral/watch settings; old profiles can still be loaded for read-only diagnosis. A loop-only validation failure records `failed` and its reason, then waits for SIGTERM rather than creating a `RestartSec=5` restart storm. YAML loading, required managed-name validation and state/log-directory errors can still fail startup.

### Session deadline admission protection (opt-in, best effort)

`loop.session_deadline_file` is optional. When it is unset, the protection is **not enabled** and the loop behaves exactly as described here. When set, it must point to a local JSON file:

```json
{"deadline":"2026-11-17T20:00:00Z"}
```

Every cycle re-reads that file, so an external coordinator can move the deadline without restarting the loop. The loop never writes, extends or infers it: uptime, SSH session age and process start time are not deadline sources, and the configured clock/deadline are trusted as given. While the file is configured, a missing, unreadable or invalid file, a missing/invalid RFC3339 time, an expired deadline, or less than 100 minutes remaining (90 minutes of work plus a 10-minute buffer) refuses **new** registration, creation and retry. The check runs before job polling and again after the host lock and registration token are obtained.

This is a best-effort admission guard, not an atomic 90-minute guarantee and not fail-closed at every stage:

- GitHub can assign a queued job between the last admission check and registration; the local check cannot reserve a job.
- A registered runner is adopted at startup. After the threshold, an idle runner is withdrawn only when GitHub reports it idle **and** DELETE returns 204; only then may its container be cleaned. Busy runners, HTTP 422 (a job was accepted), lookup errors and failed DELETEs retain the container and publish an incomplete-withdrawal diagnostic because the runner may still accept work. A DELETE/assignment race remains.
- Deadline-triggered withdrawal is not an idle timeout: it does not add candidate jobs to the in-memory 30-minute skip list, and it does not kill or remove a container on unverified data. Existing busy work may still run past the deadline.

README statements and local tests do not prove the external GitHub contract or real systemd races.

## Credentials: strict files, no silent identity switch

Profile `github.token_file` takes priority over `github.env_file`. A specified file must yield a token: missing, unreadable, empty, malformed or missing-key files fail, never fall back to the SSH process environment. A permission/read failure may try `sudo -n cat` internally; file contents are not printed.

The shared parser accepts a single bare token (including a trailing newline) and the existing env-file format. Env format requires the selected `token_env` key and a nonempty value; `GITHUB_TOKEN` is the default key. No online credential conversion is performed. A systemd `EnvironmentFile` still needs systemd-compatible env format, not a bare-token file; use `token_file` for a separate bare-token source.

Only unmigrated profiles with neither file configured in TUI/CLI, and the global runner-group client, use the global `github.env_file`. They fall back to the process environment **only when that global file does not exist**, not when it is unreadable or invalid. The loop does not read global config: without a profile credential file it uses its token environment, normally supplied by the unit. CLI/TUI use the global API URL; the loop uses GitHub's default API URL.

For a separate repository identity, point `--github-env-file` at its own file, such as `/etc/gha-runner-tui/model-router.env`. The user deploys credentials; never paste, export, inspect or print real tokens as part of these examples. Migrate existing profiles explicitly so CLI/TUI and loop select the same file.

## Docker: outer host versus inner workflow daemon

All **outer** Docker calls by loop, CLI and TUI use `--host unix:///var/run/docker.sock`, regardless of the caller's `DOCKER_HOST`. This is where managed runner containers and the host-wide slot are observed.

New profiles default to **inner workflow** `rootless` Docker access. Creation writes the configured host rootless socket mount to `/var/run/docker.sock` inside the runner and sets its `docker.env.DOCKER_HOST=unix:///var/run/docker.sock`:

```yaml
docker:
  default_access_mode: rootless
  rootless_socket_path: /run/user/1001/docker.sock
  auto_detect_rootless_socket: true
  allow_host_socket_opt_in: true
  host_socket_path: /var/run/docker.sock
```

This fragment belongs to **global** config. If no rootless socket is configured, narrow detection checks a Unix `DOCKER_HOST` and `/run/user/*/docker.sock`; zero or multiple usable candidates fail. There is no silent host-socket fallback. Explicit `--docker-access host-socket` requires `allow_host_socket_opt_in=true` and is unsafe: workflows gain control of the host root Docker daemon.

Rootless supports ordinary Docker build/pull/push/run, Compose and buildx uses; it does not promise privileged, host-network, host-root bind-mount or host-root daemon compatibility. Rootless workflow access does not mean the outer runner container is created by the rootless daemon.

## Queue-driven loop and graceful lifecycle

Each unit runs `gha-ephemeral-loop --config <profile-YAML>`. It first adopts its own retained managed containers and cleans its own terminal leftovers, then polls watched repositories. It reads one page of 100 runs for each of `queued`, `in_progress`, and `pending`, and one page of jobs per run with `filter=latest&per_page=100`; only queued jobs with matching nonempty labels are candidates. A pending run can contain a queued job; runs seen in multiple status queries are deduplicated. There is no pagination or fairness guarantee.

All loops on a host share these reads through `/run/gha-runner-tui/github` (0700, cleared on reboot), because the REST quota (5000 requests/hour for a personal token) is per user, not per loop. Every GET is sent as a conditional request with the last ETag and reuses the stored body on `304 Not Modified`, which GitHub does not count against the primary rate limit. The unfiltered queued-job scan of a repository is shared for 20 seconds: one loop scans under a per-repository `flock` while the others wait and then apply their own labels. Requesting a registration token drops that repository's snapshot (every snapshot for an organization runner), so the next loop rescans instead of launching a surplus runner for a job that was just claimed. Entries are keyed by API URL, a SHA-256 of the token and the request path; failed responses are never stored, and an unusable cache directory (not owned by the loop user, or a symlink) falls back to direct requests.

With candidates, the loop takes nonblocking `flock(LOCK_EX|LOCK_NB)` on `/run/gha-runner-tui/host.lock` (directory 0755, file 0600), rechecks all managed containers, then registers and launches at most one runner. Only loops take this lock; its path is not configurable through CLI/YAML. Managed labels are:

```text
io.gha-runner-tui.managed=true
io.gha-runner-tui.profile=<profile.name>
io.gha-runner-tui.runner=<runner name>
```

`created`, `running`, `restarting` and `paused` occupy the slot; only `exited`, `dead` and `removing` do not. Unknown states and query errors fail closed. Lock contention or a full host capacity means `waiting-host`; without a current container this is healthy, not a failure. `failed` remains unhealthy. A sleeping ephemeral profile with no container and a deregistered (`gone`) runner can also be healthy. State JSON is diagnostic; Docker supplies live occupancy. CLI `status` shows `SLOT: free`, the holder(s), the occupied count for multiple holders, or `unknown` on query failure; the TUI does not add another scheduling entry point or slot control.

While waiting, normal inspect cadence is 15 seconds. A retained `created` container gets 60 seconds from creation before a **non-force** removal attempt; failed/unknown removal retains the slot and rechecks rather than assuming it is free. A runner never seen in GitHub is never automatically force-deleted: it can block the host indefinitely, requiring an explicit operator decision.

Idle cleanup requires authoritative evidence: DELETE HTTP 204 authorizes force removal, HTTP 422 keeps waiting, other errors retain the container; a previously seen runner missing on two consecutive checks also authorizes removal. After idle cleanup, candidate job IDs are skipped **in memory for 30 minutes**. Restart loses that skip list; it is not persistent scheduling. Deadline-triggered withdrawal does not use this idle skip list.

Stop/restart call only systemctl, not Docker kill: **停止后不再接新 job；正在运行的 job 会继续跑完**. Restart starts the loop again and adopts a retained container. A stopped service does not prove a finished job or a free slot. On SIGTERM, the loop drains/checks for up to 60 seconds at 5-second intervals, then can exit leaving the container occupying the slot. A launch already in progress uses an independent 120-second context and `docker run --sig-proxy=false`, not signal cancellation. Completed-container logs are redacted before persistence.

New units include `KillMode=mixed` and `TimeoutStopSec=240`, with `Restart=always` / `RestartSec=5`. Existing units need the separately authorized stop-signal drop-in from the external deployment procedure below; `migrate` does not install it. These settings and local tests are not evidence that real systemd startup/stop races are safe.

**Destructive recovery:** `stop <profile> --force` removes that profile's slot holders, including `created`; running jobs will fail. TUI `k` explicitly kills the current runner container and warns about failing a busy job. Neither action is part of ordinary stop/restart.

## CLI commands

The following are usage examples on an already configured host, **not authorization to deploy or operate remote services**. Use installed binaries or substitute the local build paths.

No arguments, or a first argument beginning with `-`, opens the TUI:

```bash
gha-runner-tui
gha-runner-tui -config /etc/gha-runner-tui/config.yaml -systemd-unit-dir /etc/systemd/system
```

CLI `--config` defaults to `/etc/gha-runner-tui/config.yaml`. Flags may follow a profile positional argument, e.g.:

```bash
gha-runner-tui logs model-router --docker -n 20 --config /etc/gha-runner-tui/config.yaml
gha-runner-tui stop model-router --force
```

| Command | Behavior |
|---|---|
| `status [--json] [--config PATH]` | Read-only live slot/profile status; JSON uses a narrow DTO, never Docker Env or raw inspect data. Per-profile errors remain visible without blocking other profiles. Slot/profile observation failures are reported in the payload; status can still exit 0. |
| `logs PROFILE [--docker] [-n 200] [--config PATH]` | Default: journal for the profile unit. `--docker`: current/latest matching container logs; no container is an error. No CLI follow option. |
| `start PROFILE [--config PATH]` | systemctl start. |
| `stop PROFILE [--force] [--config PATH]` | systemctl stop, then report remaining profile holders; force explicitly removes them with a job-failure warning. |
| `restart PROFILE [--config PATH]` | systemctl restart, preserving running containers for adoption. |
| `mode PROFILE [0\|1\|2] [--delay-seconds N] [--config PATH]` | Read or switch local admission mode without restarting the loop. `0` pauses new admission, `1` is normal, `2` delays fallback admission (default 60 seconds). |
| `create FLAGS [--config PATH]` | Exclusive creation described below. CLI uses `/etc/systemd/system`; `-systemd-unit-dir` is a TUI launch option only. |
| `migrate [--config PATH]` | Explicit access-mode/GitHub metadata migration; prints per-file results, retains existing `.bak` behavior, returns failure if any migration fails. Does not change credentials or add watch lists/unit drop-ins. |
| `version` | Shared build-info revision (`-dirty` if modified), VCS time and Go version; unavailable VCS fields show `unknown`. |
| `sync --profile PATH [--config PATH]` or `sync --config PATH` | Existing explicit migration and organization runner-group synchronization flow; no profile selects all configured profiles. |

Exit codes: **0** success, **1** operation/output failure, **2** argument/invalid-create-input error. There is no CLI cleanup command. `status`, TUI startup/refresh and profile lookup do not migrate or write configuration; metadata writes are explicit `migrate`, `sync` or `create`. Legacy service discovery remains available for display; legacy creation is removed.

Both binaries share the version formatter:

```bash
gha-runner-tui version
gha-ephemeral-loop --version
```

### Temporary admission mode (CLI)

Run these commands **on the server whose profile you want to control**. Set the temporary high-performance server's matching profiles to `1`, and the permanent fallback server's matching profiles to `2`. Runner labels, repository/group access, OS and architecture must still match the workflows.

```bash
gha-runner-tui mode PROFILE                          # inspect current mode
sudo gha-runner-tui mode PROFILE 0                   # pause new jobs
sudo gha-runner-tui mode PROFILE 1                   # normal admission
sudo gha-runner-tui mode PROFILE 2                   # fallback after 60s
sudo gha-runner-tui mode PROFILE 2 --delay-seconds 90  # custom grace period
gha-runner-tui status --json
```

The CLI atomically writes `<loop.state_file>.admission.json`, independently of profile YAML and job state. An absent override means mode `1`. The override remains until changed, including across service restarts and host reboots when the state directory is persistent; use mode `1` to restore normal admission. `--delay-seconds` is accepted only while setting mode `2`, in the range 1–86400. Each command affects one local profile; repeat for other matching profiles.

The **updated `gha-ephemeral-loop` binary is required as well as the updated CLI**; older queue loops and legacy organization loops do not read this override. Switching mode on an updated loop needs no service restart. A sleeping loop observes the change on its next queue-poll cycle (normally 30 seconds, with configured jitter); an existing runner is checked on the normal 15-second inspect cadence. A launch already in progress can finish starting, then follows the same drain protection.

Mode `2` starts a runner only after a matching job has remained queued across successful scans for the grace period, measured from this loop's first observation. It re-reads the queue each cycle, drops vanished job IDs and gives new job IDs their own timer. Mode changes, service restarts, queue-read failures and launch attempts reset these in-memory timers. The actual start can be later than the grace period because of polling or an occupied host slot. No host lock is held just to wait out the grace period. This offers a preferred host a head start; it does **not** provide strict cross-host priority or bind a particular GitHub job to a runner.

Mode `0` prevents new runner launches. Existing busy ephemeral runners finish their current job. Existing idle runners are withdrawn using the existing GitHub busy-safe DELETE check: only a successful DELETE permits force cleanup; HTTP 422 retains a runner that accepted a job. Lookup/deletion failures retain the container. Switching an existing runner from normal to delayed mode also drains it when idle, so it cannot bypass the grace period; a new runner that already passed that grace period stays eligible. No mode command calls `systemctl`, Docker kill or Docker remove. Switching takes effect as the loop observes it, so an assignment racing with the switch is allowed to finish.

`status` includes a `MODE` column (`2/60s`, for example), and its JSON includes `admission: {"mode": 2, "delay_seconds": 60}` for each readable policy. Invalid or unreadable overrides block new admission and appear in profile errors rather than silently reverting to normal. Healthy idle states include `paused` and `waiting-delay`; a paused profile can still show `running-job` while draining.

### Host concurrency

All loops on a host read `/etc/gha-runner-tui/host-policy.json` under the shared launch lock. Missing policy retains the original one-job gate. To permit four jobs across all repositories, provision `{"max_jobs":4}` (valid range 1–64); malformed/unreadable policy blocks admission. The capacity check and Docker launch remain serialized. With capacity above one, a successful launch releases the lock before waiting; existing managed containers continue to reserve capacity. Unknown container states block new launches. Startup adoption also releases the gate before monitoring an existing job. Ambiguous launch failures retain the gate until resolved.

Optional `min_root_free_bytes` adds a free-space floor for `/`, for example `{"max_jobs":4,"min_root_free_bytes":5368709120}` sets a 5 GiB floor. Below it, or on a filesystem query error, new launches back off and retry. Existing-container adoption and monitoring continue. The default zero disables this check. This is a pre-launch check, not a reservation for future job writes; bound caches with a separate filesystem.

Optional `docker_root_dir` with `min_docker_free_bytes` and `docker_reclaim_below_bytes` guards the Docker data root the same way, for example `{"max_jobs":4,"docker_root_dir":"/data/gha-runner/docker","min_docker_free_bytes":4294967296,"docker_reclaim_below_bytes":8589934592}`. Before a launch, under the host lock, free space below the reclaim threshold triggers one host-wide prune at most every 10 minutes: unused build cache, dangling images and anonymous volumes no container references. Tagged images, named volumes and containers are never removed. If free space is still below the floor, the loop shows `waiting-host`, records the reason in `last_error`, and polls at its normal interval so another host can take the job.

A profile can additionally reserve Docker data-root space for its own runner with `docker.disk_reserve_bytes`, for example `docker: {container_name_prefix: gha-heavy, disk_reserve_bytes: 4294967296}`. The value is capped at 1 PiB and defaults to zero, in which case the profile declares no reservation and its container carries no label. When set, the container carries an `io.gha-runner-tui.disk-reserve` label with the value in bytes. Before each launch the gate lists running containers with that label and inspects their writable-layer size; the outstanding reservation is the sum, per container, of the unused part of its reservation (`max(0, reserve - size_rw)`). The launch floor becomes the greater of the host `min_docker_free_bytes` and this profile's reserve, and the launch is refused when `free - outstanding` is below it, with `free`, `outstanding` and `need` in the `last_error` reason. The same reservation query runs for zero-reserve profiles, so another profile's declared reservation still counts against the host floor. This check requires `docker_root_dir`; without it the Docker gate is skipped entirely, as before. These values are launch-time reservations, not enforced container limits or filesystem quotas.

Each profile remains a single worker. For concurrent jobs from the same repository, create distinct profiles with the same repository/labels/image and unique profile names, service names, runner/container prefixes, state files, and log directories. Every worker shares the host policy. Manage their admission policies together: if extra workers' admission files are symlinked to the first worker's file (the Instabox deployment does this), `mode` on the parent controls the group. Do not set mode individually on those linked workers, as atomic replacement would detach that link. CPU limits are independent of task count: use one parent cgroup for all runner containers and their Docker children, rather than multiplying a per-container CPU limit.

Lowering capacity does not terminate existing jobs; it only blocks new admission until occupancy falls below the new limit. Existing processes must be upgraded before enabling concurrency. A host may retain idle registered runners while GitHub queue observations converge; these consume capacity too. This is a capacity limit, not a fairness guarantee.

### Create

| Flag | Requirement/default |
|---|---|
| `--scope repository\|organization` | Default repository. |
| `--owner`, `--repo`, `--name` | Required for repository creation. |
| `--org`, `--environment` | Required for organization creation; name defaults to the derived organization/environment name. |
| `--labels`, `--image`, `--cpus`, `--memory` | Required; comma-separated labels, **no CPU/memory defaults**. |
| `--watch` | Comma-separated `owner/repo`; organization CLI creation requires it. Repository defaults to its own repository. |
| `--docker-access` | Global default; explicit `rootless` or unsafe `host-socket` opt-in. |
| `--service`, `--container-prefix` | Default `gha-<name>.service`, `gha-<name>`. |
| `--github-env-file` | Independent profile credential-file path and unit `EnvironmentFile`; default global env-file path. |
| `--no-start` | Still write YAML/unit, daemon-reload and enable; skip only the final start. |

For example, after the user has provisioned credentials and the rootless socket:

```bash
sudo gha-runner-tui create --scope repository \
  --owner example --repo model-router --name model-router \
  --labels self-hosted,linux,x64,docker --image ghcr.io/example/actions-runner:latest \
  --cpus 2 --memory 4g --github-env-file /etc/gha-runner-tui/model-router.env --no-start
gha-runner-tui status --json
gha-runner-tui start model-router
```

Replace example owner/image/labels/resources with confirmed workflow values; the example image is a placeholder, not a supplied runner image. Organization creation also needs `--scope organization --org <org> --environment <environment> --watch <owner/repo,...>` instead of repository identity flags.

The shared CLI/TUI manager writes new YAML/unit files **in the current process using O_EXCL only**. Existing files, including dangling symlinks, are refused. Permission failure is an operation failure with a **run create with sudo** hint: no transparent privileged install/copy fallback for these files. Directory creation and other commands retain their existing `sudo -n` fallback; that does not make file creation privileged. If a later step fails, already-created files are retained and identified for explicit resolution before retry; do not assume rollback or overwrite.

## TUI controls and log safety

- Dashboard: `r` read-only refresh, `enter` detail, `c` create, `g` sync group, `s` start, `x` graceful stop, `R` graceful restart.
- Detail: `j` journal, `d` Docker logs, `g` sync group, `D` delete group (active/busy runners block deletion), `k` destructive kill, `C` cleanup exited/dead containers matching the prefix, `b`/`esc` back.
- Logs: `r` refresh, `f` toggle 2-second refresh, `b`/`esc` back.
- Create: `tab`/`shift+tab` fields, `ctrl+s` create and enable/start, **`q` is ordinary input**, `ctrl+c` quit, `esc` cancel. Fill CPU/memory explicitly; Ephemeral must be true. Organization creation has **no watch field**: manually add YAML `runner.watch_repositories` before the loop can start successfully (an already started loop can remain `failed`; restart after correcting YAML). Prefer CLI `--watch --no-start` when preparing an organization profile.
- Dashboard/detail/logs: `q` or `ctrl+c` quit. `?` opens help on dashboard/detail; help `q`/`esc` returns to dashboard.

Docker logs are read only after reliably decoding `Config.Env` for necessary `RUNNER_TOKEN`/`REG_TOKEN` values; those values are replaced with `[REDACTED]`. This does not depend on successful full-state/timestamp inspect parsing. If Env cannot be obtained/decoded, raw logs/error bodies are refused; the TUI clears log content and shows the safe error. Normal and failing log reads/persistence follow this gate. Journal is independent of Docker's Env gate. This is **not** a promise to redact unknown workflow secrets: workflows must not print secrets, and raw inspect/environment content must not be exposed.

## Deployment and rollback: external documents only

The single authoritative operational flow is **§8, “上线与回滚（tokyo-s2）”** of the external spec [2026-10-07-ssh-cli-single-job-design.md](../../../docs/tui/superpowers/specs/2026-10-07-ssh-cli-single-job-design.md). Exact command supplements and handoff gates are **Task 10, “上线验收/回滚短交接与门禁（非代码、不执行上线）”** in the external plan [2026-10-07-ssh-cli-single-job.md](../../../docs/tui/superpowers/plans/2026-10-07-ssh-cli-single-job.md).

These links resolve from this worktree's `src` into the **outer workspace's separate docs repository**, not files shipped in this source repository. Their outer-workspace paths are `docs/tui/superpowers/specs/2026-10-07-ssh-cli-single-job-design.md` and `docs/tui/superpowers/plans/2026-10-07-ssh-cli-single-job.md`; there is no standalone runbook.

Every remote step needs separate explicit authorization; the user installs tokens without the operator reading them. **External contract unverified / real Linux systemd validation pending:** organization and repository busy-runner DELETE must return 422 while the test job completes; a 204 response blocks rollout and returns to design. Real startup/stop/restart signal races must be verified separately. Drain old unlabeled runners before enabling the new gate. On rollback, stop new loops and wait for `SLOT: free` (including no `created` holder) before starting old units, which do not recognize the new managed gate. Local green gates alone authorize neither rollout nor rollback.
