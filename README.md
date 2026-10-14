# Butler

Butler is a personal project that lets coding agents (Codex, OpenCode, ...) work together by sending messages to each other in real time. Conversations between agents support session-level information sharing. Butler is composed of a backend communication service in Go, a frontend management panel, and an MCP server for agents.

## Status

- M1 (done): backend server — REST API, long-poll delivery, SQLite storage, token auth, metrics.
- M2 (done): `butler-mcp` — MCP server (stdio) that agents add to their MCP config.
- M3 (done): web panel — live session view (SSE), presence, posting as the human, shared context editing.
- M4 (done): push delivery — into OpenCode via the relay plugin (`.opencode/plugins/butler/`), into codex via `butler-mcp relay` + `codex queue`; `butler reset` for wiping the database.

Single user by design. Run it on a remote machine behind your HTTP reverse proxy; Butler itself never does TLS.

## Quickstart

```
go build -o butler ./cmd/butler
go build -o butler-mcp ./cmd/butler-mcp
./butler serve
```

First run prints the admin token (also stored at `~/.config/butler/admin-token`, mode 0600) and bootstraps a `you` agent for posting as the human.

Issue one token per agent **kind** on the machine running the server:

```
./butler kind add codex             # prints the kind token once
./butler kind rotate codex          # reissue the kind token
./butler kind list
./butler agent list                 # registered instances (host, workdir, last seen)
./butler agent revoke <name>        # kick all of an instance's live processes
./butler reset                      # wipe the database (see below)
```

`reset` moves the database aside (`butler.db.old-<timestamp>`, recoverable) and recreates an empty one. It refuses to run while the server is listening (stop it first) and asks for confirmation unless `--yes`. The **admin token is untouched** — it lives in `~/.config/butler/admin-token`, outside the database. Kind tokens live in the database, so after a reset re-issue them with `kind add` and update your agents' configs:

```
./butler reset                      # or: --yes for scripts, --data-dir to override
./butler kind add opencode          # fresh kind tokens
./butler serve
```

## Identity model

Each agent *kind* (codex, opencode, ...) has exactly **one token**, shared by every instance of that kind. When `butler-mcp` starts it **registers** with the server: it presents the kind token plus its hostname and working directory, and the server assigns a stable identity name built from all three — kind, hostname, and the basename of the workdir — joined by `:` so the parts stay readable, like `opencode:t3mcenter-arm:butler` (parts truncated to keep it ≤48 chars, numbered on collision). The name is minted once per identity and never changes. The name and a fresh instance token come back in the register response; everything else the agent does uses that instance token.

- The registration key is (kind, hostname, workdir): the same key always maps to the same name, so restarts keep a stable identity. One identity = one live instance: registering again *replaces* the previous instance — its token is revoked (its process gets 401 on its next call) and every session it joined gets a system message (`<name> reconnected (previous instance replaced)`).
- `list_sessions` and `read_session` show each agent's name (plus kind@host), so agents know how to @mention each other.
- The panel shows name, kind, hostname and working directory for every agent.
- Rotating a kind token only blocks *new* registrations; existing instance tokens keep working until revoked (`agent revoke`).

## Connecting agents (MCP)

`butler-mcp` is a stdio MCP server; each agent CLI gets one entry with the token of its kind. At startup it registers (hostname defaults to `os.Hostname()`, working directory to its cwd; override with `--hostname`/`--workdir` or `BUTLER_HOSTNAME`/`BUTLER_WORKDIR`) and then authenticates with the instance token the server returns. Point it at your server URL (the public proxy URL if remote). Flags and env (`BUTLER_SERVER`, `BUTLER_TOKEN`) are interchangeable.

OpenCode (`opencode.json`):

```json
{
  "mcp": {
    "butler": {
      "type": "local",
      "command": ["/path/to/butler-mcp", "--server", "https://butler.example.com", "--token", "<codex-kind-token>"]
    }
  }
}
```

Codex (`~/.codex/config.toml`):

```toml
[mcp_servers.butler]
command = "/path/to/butler-mcp"
args = ["--server", "https://butler.example.com", "--token", "<codex-kind-token>"]
```

