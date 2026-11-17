# instabox-controller

Minimal external coordinator for the loop's opt-in session deadline guard. It
polls the Instabox API for the configured machines, keeps each machine's
absolute `session-deadline.json` aligned with that machine's real run budget,
and can optionally wake one idle-suspended standby.

It is **not a read-only status tool**. Deadline delivery is a write to each
machine over SSH, and enabling wake sends `POST /v1/machines/{id}/wake`. Both
are disabled in the configuration below until you explicitly enable them.

The intended deployment host is a **constantly online Linux machine** (the
`tokyo-s2` SSH host in the current setup). It can be built and tested on macOS,
but a laptop that suspends cannot poll reliably. This repository does not
install a systemd unit, launchd job or any remote service.

## Build and test (from `src`)

```bash
go build ./cmd/instabox-controller
go test ./internal/instaboxcontroller ./cmd/instabox-controller -count=1
```

## What it reports

- Whether each machine answered a fresh `GET /v1/machines/{id}` and is
  `running` with `runtime.ready` true.
- Whether that run is **budget-ready**: the delivered deadline for *this* run
  is confirmed, a post-write GET still shows the same run and deadline, and the
  effective remaining time is inside the 100–120 minute window.

It does not use the GitHub API and does not claim a runner is online. A
budget-ready machine only means the local guard would admit new work at that
moment.

## Configuration

The controller reads one JSON file (`--config`). There is deliberately no API
base URL and no key field: the key comes only from `INSTABOX_API_KEY`, and the
API host is fixed to `https://api.instabox.sh`.

```json
{
  "pollIntervalSeconds": 30,
  "stateDir": "/var/lib/instabox-controller",
  "enableRemoteWrite": false,
  "enableWake": false,
  "machines": [
    {
      "machineId": "REPLACE_WITH_MACHINE_ID",
      "deadlinePath": "/var/lib/gha-runner-tui/state/session-deadline.json",
      "sshHost": "REPLACE_WITH_SSH_HOST",
      "sshUser": "REPLACE_WITH_SSH_USER",
      "sshPort": 22,
      "sshIdentityFile": "/home/deploy/.ssh/id_ed25519"
    },
    {
      "machineId": "REPLACE_WITH_OTHER_MACHINE_ID",
      "deadlinePath": "/var/lib/gha-runner-tui/state/session-deadline.json",
      "sshHost": "REPLACE_WITH_OTHER_SSH_HOST",
      "sshUser": "REPLACE_WITH_SSH_USER",
      "sshPort": 22
    }
  ]
}
```

- Machine IDs and SSH details are site-specific; none are built in. `machineId`
  must be the machine's real `m_...` ID (names are not accepted, so the
  controller can compare identities from responses).
- `deadlinePath` must be the same **absolute** file configured as
  `loop.session_deadline_file` on that machine, and that file must be updated
  by this controller only.
- `sshHost` is a literal hostname or IPv4 address. An `~/.ssh/config` alias is
  allowed because it matches the same character set, and `ssh` resolves it.
- `sshIdentityFile` is optional; omit it to use the agent/default key.
- `stateDir` holds the single-instance lock and the small deduplication state.
  It is created with mode `0700`; state is written atomically with mode `0600`.

`pollIntervalSeconds` defaults to 30 and accepts 10–3600.

## Enabling writes and wake

1. Put the key in `INSTABOX_API_KEY` in the controller's environment
   (for example a systemd `EnvironmentFile` that you create yourself). It is
   never read from the config file and never logged.
2. Scope: polling needs `machines:read`. Enabling wake needs `machines:write`
   (which includes read). Instabox has no wake-only scope.
3. First run a preflight round; this makes GETs but no SSH and no wake:

   ```bash
   INSTABOX_API_KEY=... ./instabox-controller --config controller.json --once --dry-run
   ```

   The config already defaults to `enableRemoteWrite: false` and
   `enableWake: false`, so `--dry-run` is an extra guard, not the only one.
4. Set `enableRemoteWrite: true` to let the controller replace each machine's
   deadline file. Set `enableWake: true` separately to allow one standby wake.
   `--dry-run` on the command line always overrides both to off.

Run continuously without `--once` for the normal loop. Missing
`INSTABOX_API_KEY` exits with status 2 before any config-dependent network or
SSH activity. A second instance on the same `stateDir` fails closed while the
first holds the lock. Run exactly one controller on one always-on host: the
lock is local and cannot coordinate copies on separate hosts.

## Deadline policy

For the current run, the effective deadline is

```
runStartedAt + min(maxRunMinutes, 120 minutes)
```

so platform max-run growth never moves the delivered deadline past the first
two hours of the run. The controller does not trust a missing/invalid
`runStartedAt` or `maxRunMinutes`: such a machine is not budget-ready and is
never guessed at. A delivered deadline is considered ready only when the fresh
GET leaves at least 100 minutes (the guard's admission floor) and at most 120
minutes.

Delivery happens for a new run, a changed effective deadline, or an earlier
write whose result was not confirmed. Once the same run and deadline are
confirmed, further polls do not SSH again. A write is an atomic remote replace
followed by an exact readback on the same SSH connection, then a second GET
checks that machine identity, run and deadline did not change. An unknown
write is read first before any replacement; an unconfirmed result is never
counted as ready.

SSH access is batch-mode with connect/session timeouts and normal host-key
checking (never disabled). Paths, hosts, users and ports are validated and
shell-quoted; an invalid value refuses SSH.

## Wake policy (opt-in)

With `enableWake: true`, at most one machine is woken per poll round, and only
when either no machine is budget-ready, or exactly one is and it has 105
minutes or less remaining. A candidate must be explicitly recognizable as
**automatically idle-suspended**:

- `state` and `desiredState` are `suspended`;
- `autoSuspendedAt` is present and valid;
- `cooldownUntil` is absent or in the past, and `cooldownReason` is absent;
- `runtime.hostUnreachable` is false and no operation is active.

A manual stop or manual suspend, a cooldown, any running/transitioning/error
state, or an ambiguous document is skipped. A GET failure anywhere in the round
cancels waking entirely. Running-but-expired machines are never woken.

The controller records the attempt in local state *before* the POST. A
timeout is disambiguated with a GET but is never retried for the same suspend
cycle; neither is a refused wake. A restart is not a reason to resend. A new
`autoSuspendedAt` is a new cycle and may be attempted once.

Waking resumes a machine and can consume plan starts or incur charges. The
controller cannot force a pause, restart, or kill anything, and it cannot
override a platform cooldown.

## Real limits

This controller plus the loop guard is **best effort, not a strict 90-minute
fail-closed guarantee**:

- The guard checks locally before registration and can still race GitHub job
  assignment and runner deregistration.
- The controller is only as fresh as its last poll; a failed GET makes the
  machine not ready, but a queued job can be assigned between checks.
- Delivering the deadline over SSH is itself activity and can postpone the
  machine's idle auto-pause. It does not extend a platform max-run budget.
- Reusing an old run's deadline file is conservative when the platform starts
  every run at 120 minutes and the controller never delivers beyond the first
  two hours. It cannot cover the platform shortening a run, an earlier
  unrelated pause/stop, host clock anomalies, or an operator editing the file.
- A wake decision relies on `autoSuspendedAt`, `cooldownUntil` and machine
  state; anything the API cannot distinguish is skipped, which can leave no
  standby awake.
- If remote writes are disabled, the controller only reports that a deadline
  would be delivered; it never claims budget-ready.
