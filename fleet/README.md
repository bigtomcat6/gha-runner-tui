# gha-fleet

The Rust center CLI for fleet-managed `gha-runner-tui` profiles. It does not
parse profile YAML and does not keep a daemon; it invokes the node CLI over
SSH and relies on the node for validation, locking and writes.

## Build and test

```bash
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test
cargo build --release
```

The release binary is `target/release/gha-fleet`.

## Expected-state directory

`--dir` defaults to `$GHA_FLEET_DIR` and then `/etc/gha-fleet`:

```
fleet.toml
hosts/<host>/profiles/*.yaml
```

Each profile file is the node's original profile text and must carry a
top-level `managed_by: fleet` line. Every `.yaml` entry must be a regular file;
symlinks and other special files are refused before any SSH, so a prune cannot
silently drop a profile from the bundle. The center builds the node bundle from
these files: sorted by file name, documents separated by a single `---` line,
each document ending in a newline, at most 1 MiB.

```toml
state_dir = "/var/lib/gha-fleet"

[hosts.s1]
ssh = "instabox-s1"
cli = "/data/gha-runner/tools/<sha>/bin/gha-runner-tui"
config = "/etc/gha-runner-tui/config-live-20261117.yaml"
timeout_seconds = 60
instabox_machine_id = "m_..."
instabox_api_key_env = "INSTABOX_S1_API_KEY"   # optional sleep gate
```

Validation rejects non-absolute `cli`/`config`, characters outside
`[A-Za-z0-9._/-]`, host names outside `[a-z0-9-]`, SSH targets that could look
like options, unsafe environment variable names, and unknown TOML keys. SSH is
always called as `ssh -o BatchMode=yes -o ConnectTimeout=10 -- <target> ...`
and every remote argument is single-quoted, so the target cannot become an
option and profile values cannot become shell syntax.

## Commands

`--host` is repeatable; omitting it or passing `all` selects every configured
host. Runtime and adopt `--match` values are limited to
`[A-Za-z0-9._*?\[\]-]` and must not start with `-`.

- `status [--json] [--cached]` — concurrent `status --config C --json
  --no-github`. The JSON form passes the node's status object through as
  `"status"`. `--cached` reads `<state_dir>/snapshots/<host>.json` written by
  `serve` (T4); no snapshot is reported as such, with a stale marker after
  three 300-second periods.
- `plan [--prune]` — dry-run `apply` per host; prints actions and the opaque
  node `plan_id`. Hosts without expected-state files are skipped. An empty or
  missing directory plus `--prune` refuses the whole run because an empty
  bundle would delete every fleet profile on the node.
- `apply [--prune] [--yes]` — previews every host, prints all plans, asks once
  for confirmation, then executes only hosts whose preview was accepted and
  had changes, passing the exact preview `plan_id` and reusing the same
  in-memory bundle and flags. Without a terminal and without `--yes` it exits
  2 before executing.
- `adopt --match GLOB [--owner fleet|local] [--yes]` — same preview/execute
  flow for ownership-only changes. Previews are `adopt --dry-run`; execution
  uses the preview plan id. It never triggers a restart.
- `import [--force]` — `export --owner fleet --json`. Every selected export
  must have `complete=true`; otherwise nothing is written. Names are validated
  before use as file names, duplicates (including names that differ only by
  case) and non-fleet ownership are refused, and existing different files are
  refused unless `--force`. An existing local file whose name differs only by
  case from an exported name also refuses the import. All new contents are
  staged with exclusive-create temporary files and renamed only after the whole
  run preflights. Local-only files are reported and never deleted.
- `mode --match GLOB 0|1|2 [--delay-seconds N]`,
  `start|stop|restart --match GLOB` — forwarded to the node; the center never
  offers `--force`.

## Results and exit codes

Host categories are `ok`, `asleep(<state>)`, `unreachable`, `unknown`, `error`
for reads, and `applied`, `rejected`, `partial`, `unknown`, `asleep`,
`unreachable` (plus `error`) for writes.

- 0 — all succeeded, or there was nothing to change. `status` treats `asleep`
  as success.
- 1 — at least one host did not execute: rejected/preflight conflict, asleep,
  unreachable, or a local/node error.
- 2 — argument error, or non-terminal stdin without `--yes`.
- 3 — at least one host is `partial` or `unknown`.

Classification follows the node contract strictly:

- `unreachable` only when ssh could not be started locally, which proves no
  remote command was dispatched. Every SSH exit 255 is `unknown`: ssh shares
  its stderr with the remote command and also reports a remote command that
  itself exited 255, so no stderr text or stdout can prove dispatch did not
  happen. Valid-looking JSON is ignored when the ssh status is 255.
- timeout (ssh killed), a stdout/stderr pipe not closed by the deadline even
  after the child exited, child death by signal, invalid/truncated JSON, or an
  unexpected node reply is `unknown`.
- `unknown` is never retried, because it does not mean "nothing ran". The
  message suggests the read-only follow-up: `plan` after `apply`, a fresh
  `adopt` preview after `adopt`, `status` after runtime commands.

Incomplete export is a valid JSON report with `complete=false` and node exit 1;
`import` refuses it. Attempted runtime failures are per-target JSON with node
exit 3 and are reported as `partial`. Apply/adopt pre-execution rejections are
`phase=plan`, `complete=false`, node exit 1, and the center reports
`rejected`.

## Sleep gate

If `instabox_machine_id` is configured, `GET
https://api.instabox.sh/v1/machines/{id}` with `X-API-Key` is called before
each SSH (15 second timeout). A state other than `running` yields
`asleep(<state>)` and no SSH. A missing/empty key environment variable is a
host error and no SSH. Hosts without a machine id are not checked; one note is
printed on stderr. In debug/test builds the API base can be overridden with
`GHA_FLEET_INSTABOX_API_BASE` (HTTP is accepted for local mock servers);
release builds always use `https://api.instabox.sh`.

## Partner contract fixtures

T2's hand-written samples live in `tests/fixtures/<scenario>.json` and use the
same names as the node's Go goldens. Once T1 has published
`src/cmd/gha-runner-tui/testdata/contract/`, run the cross-check explicitly:

```bash
GOLDEN_DIR=../src/cmd/gha-runner-tui/testdata/contract \
  cargo test --test golden_contract
```

The test decodes each Go golden with the center's contract types, checks the
scenario's expected phase/exit code (an optional `<scenario>.exit` sidecar is
honored), compares object schemas with our fixture, and drives the same golden
through fake-ssh center flows. Without `GOLDEN_DIR` it uses the default path
when present and otherwise skips.
