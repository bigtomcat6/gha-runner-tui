# gha-runner-tui

A terminal dashboard and SSH-friendly CLI for ephemeral GitHub Actions Docker runners, with one systemd loop per profile and a host-wide single-job gate.

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

Each unit runs `gha-ephemeral-loop --config <profile-YAML>`. It first adopts its own retained managed containers and cleans its own terminal leftovers, then polls watched repositories. It reads one page of 100 runs for each of `queued` and `in_progress`, and one page of jobs per run with `filter=latest&per_page=100`; only queued jobs with matching nonempty labels are candidates. There is no pagination or fairness guarantee.

With candidates, the loop takes nonblocking `flock(LOCK_EX|LOCK_NB)` on `/run/gha-runner-tui/host.lock` (directory 0755, file 0600), rechecks all managed containers, then registers and launches at most one runner. Only loops take this lock; its path is not configurable through CLI/YAML. Managed labels are:

```text
io.gha-runner-tui.managed=true
io.gha-runner-tui.profile=<profile.name>
io.gha-runner-tui.runner=<runner name>
```

`created`, `running`, `restarting` and `paused` occupy the slot; only `exited`, `dead` and `removing` do not. Unknown states and query errors fail closed. Lock contention or another holder means `waiting-host`; without a current container this is healthy, not a failure. `failed` remains unhealthy. A sleeping ephemeral profile with no container and a deregistered (`gone`) runner can also be healthy. State JSON is diagnostic; Docker supplies live occupancy. CLI `status` shows `SLOT: free`, the holder(s), `WARNING` for multiple holders, or `unknown` on query failure; the TUI does not add another scheduling entry point or slot control.

While waiting, normal inspect cadence is 15 seconds. A retained `created` container gets 60 seconds from creation before a **non-force** removal attempt; failed/unknown removal retains the slot and rechecks rather than assuming it is free. A runner never seen in GitHub is never automatically force-deleted: it can block the host indefinitely, requiring an explicit operator decision.

Idle cleanup requires authoritative evidence: DELETE HTTP 204 authorizes force removal, HTTP 422 keeps waiting, other errors retain the container; a previously seen runner missing on two consecutive checks also authorizes removal. After idle cleanup, candidate job IDs are skipped **in memory for 30 minutes**. Restart loses that skip list; it is not persistent scheduling.

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
