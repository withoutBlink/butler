# AGENTS.md

## Project

Butler: real-time messaging layer so coding agents (Codex, OpenCode) can talk to each other and share session-level context. Design: Go backend server (M1), MCP server for agents (M2), web panel (M3) — all done. See README for the API contract, MCP onboarding, and panel usage.

## Commands

- Run server: `go run ./cmd/butler-server serve` (or build `./cmd/butler-server`)
- Run MCP server: `go run ./cmd/butler-mcp --server URL --token TOK` (stdio)
- Test: `go test ./...`
- Vet: `go vet ./...`
- CLI: `butler-server agent add|rotate|list` — flags work before or after the positional id

## Layout

- `cmd/butler-server/` — server binary; subcommands `serve`, `agent`, `version`
- `cmd/butler-mcp/` — stdio MCP server for agents; flags/env `--server`/`BUTLER_SERVER`, `--token`/`BUTLER_TOKEN`
- `internal/store/` — SQLite via `modernc.org/sqlite` (pure Go, no CGO); migrations embedded in `internal/store/migrations/`, applied by version number
- `internal/api/` — REST + long-poll handlers, auth middleware, Go 1.22+ pattern routing
- `internal/hub/` — in-process wake-up fanout; per-agent subscribers (long-poll `wait`) and per-session subscribers (panel SSE)
- `internal/mcpserver/` — MCP tool handlers + typed HTTP client for the API
- `internal/ui/` — embedded web panel: html/template + first-party vanilla JS (no htmx, no build step), SSE stream, admin-token login, posts as the `you` agent
- `internal/auth`, `internal/ids`, `internal/metrics`
- `deploy/nginx.conf.example` — reverse proxy config for remote deployment

## Conventions

- Keep dependencies minimal: stdlib first; new deps need a strong reason.
- Message ordering: `messages.global_seq` (SQLite AUTOINCREMENT rowid) is the universal cursor; clients long-poll `GET /api/v1/wait?after=<seq>`.
- After any message/context/join write, fan out to BOTH hub subscriber kinds: `Hub.NotifySession(sid)` and `Hub.Notify(participantIDs)` — see `api.notifySession` / `ui.fanout`.
- Store uses MaxOpenConns(1); writes are serialized. WAL + busy_timeout let the CLI subcommands touch the DB while the server runs.
- Tokens are SHA-256 hashes at rest; admin token lives at `~/.config/butler/admin-token` (0600); agent tokens are only printed at creation/rotation.
- Message inserts are idempotent via client-supplied `id` (`ON CONFLICT DO NOTHING`).
- Panel UI is html/template + first-party vanilla JS embedded via go:embed — deliberately no htmx/React/node toolchain. Keep it that way unless there's a strong reason.

## Gotchas

- go.mod requires Go 1.26.1.
- Schema changes: add a new `internal/store/migrations/NNN_*.sql` — never edit an applied migration.
- Server binds 127.0.0.1:8787 by default; remote deploys sit behind an HTTP reverse proxy (buffering off, read timeout > wait timeout — see deploy/nginx.conf.example).
- Long-poll `wait` handler re-checks the DB after subscribing to avoid the missed-wakeup race — preserve that pattern when touching it; the panel SSE `stream` handler uses the same pattern.
- Go's `flag` stops at the first positional arg; `cmd/butler-server` uses a rotation loop so `agent add <id> --kind x` works. Keep that in mind for any new CLI subcommand.
- `butler-mcp` speaks MCP on stdout — never log or print to stdout there (logs go to stderr).
- `butler-mcp` keeps its message cursor in memory, initialized to the server's latest seq at first poll: restarts intentionally skip old messages; agents use `read_session` for history. `send_message`/`set_context` advance the cursor past their own messages to avoid self-echo.
- Panel fragment templates (`fragments.tmpl`) are rendered for both normal pages and SSE data — keep them valid single templates and remember SSE re-sends them as `data:` lines.
- Panel login sessions are in-memory (lost on restart by design); login cookie is scoped to /panel.
