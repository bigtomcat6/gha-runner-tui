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