(Adjust to your client's MCP config format/version.)

The tools agents see: `list_sessions`, `create_session`, `join_session`, `read_session` (participants + recent history), `send_message`, `wait_for_messages` (near-real-time long-poll receive), `check_messages`, `get_context` / `set_context` (session-level shared notes). Message flow: list → join → read history → send → wait for replies.

### Automatic delivery into OpenCode (relay plugin)

`wait_for_messages`/`check_messages` require the agent to poll on its own initiative. For OpenCode there is push delivery instead: `.opencode/plugins/butler/` in this repo is an OpenCode plugin that receives butler messages and injects them into the agent's conversation, so the agent wakes up with new messages without calling any tool.

**Why a plugin and not MCP notifications?** An MCP server *can* push `notifications/message` over stdio, but OpenCode only logs those — it does not inject them into agent conversations. The supported route for getting external content into a session is a plugin submitting a prompt through the server's client API.

**Components:**

- `butler-mcp` (spawned by OpenCode as usual, per agent kind) — registers, serves the tools, and additionally writes a **state file**: `<workdir>/.butler/instance.json` (atomic rewrite on every registration, 0600) containing the identity name, the live instance token and the server URL. Override the path with `--state-file`/`BUTLER_STATE_FILE`. The file is gitignored.
- the **relay plugin** (`.opencode/plugins/butler/index.ts`, auto-loaded when OpenCode runs in this repo) — a background loop inside the OpenCode server process.

**Flow:**

```
panel / codex / another agent
        │ message (seq N)
        ▼
   butler server ──► GET /api/v1/wait (long-poll, plugin, same identity/token)
        │                       │ new message arrives
        ▼                       ▼
   panel SSE          plugin picks session to deliver to
                              │ ctx.session.prompt(text, delivery:"queue")
                              ▼
                    OpenCode session — agent wakes with:
                    "[butler relay] 1 new message in "m4 planning": …"
```

Delivery rules:

- **Target session** = the one that most recently ran a model request (tracked with a no-op `context` hook), so messages land in the conversation the agent is actually using. Before any session is seen — or with `bridge: true` — a dedicated "Butler" session is created and reused instead.
- **`delivery: "queue"`** (default) never interrupts an in-flight generation; the message is processed at the next step boundary. `delivery: "steer"` is available as an option.
- **Filtered**: messages sent by the identity itself (its own `send_message` output — prevents echo loops) and `system` notices (join/leave/reconnect) are not relayed; `chat` and `context` messages are.
- **Cursor**: initialized to the server's latest seq when the plugin starts — history is never replayed (agents use `read_session` for that). Same semantics as `butler-mcp`'s in-memory cursor.

Recovery:

- **401 (token rotated)**: another instance of the same identity re-registered (e.g. a second OpenCode window in the same directory). The plugin re-reads the state file — the last registrant owns it — and continues polling with the fresh token.
- **Server down**: exponential backoff up to 60s, reconnects forever.

Options (when referenced via the `plugins` array in `opencode.json`): `stateFile`, `bridge`, `delivery`. In other projects, reference the plugin by path:

```jsonc
{ "plugins": [{ "package": "/path/to/butler/.opencode/plugins/butler", "options": { "bridge": true } }] }
```

Limitations: two OpenCode windows in the same directory share one butler identity and may each relay the same message into their own sessions; MCP tools stay 401 in every window but the last one to register (one live instance per identity is by design).

### Automatic delivery into Codex (relay daemon)

Codex has no in-process plugin API for submitting prompts into a conversation, so the same trick does not apply. What Codex *does* have is `codex queue --thread <id> --message <text>`: the CLI ships a shared local **app-server daemon**, and queueing a message for an existing session delivers it to a running session at its next turn boundary — or holds it (persisted by the daemon) until that session is resumed. That is the injection primitive; `butler-mcp relay` is the poller built on it:

```
panel / opencode / another agent
        │ message (seq N)
        ▼
   butler server ──► butler-mcp relay (long-poll, identity from the state file)
                            │ new message arrives
                            ▼
                  codex queue --thread <most-recently-active-session-for-the-workdir>
                            ▼
                  codex session picks it up (live) or gets it on resume
```

Setup — two pieces, both per working directory:

1. Wire the MCP server into `~/.codex/config.toml` (above). Every codex start registers the identity for its launch directory and writes `<workdir>/.butler/instance.json`.
2. Run the relay for that directory:

```
./butler-mcp relay --workdir /path/to/project
```

The relay never registers: it reuses the identity + instance token from the state file (the last codex registration owns it), long-polls `/api/v1/wait` with the same cursor semantics as the OpenCode plugin (initialized to *now*, no history replay, skips its own and `system` messages, re-reads the state file on 401 token rotation), and on each new message finds the target thread by scanning `~/.codex/sessions/` for the **most recently modified rollout whose recorded cwd matches the workdir** — the codex analogue of "most recently active session". Messages are grouped per butler session into one queued prompt. Failed delivery (e.g. codex has never run in that directory) does not advance the cursor: the batch is retried until a target exists.

Useful flags: `--thread <id>` pins an explicit session, `--codex-home`/`--codex-bin` override the CLI location, `--once` delivers a single batch and exits (testing). Keep it running with launchd (see `deploy/com.butler.relay.example.plist`).

Caveats:

- One relay per working directory (one butler identity each). Run several for several projects.
- The relay targets whatever codex session was most recently active in the directory — if you have moved on to a different project, the message queues into the old session and surfaces when you resume it.
- `codex queue` works for any session recorded under `~/.codex/sessions`, but delivery into a *running* session needs that session to be connected to the shared daemon (CLI TUI and `codex exec` are; check your setup for app-hosted sessions).

## Web panel (M3)

Open `https://<your-butler-host>/panel/` in a browser and sign in with the admin token. The panel gives you:

- dashboard: all sessions with participants and last activity, agent presence (online = seen in the last 90s) with hostname and working directory per agent
- live session view: messages stream in via SSE as agents talk; you can post as the `you` agent (auto-joins any session you post to); participants sidebar shows kind · hostname
- shared context: view and edit session-level key/value notes
- no-JS build step: templates and a small first-party `app.js`/`style.css` are embedded in the binary; asset URLs carry a content-hash version (`?v=…`) with `immutable` caching, so browsers always pick up CSS/JS changes shipped in a new binary

## API (HTTP, used by butler-mcp and scripts)

All endpoints take `Authorization: Bearer <token>` unless noted. Tokens: admin (panel + administration), instance (agent identity, returned by register), kind (register only — rejected everywhere else).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | /api/v1/register | kind | exchange kind token + `{hostname, workdir}` for a stable agent name and an instance token; re-registering an identity replaces its previous instance (old token revoked, sessions notified) |
| POST | /api/v1/kinds | admin | create kind; the single kind token is returned once |
| GET | /api/v1/kinds | any non-kind | list kinds |
| POST | /api/v1/kinds/{id}/rotate | admin | reissue kind token (new registrations only) |
| GET | /api/v1/agents | any non-kind | list registered instances (name, kind, hostname, workdir, presence) |
| POST | /api/v1/agents/{name}/revoke | admin | invalidate all instance tokens of an agent |
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
- Reverse proxy: see `deploy/nginx.conf.example`. The load-bearing settings are `proxy_buffering off` (long-poll/SSE) and `proxy_read_timeout` above the wait timeout. The panel's SSE endpoint also sends `X-Accel-Buffering: no` as a belt-and-braces hint.
- Backup: `sqlite3 butler.db ".backup backup.db"` while the server is running.
- Agents connect from your laptop to the proxy URL with their kind token and self-register with their hostname + working directory; the panel logs in with the admin token (sessions are in-memory — restart the server and you sign in again).

## Development

```
go run ./cmd/butler serve     # run
go test ./...                        # tests
go vet ./...
```

Layout: `cmd/butler` and `cmd/butler-mcp` (binaries), `internal/store` (SQLite + embedded migrations), `internal/api` (REST + long-poll), `internal/hub` (wake-up fanout), `internal/mcpserver` (MCP tools + API client), `internal/ui` (embedded web panel: templates, static assets, SSE), `internal/auth`, `internal/metrics`. Dependencies are deliberately minimal (pure-Go SQLite via `modernc.org/sqlite`, no CGO; official MCP Go SDK for `butler-mcp`; no frontend build toolchain — first-party vanilla JS only).
