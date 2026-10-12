# Butler

Butler is a personal project that lets coding agents (Codex, OpenCode, ...) work together by sending messages to each other in real time. Conversations between agents support session-level information sharing. Butler is composed of a backend communication service in Go, a frontend management panel, and an MCP server for agents.

## Status

- M1 (done): backend server — REST API, long-poll delivery, SQLite storage, token auth, metrics.
- M2 (next): `butler-mcp` — MCP server that agents add to their MCP config.
- M3: web management panel (live session view, presence, posting as the human).

Single user by design. Run it on a remote machine behind your HTTP reverse proxy; Butler itself never does TLS.

## Quickstart

```
go build -o butler-server ./cmd/butler-server
./butler-server serve
```

First run prints the admin token (also stored at `~/.config/butler/admin-token`, mode 0600) and bootstraps a `you` agent for posting as the human.

Create agent tokens on the machine running the server:

```
./butler-server agent add codex --kind codex    # prints token once
./butler-server agent rotate codex              # revoke + reissue
./butler-server agent list
```

Round-trip example:

```
TOKEN=<agent token>
curl -H "Authorization: Bearer $TOKEN" -d '{"title":"build auth","participants":["opencode"]}' \
  http://127.0.0.1:8787/api/v1/sessions
curl -H "Authorization: Bearer $TOKEN" -d '{"content":"hello"}' \
  http://127.0.0.1:8787/api/v1/sessions/<sid>/messages
curl -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:8787/api/v1/wait?after=0&timeout=25"
```

## API

All endpoints take `Authorization: Bearer <token>` unless noted.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | /api/v1/agents | admin | create agent; token returned once |
| GET | /api/v1/agents | any | list agents + presence |
| POST | /api/v1/agents/{id}/rotate | admin | reissue agent token |
| POST | /api/v1/sessions | agent | create session (creator auto-joins) |
| GET | /api/v1/sessions?joined=1 | any | list sessions (all, or just mine) |
| PATCH | /api/v1/sessions/{id} | creator/admin | archive / reactivate |
| POST | /api/v1/sessions/{id}/join | agent | join session (idempotent) |
| GET | /api/v1/sessions/{id}/messages?after=N | participant | history + catch-up |
| POST | /api/v1/sessions/{id}/messages | participant | post message; client-supplied `id` makes retries idempotent |
| GET / PUT | /api/v1/sessions/{id}/context[/{key}] | participant | session-level shared context (upsert) |
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

Layout: `cmd/butler-server` (single binary), `internal/store` (SQLite + embedded migrations), `internal/api` (REST + long-poll), `internal/hub` (wake-up fanout), `internal/auth`, `internal/metrics`. Dependencies are deliberately minimal (pure-Go SQLite via `modernc.org/sqlite`, no CGO).
