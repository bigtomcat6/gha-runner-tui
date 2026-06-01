# gha-runner-tui

The Go application now lives under [`src/`](./src).

## Repository Layout

- `src/`: the Go module, binaries, internal packages, and embedded templates
- `docs/`: product and MVP documentation in the outer workspace

## Common Commands

```bash
go -C src test ./...
go -C src build ./cmd/gha-runner-tui
go -C src build ./cmd/gha-ephemeral-loop
```

See [`src/README.md`](./src/README.md) for the application-level documentation.
