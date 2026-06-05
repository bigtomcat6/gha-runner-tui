# gha-runner-tui

A terminal dashboard and lifecycle manager for GitHub Actions self-hosted runners running inside Docker and supervised by systemd loop services.

## What It Does

- Loads runner profiles from YAML files.
- Reads systemd, loop-state JSON, Docker, and GitHub runner status for each profile.
- Resolves a combined health state that understands ephemeral runner deregistration.
- Shows dashboard, detail, and log views in a Bubble Tea TUI.
- Starts, stops, restarts loop services.
- Kills the current runner container.
- Cleans exited containers that match the profile prefix.
- Creates a new profile YAML plus a systemd unit, then reloads/enables/starts the service.
- Defaults new profiles to a shared rootless Docker daemon instead of the host root Docker socket.
- Migrates older profiles to explicit Docker access metadata when the previous access mode can be identified without changing runtime behavior.
- Provides `gha-ephemeral-loop`, a companion binary that writes the loop state JSON contract consumed by the TUI.

## Why "Runner Gone" Can Be Healthy

Ephemeral GitHub Actions runners deregister after a single job. When the loop is sleeping and there is no current runner container, a GitHub state of `gone` is treated as healthy instead of failed.

## Project Layout

```text
src/
  cmd/
    gha-runner-tui/
    gha-ephemeral-loop/
internal/
  app/
  command/
  config/
  docker/
  github/
  loop/
  state/
  systemd/
  tui/
templates/
```

## Build

```bash
cd src
go build ./cmd/gha-runner-tui
go build ./cmd/gha-ephemeral-loop
```

## Run

```bash
cd src
export GITHUB_TOKEN=ghp_xxx
./gha-runner-tui -config /etc/gha-runner-tui/config.yaml
```

The create flow also needs a writable systemd unit directory. By default it writes to `/etc/systemd/system`, but you can override that at launch time:

```bash
cd src
./gha-runner-tui \
  -config /etc/gha-runner-tui/config.yaml \
  -systemd-unit-dir /etc/systemd/system
```

## Docker Access Defaults

New profiles default to `rootless` Docker access. The create flow resolves the configured host socket and then writes the final runtime wiring directly into the profile YAML:

- `docker.access_mode: rootless`
- `docker.volumes: ["<host rootless socket>:/var/run/docker.sock"]`
- `docker.env.DOCKER_HOST=unix:///var/run/docker.sock`

That keeps the runner container compatible with common Docker CLI expectations without exposing the host root Docker daemon by default.

Example `config.yaml` block:

```yaml
docker:
  default_access_mode: rootless
  rootless_socket_path: /run/user/1001/docker.sock
  auto_detect_rootless_socket: true
  allow_host_socket_opt_in: true
  host_socket_path: /var/run/docker.sock
```

If `rootless_socket_path` is empty and `auto_detect_rootless_socket=true`, the create flow will try a narrow detection path:

- `DOCKER_HOST=unix://...`
- `/run/user/*/docker.sock`

If it cannot find exactly one usable rootless socket, profile creation fails with an actionable error instead of silently falling back to the host Docker socket.

## Unsafe Host Socket Opt-In

If you need direct host Docker daemon access for a specific workflow, you can explicitly opt a profile into `host-socket` mode from the create flow.

This is intentionally unsafe:

- the runner container can control the host Docker daemon
- workflows can escape the intended container-to-host boundary

That path is only allowed when `docker.allow_host_socket_opt_in=true`.

## Compatibility Boundaries

The rootless-default path is intended to support:

- `docker build`
- `docker pull` / `push`
- most `docker run`
- most `docker compose`
- `buildx`
- GitHub Actions workloads that only need a normal Docker daemon API

It does not aim to preserve full compatibility for:

- `--privileged`
- `--network host`
- host-root bind mount assumptions
- workflows that depend on the real host root Docker daemon

## Loop Process

The generated systemd unit starts:

```bash
/usr/local/bin/gha-ephemeral-loop --config /etc/gha-runner-tui/profiles/<profile>.yaml
```

`gha-ephemeral-loop`:

- cleans exited containers for the profile prefix
- requests a GitHub registration token
- starts a Docker runner container
- waits for the container to finish
- persists Docker logs to the profile log directory
- writes loop state transitions to the configured state JSON file

## Expected Config Paths

Defaults are aligned with the MVP spec:

```text
/etc/gha-runner-tui/config.yaml
/etc/gha-runner-tui/profiles/*.yaml
/var/lib/gha-runner-tui/state/*.json
/var/log/gha-runner-tui/<profile>/
```

## TUI Keys

Dashboard:

- `r` refresh
- `enter` open detail
- `c` create profile
- `s` start service
- `x` stop service
- `R` restart service

Detail:

- `j` systemd logs
- `d` Docker logs
- `k` kill current container
- `C` cleanup exited containers

Global:

- `?` help
- `q` quit
- `esc` back
