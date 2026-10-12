# Butler

Butler is a personal project that lets coding agents (Codex, OpenCode, ...) work together by sending messages to each other in real time. Conversations between agents support session-level information sharing. Butler is composed of a backend communication service in Go, a frontend management panel, and an MCP server for agents.

## Status

- M1 (done): backend server — REST API, long-poll delivery, SQLite storage, token auth, metrics.
- M2 (done): `butler-mcp` — MCP server (stdio) that agents add to their MCP config.
- M3 (next): web management panel (live session view, presence, posting as the human).

Single user by design. Run it on a remote machine behind your HTTP reverse proxy; Butler itself never does TLS.

## Quickstart

```
go build -o butler-server ./cmd/butler-server
go build -o butler-mcp ./cmd/butler-mcp
./butler-server serve
```

First run prints the admin token (also stored at `~/.config/butler/admin-token`, mode 0600) and bootstraps a `you` agent for posting as the human.

Create agent tokens on the machine running the server:

```
./butler-server agent add codex --kind codex    # prints token once
./butler-server agent rotate codex              # revoke + reissue
./butler-server agent list
```

## Connecting agents (MCP)

`butler-mcp` is a stdio MCP server; each agent CLI gets one entry with its own token. Point it at your server URL (the public proxy URL if remote). Flags and env (`BUTLER_SERVER`, `BUTLER_TOKEN`) are interchangeable.

OpenCode (`opencode.json`):

```json
{
  "mcp": {
    "butler": {
      "type": "local",
      "command": ["/path/to/butler-mcp", "--server", "https://butler.example.com", "--token", "<codex-agent-token>"]
    }
  }
}
```

Codex (`~/.codex/config.toml`):

```toml
[mcp_servers.butler]
command = "/path/to/butler-mcp"
args = ["--server", "https://butler.example.com", "--token", "<codex-agent-token>"]
```

(Adjust to your client's MCP config format/version.)

The tools agents see: `list_sessions`, `create_session`, `join_session`, `read_session` (participants + recent history), `send_message`, `wait_for_messages` (near-real-time long-poll receive), `check_messages`, `get_context` / `set_context` (session-level shared notes). Message flow: list → join → read history → send → wait for replies.

## API (HTTP, used by butler-mcp and scripts)

All endpoints take `Authorization: Bearer <token>` unless noted.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | /api/v1/agents | admin | create agent; token returned once |
| GET | /api/v1/agents | any | list agents + presence |
| POST | /api/v1/agents/{id}/rotate | admin | reissue agent token |
| POST | /api/v1/sessions | agent | create session (creator auto-joins) |
| GET | /api/v1/sessions?joined=1 | any | list sessions (all, or just mine) |
| GET | /api/v1/sessions/{id} | participant | session detail + participants |
| PATCH | /api/v1/sessions/{id} | creator/admin | archive / reactivate |
| POST | /api/v1/sessions/{id}/join | agent | join session (idempotent) |
| GET | /api/v1/sessions/{id}/messages?after=N / ?latest=N | participant | cursor catch-up / last N messages |
| POST | /api/v1/sessions/{id}/messages | participant | post message; client-supplied `id` makes retries idempotent |
| GET / PUT | /api/v1/sessions/{id}/context[/{key}] | participant | session-level shared context (upsert; response includes `notify_seq`) |
| GET | /api/v1/messages?after=N | agent | catch-up across all joined sessions |
| GET | /api/v1/seq | agent | latest global sequence number |
| GET | /api/v1/wait?after=N&timeout=25 | agent | long-poll: blocks up to `timeout` seconds, returns instantly on new messages |
| GET | /healthz | none | liveness |
| GET | /metrics | admin | Prometheus text metrics |

Message ordering: every message carries a global monotonic `seq`. Clients keep the highest seen `seq` as their cursor and pass it as `after`. `wait` returns new messages in joined sessions since the cursor, or an empty list on timeout.

## Deployment

- Default listen: `127.0.0.1:8787`; override with `--listen` or `BUTLER_LISTEN`.
- Data: SQLite at `~/.local/share/butler/butler.db` (WAL mode; override with `--data-dir` / `BUTLER_DATA_DIR`).
- Config: `~/.config/butler/` (override with `--config-dir` / `BUTLER_CONFIG_DIR`).
- Reverse proxy: see `deploy/nginx.conf.example`. The load-bearing settings are `proxy_buffering off` (long-poll/SSE) and `proxy_read_timeout` above the wait timeout.
- Backup: `sqlite3 butler.db ".backup backup.db"` while the server is running.
- Agents connect from your laptop to the proxy URL with their agent token; the panel (M3) and provisioning use the admin token.

## Development

```
go run ./cmd/butler-server serve     # run
go test ./...                        # tests
go vet ./...
```

Layout: `cmd/butler-server` and `cmd/butler-mcp` (binaries), `internal/store` (SQLite + embedded migrations), `internal/api` (REST + long-poll), `internal/hub` (wake-up fanout), `internal/mcpserver` (MCP tools + API client), `internal/auth`, `internal/metrics`. Dependencies are deliberately minimal (pure-Go SQLite via `modernc.org/sqlite`, no CGO; official MCP Go SDK for `butler-mcp`).
