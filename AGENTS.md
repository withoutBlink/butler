# AGENTS.md

## Project

Butler: real-time messaging layer so coding agents (Codex, OpenCode) can talk to each other and share session-level context. Design: Go backend server (M1, done), MCP server for agents (M2, next), web panel (M3). See README for the API contract.

## Commands

- Run server: `go run ./cmd/butler-server serve` (or build `./cmd/butler-server`)
- Test: `go test ./...`
- Vet: `go vet ./...`
- CLI: `butler-server agent add|rotate|list` — flags work before or after the positional id

## Layout

- `cmd/butler-server/` — single binary; subcommands `serve`, `agent`, `version`
- `internal/store/` — SQLite via `modernc.org/sqlite` (pure Go, no CGO); migrations embedded in `internal/store/migrations/`, applied by version number
- `internal/api/` — REST + long-poll handlers, auth middleware, Go 1.22+ pattern routing
- `internal/hub/` — in-process wake-up fanout used by the long-poll `wait` endpoint
- `internal/auth`, `internal/ids`, `internal/metrics`
- `deploy/nginx.conf.example` — reverse proxy config for remote deployment

## Conventions

- Keep dependencies minimal: stdlib first; new deps need a strong reason.
- Message ordering: `messages.global_seq` (SQLite AUTOINCREMENT rowid) is the universal cursor; clients long-poll `GET /api/v1/wait?after=<seq>`.
- Store uses MaxOpenConns(1); writes are serialized. WAL + busy_timeout let the CLI subcommands touch the DB while the server runs.
- Tokens are SHA-256 hashes at rest; admin token lives at `~/.config/butler/admin-token` (0600); agent tokens are only printed at creation/rotation.
- Message inserts are idempotent via client-supplied `id` (`ON CONFLICT DO NOTHING`).

## Gotchas

- go.mod requires Go 1.26.1.
- Schema changes: add a new `internal/store/migrations/NNN_*.sql` — never edit an applied migration.
- Server binds 127.0.0.1:8787 by default; remote deploys sit behind an HTTP reverse proxy (buffering off, read timeout > wait timeout — see deploy/nginx.conf.example).
- Long-poll `wait` handler re-checks the DB after subscribing to avoid the missed-wakeup race — preserve that pattern when touching it.
- Go's `flag` stops at the first positional arg; `cmd/butler-server` uses a rotation loop so `agent add <id> --kind x` works. Keep that in mind for any new CLI subcommand.
