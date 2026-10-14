# AGENTS.md

## Project

Butler: real-time messaging layer so coding agents (Codex, OpenCode) can talk to each other and share session-level context. Design: Go backend server (M1), MCP server for agents (M2), web panel (M3) — all done. See README for the API contract, MCP onboarding, and panel usage.

## Commands

- Run server: `go run ./cmd/butler serve` (or build `./cmd/butler`)
- Run MCP server: `go run ./cmd/butler-mcp --server URL --token TOK` (stdio)
- Run codex relay: `go run ./cmd/butler-mcp relay --workdir DIR` (long-poll butler → `codex queue`; see `cmd/butler-mcp/relay.go`)
- Test: `go test ./...`
- Vet: `go vet ./...`
- CLI: `butler kind add|rotate|list` (one token per agent kind), `butler agent list|revoke` (registered instances), `butler reset [--yes]` (move the DB aside and recreate it empty; refuses while the server is listening; admin token file is untouched) — flags work before or after the positional id

## Layout

- `cmd/butler/` — server binary; subcommands `serve`, `agent`, `version`
- `cmd/butler-mcp/` — stdio MCP server for agents; flags/env `--server`/`BUTLER_SERVER`, `--token`/`BUTLER_TOKEN`, `--state-file`/`BUTLER_STATE_FILE` (writes `<workdir>/.butler/instance.json` after registration); `relay` subcommand (`cmd/butler-mcp/relay.go`) = the codex-side push-delivery daemon: long-polls as the state-file identity and injects via `codex queue`
- `internal/store/` — SQLite via `modernc.org/sqlite` (pure Go, no CGO); migrations embedded in `internal/store/migrations/`, applied by version number
- `internal/api/` — REST + long-poll handlers, auth middleware, Go 1.22+ pattern routing
- `internal/hub/` — in-process wake-up fanout; per-agent subscribers (long-poll `wait`) and per-session subscribers (panel SSE)
- `internal/mcpserver/` — MCP tool handlers + typed HTTP client for the API
- `internal/ui/` — embedded web panel: html/template + first-party vanilla JS (no htmx, no build step), SSE stream, admin-token login, posts as the `you` agent
- `.opencode/plugins/butler/` — OpenCode relay plugin: long-polls `/api/v1/wait` as the identity from `.butler/instance.json` and injects new messages into the most-recently-active OpenCode session as prompts (push delivery, no agent polling)
- `internal/auth`, `internal/ids`, `internal/metrics`
- `deploy/nginx.conf.example` — reverse proxy config for remote deployment

## Conventions

- Identity model: one token per agent **kind** (`kinds` table, CLI `kind add`). `butler-mcp` registers at startup (`POST /api/v1/register` with kind token + hostname + workdir) and gets back a server-generated stable name plus an instance token. Registration key = (kind, hostname, workdir); same key → same name. Names are minted once from kind + hostname + workdir basename, ":"-joined (`opencode:t3mcenter-arm:butler`, caps: kind 16 / host 20 / dir 20 / total 48, `-2` suffix on collision); existing identities keep their minted names. One live instance per identity: re-registering revokes the previous instance's token (`ReplaceAgentToken`, last login wins) and announces the replacement (system message) in every session the identity joined. `agent revoke` clears the tokens of an identity outright.
- Keep dependencies minimal: stdlib first; new deps need a strong reason.
- Message ordering: `messages.global_seq` (SQLite AUTOINCREMENT rowid) is the universal cursor; clients long-poll `GET /api/v1/wait?after=<seq>`.
- After any message/context/join write, fan out to BOTH hub subscriber kinds: `Hub.NotifySession(sid)` and `Hub.Notify(participantIDs)` — see `api.notifySession` / `ui.fanout`.
- Store uses MaxOpenConns(1); writes are serialized. WAL + busy_timeout let the CLI subcommands touch the DB while the server runs.
- Tokens are SHA-256 hashes at rest; admin token lives at `~/.config/butler/admin-token` (0600); agent tokens are only printed at creation/rotation.
- Message inserts are idempotent via client-supplied `id` (`ON CONFLICT DO NOTHING`).
- Push delivery to OpenCode: `butler-mcp` writes `<workdir>/.butler/instance.json` (atomic temp+rename, 0600, agent + instance token + server URL) on every registration — last registrant owns the file; readers re-read it on 401. The relay plugin (`.opencode/plugins/butler/`) polls `/api/v1/wait` with that token, skips self + `system` messages, and relays the rest into the most-recently-active OpenCode session via `ctx.session.prompt(..., delivery: "queue")`, tracked with a no-op `ctx.session.hook("context")`. Cursor semantics match butler-mcp: initialized to the server's latest seq at startup, old messages are never replayed.
- Push delivery to codex: same state file, different consumer — `butler-mcp relay --workdir DIR` (in `cmd/butler-mcp/relay.go`) long-polls as the codex identity and injects each batch with `codex queue --thread <id> --message <text>`. Codex's shared app-server daemon persists queued messages (`~/.codex/queue_1.sqlite`): live sessions receive them at the next turn boundary, closed sessions on resume. Target thread = most recently modified rollout under `~/.codex/sessions/` whose first-record `payload.cwd` matches the workdir (`thread_id` on old rollouts, `id` on new ones). Delivery failure does NOT advance the cursor (batch retried until a thread exists). One relay process per workdir.
- Panel UI is html/template + first-party vanilla JS embedded via go:embed — deliberately no htmx/React/node toolchain. Keep it that way unless there's a strong reason.

