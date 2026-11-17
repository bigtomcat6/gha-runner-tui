# instabox-controller

Minimal external coordinator for the loop's opt-in session deadline guard. It
polls the Instabox API for the configured machines, keeps each machine's
absolute `session-deadline.json` aligned with that machine's real run budget,
and can optionally wake one idle-suspended standby.

It is **not a read-only status tool**. Deadline delivery is a write to each
machine, either over SSH (the default) or, opt-in per machine, through the
documented synchronous `POST /v1/machines/{id}/exec` route with that machine's
own API key. Enabling wake sends `POST /v1/machines/{id}/wake`. Both writes and
wake are disabled in the configuration below until you explicitly enable them.

The intended deployment host is a **constantly online Linux machine** (the
`tokyo-s2` SSH host in the current setup). It can be built and tested on macOS,
but a laptop that suspends cannot poll reliably. With `api-exec` it needs
outbound HTTPS and the account keys, but no SSH identity or route to the
machines; the default `ssh` transport still needs the matching SSH access. This
repository does not install a systemd unit, launchd job or any remote service.

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
base URL and no raw key field: each machine may name the environment variable
that holds the key for **its** Instabox account, and the API host is fixed to
`https://api.instabox.sh`. A machine without `apiKeyEnv` keeps the legacy
single-account behavior and reads `INSTABOX_API_KEY`.

A machine's `deliveryTransport` is `"ssh"` (the default) or `"api-exec"`. With
`"api-exec"`, `sshHost`, `sshUser`, `sshPort` and `sshIdentityFile` are not
required and are ignored: the controller sends a bounded `bash -lc` command
through `POST /v1/machines/{id}/exec` using that machine's own account client.
Omit `deliveryTransport` to keep the SSH behavior and then set `sshHost` and
`sshUser` (optionally `sshPort` and `sshIdentityFile`).

```json
{
  "pollIntervalSeconds": 30,
  "stateDir": "/var/lib/instabox-controller",
  "enableRemoteWrite": false,
  "enableWake": false,
  "machines": [
    {
      "machineId": "REPLACE_WITH_MACHINE_ID",
      "apiKeyEnv": "INSTABOX_S1_API_KEY",
      "deliveryTransport": "api-exec",
      "deadlinePath": "/data/gha-runner-tui/session-deadline.json"
    },
    {
      "machineId": "REPLACE_WITH_OTHER_MACHINE_ID",
      "apiKeyEnv": "INSTABOX_S2_API_KEY",
      "deliveryTransport": "api-exec",
      "deadlinePath": "/data/gha-runner-tui/session-deadline.json"
    }
  ]
}
```

The matching `EnvironmentFile` (for example
`/etc/instabox-controller/credentials.env`, created by you with mode `0600`)
holds the values; the variable names are the only key-related text in the JSON
file:

```dotenv
INSTABOX_S1_API_KEY=REPLACE_WITH_S1_ACCOUNT_KEY
INSTABOX_S2_API_KEY=REPLACE_WITH_S2_ACCOUNT_KEY
```

- Each key is used only for the machines that name its variable. A machine's
  GET, wake, exec and post-write check always use that machine's own account
  key. Two machines may reference the same variable when they really share one
  account; the legacy global `INSTABOX_API_KEY` is still supported for a
  single-account config.
- Key scope is per account: polling needs `machines:read`; enabling wake or
  `api-exec` needs `machines:write` (which includes read). The key is sent only
  in the `X-API-Key` HTTP header to `https://api.instabox.sh`; it is never part of
  an exec command and never reaches the machine. One key must not be reused
  across two different Instabox accounts; give each account its own variable.
- A `403 missing_scope` on an exec is reported as a bounded, non-sensitive
  failure. It is never retried with another account's key and never falls back
  to SSH.
- Machine IDs and SSH details are site-specific; none are built in. `machineId`
  must be the machine's real `m_...` ID (names are not accepted, so the
  controller can compare identities from responses). Two accounts may use
  identical display names; IDs are still the identity.
- `deliveryTransport` accepts only `"ssh"` or `"api-exec"`; missing means
  `"ssh"`, and `null`, an empty string or any other value is rejected at
  startup. With `api-exec`, the remote command runs as the machine's login user
  (`dev`, HOME `/data/home`) and the machine must be `running`; a non-running
  machine is a plain `409 machine_not_running` and is never treated as success.
- `apiKeyEnv` is an environment variable **name**. A value that is not a valid
  name is rejected at startup, and a missing or blank value fails closed
  before any network, SSH or state activity. An explicit `apiKeyEnv` never
  falls back to `INSTABOX_API_KEY` or another machine's key.
- `deadlinePath` must be the same **absolute** file configured as
  `loop.session_deadline_file` on that machine, and that file must be updated
  by this controller only. For `api-exec` it must be writable by the login user
  (`dev`); the controller never uses `sudo` and never defaults to a root-only
  path. Its parent directory must already exist and be writable by `dev`,
  because the atomic replacement creates its random temporary file there; the
  controller does not create directories. A permission failure is reported as
  an unconfirmed delivery. For the planned deployment, use a dedicated
  user-writable directory such as `/data/gha-runner-tui/session-deadline.json`.
