# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build              # build ./matrixmigrate with version ldflags
make release            # optimized build (-trimpath)
make test               # go test -v ./...
make deps               # go mod download && go mod tidy
make build-all          # cross-compile linux/darwin/windows

go build ./...          # fast compile check, no binary
go test ./internal/matrix/                          # one package
go test ./internal/matrix/ -run TestNormalizeMatrixMentions -v   # one test
```

**`/tmp` is mounted `noexec` on this machine**, so `go test` fails with
`fork/exec /tmp/go-build.../pkg.test: permission denied`. Prefix test runs with a writable
exec tmpdir:

```bash
GOTMPDIR=/Storage/.gotmp go test ./...
```

`gofmt -l .` reports ~27 pre-existing files (struct field alignment). That is the baseline —
do not bulk-reformat, it buries real diffs. Keep new code gofmt-clean.

## Architecture

A one-way Mattermost → Matrix Synapse migration tool. Two sides with very different access
models, joined by an ID mapping file.

**Mattermost side (read-only).** SSH in, read `/opt/mattermost/config/config.json` to
auto-discover database credentials (`internal/mattermost/config_reader.go`), then query
PostgreSQL directly through an SSH tunnel (`internal/ssh/tunnel.go` forwards a local port).
The user never configures DB credentials by hand. `internal/ssh/remote.go` is separate — it
executes commands / reads files over SSH rather than forwarding ports.

**Matrix side (write).** Synapse Admin API over HTTP, reached either directly or through a
second SSH tunnel. Authenticated by admin token, or username/password login that yields one.

**`internal/migration/orchestrator.go` is the single coordination point.** Both the CLI
(`internal/cli/`) and the TUI (`internal/tui/`) drive the same `Orchestrator` methods —
`ExportAssets`, `ImportAssets`, `ExportMemberships`, `ImportMemberships`, `ExportMessages`,
`ImportMessages`. New migration behaviour belongs on the orchestrator, not in a front end;
otherwise it exists in only one of the two modes.

### The six-step state machine

`internal/migration/state.go` defines six steps with hard dependencies enforced by
`CanRunStep`, persisted to `state.json` so a run is resumable:

```
export_assets ─┬→ import_assets ─┬→ export_memberships → import_memberships
               │                 │
               └→ export_messages┴→ import_messages
```

`import_messages` requires *both* `export_messages` and `import_assets` — it needs the room
and user mappings, not just the message dump.

`import_assets` is the linchpin: it writes `mappings/asset-mapping-<ts>.json`, the join table
of `mm_user_id → matrix_user_id`, `mm_team_id → matrix_space_id`,
`mm_channel_id → matrix_room_id` (`internal/migration/mapping.go`). Every later step reads it.
A step that can't resolve an ID through this mapping skips the item — this is the usual reason
imports silently drop content.

Artifacts under `data/` (paths from `DataConfig`):

| file | written by |
|---|---|
| `assets/mattermost-{assets,memberships,messages}-<ts>.json.gz` | export steps |
| `mappings/asset-mapping-<ts>.json` | `import_assets` |
| `mappings/message-mapping-<ts>.json` | `import_messages` |
| `assets/message-errors-<ts>.log` | `import_messages`, on failures |
| `state.json` | every step |

Timestamped files are resolved by newest-glob, not by exact name.

### Three separate Matrix identities

Which one is in play changes what the code can do — this is the most common source of
confusion:

1. **Admin token** — baseline. Creates users and rooms *as the admin*, so the admin ends up
   the room creator.
2. **Application Service (AS) token** — required for message import. Lets requests be made as
   another user via `?user_id=`, which is the only way to preserve original message senders
   and timestamps. Also required for `preserve_owner_and_alias` (real room creator) and for
   DM import (setting `m.direct` account data for both participants). See
   `CreateRoomAsUser` in `internal/matrix/client.go`.
3. **MAS (Matrix Authentication Service)** OAuth2 client credentials — needed when the
   homeserver delegates auth to MAS and the Synapse Admin user-creation API is unavailable.
   `internal/matrix/mas.go`.

Import steps degrade rather than fail when the AS token is absent (create as admin, then
invite and set power levels), so a "working" run can still produce wrong ownership. Check for
AS token presence before debugging ownership or timestamp bugs.

### Mattermost → Matrix model mapping

Team → Space, Channel → Room, Channel membership → Room membership. Channel types: `O` public,
`P` private, `G` group, `D` direct message. `D` channels are only touched when
`import_direct_messages` is enabled, and their two participants come from `creator_id` plus
the `senderID__receiverID` channel name (`Channel.DMParticipantIDs`).

## Conventions

**Config secrets are always indirect.** `config.yaml` stores the *name* of an environment
variable (`admin_token_env`, `password_env`, `client_secret_env`, `as_token_env`), never a
value. Accessors in `internal/config/config.go` resolve them via `os.Getenv`. Keep this
pattern for any new credential — no field should ever hold a literal secret.

**All user-facing strings go through `i18n.T(key)`**, with entries in *both*
`internal/i18n/locales/en.yaml` and `tr.yaml`. Log output (`internal/logger`) is English-only
and not translated.

**Rate limiting matters.** Synapse aggressively rate-limits; `internal/matrix/client.go` has a
built-in limiter with retry (`NewClientWithRateLimit`). Large imports are expected to be slow
and resumable rather than fast. Don't add API calls that bypass the client's request path.

**Idempotency is the design assumption.** Steps are re-run against existing state
(force-join treats an already-present user as success). New import logic should be safe to
replay.

**Line endings: LF only.** `.gitattributes` enforces `* text=auto eol=lf`. History contains
some CRLF-committed blobs, so a diff that looks enormous is often pure line-ending noise —
verify with `diff <(git show A:f | tr -d '\r') <(git show B:f | tr -d '\r')` before judging
scope.

## Tests

Test coverage is thin and targeted at pure functions — markdown/mention rendering
(`internal/matrix`), post classification (`internal/mattermost`), error categorisation
(`internal/migration`). There are no integration tests and no fixtures for the SSH, database,
or Matrix API paths; those are exercised manually against a live pair of servers via
`matrixmigrate test mattermost|matrix|all`.

Use `example.com` and placeholder usernames (`alice`, `bob_dev`) in tests. This repo is public
— real hostnames, usernames, and room IDs must not appear in code, tests, docs, or commit
messages.