## Gotchas

- go.mod requires Go 1.26.1.
- Schema changes: add a new `internal/store/migrations/NNN_*.sql` — never edit an applied migration. (Pre-release exception: `001_init.sql` was rewritten in place when the agent identity model changed; any pre-rewrite dev database must be deleted.)
- Server binds 127.0.0.1:8787 by default; remote deploys sit behind an HTTP reverse proxy (buffering off, read timeout > wait timeout — see deploy/nginx.conf.example).
- Long-poll `wait` handler re-checks the DB after subscribing to avoid the missed-wakeup race — preserve that pattern when touching it; the panel SSE `stream` handler uses the same pattern.
- Go's `flag` stops at the first positional arg; `cmd/butler` uses a rotation loop (`parseSubFlags`) so `kind add <id> --name x` works. Keep that in mind for any new CLI subcommand.
- `butler-mcp` speaks MCP on stdout — never log or print to stdout there (logs go to stderr).
- `butler-mcp` registers at startup with a kind token (hostname/workdir defaults: `os.Hostname()` / cwd) and swaps its client to the returned instance token BEFORE serving tools — `Client.SetToken` is not concurrency-safe, keep that ordering.
- `butler-mcp` keeps its message cursor in memory, initialized to the server's latest seq at first poll: restarts intentionally skip old messages; agents use `read_session` for history. `send_message`/`set_context` advance the cursor past their own messages to avoid self-echo.
- The relay plugin and the MCP tools are two consumers of one identity: if you run `butler-mcp` manually with the same (kind, hostname, workdir) to refresh `.butler/instance.json`, the running MCP's token is revoked (401) until the client restarts it — expected, not a bug.
- Two OpenCode windows in the same directory both load the relay plugin and may each inject the same message into their own sessions; MCP tools in all but the last-registered window stay 401 (one live instance per identity).
- `butler-mcp relay` targets the most recently active codex session in the workdir — if codex has never run there, delivery retries without advancing the cursor (by design, at-least-once). Don't run two relays for one workdir: both would queue every message (duplicates).
- `codex queue` accepts any thread id known to the daemon, but delivery into a *running* session requires that session to be connected to the shared app-server daemon (CLI TUI / `codex exec` are). Headless `codex exec` may fail with account/model errors unrelated to queueing (e.g. `gpt-6.1-sol` via ChatGPT login) — the relay itself only needs the `codex` binary on PATH.
- Panel fragment templates (`fragments.tmpl`) are rendered for both normal pages and SSE data — keep them valid single templates and remember SSE re-sends them as `data:` lines.
- Panel login sessions are in-memory (lost on restart by design); login cookie is scoped to /panel.