- `sshHost` is a literal hostname or IPv4 address. An `~/.ssh/config` alias is
  allowed because it matches the same character set, and `ssh` resolves it.
- `sshIdentityFile` is optional; omit it to use the agent/default key.
- `stateDir` holds the single-instance lock and the small deduplication state.
  It is created with mode `0700`; state is written atomically with mode `0600`.
  It is shared by all configured accounts; keys, names and quotas stay
  independent per account, and the state keys remain machine IDs.

`pollIntervalSeconds` defaults to 30 and accepts 10–3600.

## Enabling writes and wake

1. Put each account's key in the variable named by its machines'
   `apiKeyEnv` (for example a systemd `EnvironmentFile` that you create
   yourself), or keep `INSTABOX_API_KEY` for a legacy single-account config.
   Keys are never read from the JSON config file and never logged.
2. Scope per account: polling needs `machines:read`; enabling wake or
   `deliveryTransport: "api-exec"` needs `machines:write` (which includes
   read). Instabox has no wake-only scope. A different account needs its own
   key; never point two accounts at one key.
3. First run a preflight round; this makes GETs but no SSH, no exec and no
   wake. With split accounts, each key performs only the GETs for its own
   machines.
   Let systemd read the `EnvironmentFile` you created above; never source or
   `eval` it in a shell (its values are data, and keys may contain `$` or
   backticks that must stay literal):

   ```bash
   sudo systemd-run --wait --pipe --collect \
     -p EnvironmentFile=/etc/instabox-controller/credentials.env \
     -- "$(pwd)/instabox-controller" --config "$(pwd)/controller.json" --once --dry-run
   ```

   For a legacy single-account config, keep one key inline:

   ```bash
   INSTABOX_API_KEY=... ./instabox-controller --config controller.json --once --dry-run
   ```

   The config already defaults to `enableRemoteWrite: false` and
   `enableWake: false`, so `--dry-run` is an extra guard, not the only one.
4. Set `enableRemoteWrite: true` to let the controller replace each machine's
   deadline file. Set `enableWake: true` separately to allow one standby wake.
   `--dry-run` on the command line always overrides both to off.

Run continuously without `--once` for the normal loop. Any configured machine
whose key variable is missing, blank or not a valid name exits with status 2
before any state directory, lock, network or SSH activity. A second instance on
the same `stateDir` fails closed while the first holds the lock. Run exactly one
controller on one always-on host: the lock is local and cannot coordinate
copies on separate hosts.

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
confirmed, further polls do not deliver again; the controller keeps polling
with `GET` only and does not run a periodic exec keepalive. A write is an
atomic remote replace followed by an exact readback on the same transport
(one SSH connection, or one exec result), then a second GET checks that machine
identity, run and deadline did not change. An unknown write is read first
before any replacement; an unconfirmed result is never counted as ready.

With `api-exec`, the request blocks until the command exits and the result must
be a complete documented `ExecResult` with `exitCode` 0 and `timedOut`,
`truncated`, `invalidUtf8` all false. A non-zero exit, a server-side timeout, a
truncated or binary readback, an incomplete document, an HTTP timeout or an
HTTP error all count as **unknown**, never as success; a body that arrives
after an HTTP 200 as the documented error envelope is reported as that error.
The request carries only `cmd`, `timeoutSeconds` and `noHistory`; paths are
shell-quoted with the same random-temporary-file replace script used over SSH.

SSH access (when `deliveryTransport` is `ssh`) is batch-mode with
connect/session timeouts and normal host-key checking (never disabled). Paths,
hosts, users and ports are validated and shell-quoted; an invalid value refuses
SSH. Both transports re-check the target parameters before building a command.

### Official contract

- `POST /v1/machines/{id}/exec`: <https://instabox.sh/docs/api-reference/connections/run-a-command-in-the-sandbox-and-return-its-output>
- `GET /v1/machines/{id}`: <https://instabox.sh/docs/api-reference/machines/get-a-sandbox>
- Authentication and scopes: <https://instabox.sh/docs/api-reference/introduction>

The exec route runs `bash -lc` as the machine's login user (`dev`, HOME
`/data/home`), needs the machine `running`, and returns the synchronous
`ExecResult` in the same response. It is not idempotent, so the controller
never retries a write blindly; it reads the file back first.

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
- Delivering the deadline over SSH or exec is itself activity and can postpone
  the machine's idle auto-pause. It does not extend a platform max-run budget.
- Reusing an old run's deadline file is conservative when the platform starts
  every run at 120 minutes and the controller never delivers beyond the first
  two hours. It cannot cover the platform shortening a run, an earlier
  unrelated pause/stop, host clock anomalies, or an operator editing the file.
- A wake decision relies on `autoSuspendedAt`, `cooldownUntil` and machine
  state; anything the API cannot distinguish is skipped, which can leave no
  standby awake.
- If remote writes are disabled, the controller only reports that a deadline
  would be delivered; it never claims budget-ready.
