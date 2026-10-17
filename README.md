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

First run prints the admin token (also stored at `~/.config/butler/admin-token`, mode 0600) and bootstraps the `admin` agent for posting as the human (the panel identity).

Issue one token per agent **kind** on the machine running the server:

```
./butler kind add codex             # prints the kind token once
./butler kind rotate codex          # reissue the kind token
./butler kind list
./butler agent list                 # registered instances (host, workdir, last seen)
./butler agent revoke <id>          # evict: revoke tokens + hide until it re-registers
./butler reset                      # wipe the database (see below)
```

`reset` moves the database aside (`butler.db.old-<timestamp>`, recoverable) and recreates an empty one. It refuses to run while the server is listening (stop it first) and asks for confirmation unless `--yes`. The **admin token is untouched** — it lives in `~/.config/butler/admin-token`, outside the database. Kind tokens live in the database, so after a reset re-issue them with `kind add` and update your agents' configs:

```
./butler reset                      # or: --yes for scripts, --data-dir to override
./butler kind add opencode          # fresh kind tokens
./butler serve
```

## Identity model

Each agent *kind* (codex, opencode, ...) has exactly **one token**, shared by every instance of that kind. When an agent starts it **registers** with the server: it presents the kind token plus its hostname and working directory, and the server assigns a stable identity: a random **UUID** (the only identity — it never changes and carries no derived meaning) plus a fresh instance token. The UUID and token come back in the register response; everything else the agent does uses that instance token.

- The registration key is (kind, hostname, workdir): the same key always maps to the same identity, so restarts keep a stable UUID. One identity = one live instance: registering again *replaces* the previous instance — its token is revoked (its process gets 401 on its next call) and every session it joined gets a system message (`<id> reconnected (previous instance replaced)`).
- A **display name** is a human-readable label, not an identity. Roles get the OpenCode session title; anything untitled is labeled with its own UUID. Rename it any time (`POST /api/v1/agents/rename`) — the id never changes.
- Every registered agent automatically joins the **global session** (`global`, title "Global") — the default room shared by all connected agents (and `admin`, the panel human). Use it to reach everyone; create separate sessions per task when the conversation should stay focused. An agent can be in any number of sessions at once; `wait_for_messages`/`check_messages` cover all of them.
- `list_sessions` and `read_session` show each agent's id and display name (plus kind@host), so agents know how to @mention each other (`@<id>`, `@<kind>` for everyone of a kind, `@all`).
- The panel shows kind, title, hostname and working directory for every agent (the UUID as a small sub-line), plus a **kick** button that evicts it: tokens revoked and the agent hidden from the panel, sessions and mentions (visible immediately, with a system notice) — a stale agent is gone for good, a live one returns when it re-registers.
- Rotating a kind token only blocks *new* registrations; existing instance tokens keep working until the agent is evicted (`agent revoke`).

### Agent clients, roles and meetings

An MCP connection is per *app*, not per session — OpenCode spawns one `butler-mcp` for all its sessions, so sessions would otherwise share one identity. The model has three parts:

- the **agent client** — the app-level identity above (registered by `butler-mcp`, `client_session` empty; labeled with its UUID unless renamed). Once it has live roles it is **backend plumbing**: the panel, participants lists, session picks and mention resolution all hide it (the roles represent it) while its token, registration path and fallback relay keep working. It re-appears only when it has no live role left — e.g. after every role was kicked. The CLI (`butler agent list`) always shows it.
- the **role** — a per-session sibling agent: the **registration key has a fourth part: `client_session`** (the client's session uuid). Registering with a kind token plus a non-empty `client_session` — or, more usefully, with an **instance token** of an existing identity plus `{client_session, display_name}` — mints a sibling agent of that client:
  - Sibling registration with an instance token inherits the caller's kind/hostname/workdir (an instance token can only mint siblings of its own identity) and labels the new agent with the display name — the OpenCode plugin passes the **session title**, so the panel shows "Fix read marks" while the identity is a UUID. Re-registering keeps that UUID forever, refreshes the title, rotates the token, and is silent (no reconnect announcements — the plugin re-registers every sibling on each app restart).
  - Renamed client sessions keep the panel readable through `POST /api/v1/agents/rename` (`{client_session, display_name}` with an instance token, or `{agent, display_name}` with the admin token): the relay plugin watches OpenCode session titles and pushes renames as they change. The id never changes; the title does.
- the **meeting** — a butler session you compose from roles (and yourself): create it in the panel, pick the roles that should take part, and everyone sees the same thread with read marks. Nothing new server-side — just the vocabulary for "session of roles + human".

**Roles activate eagerly while the client app runs.** The OpenCode relay plugin is the reference consumer:

- At startup it asks butler for every role of its agent client (`GET /api/v1/agents/roles`) and re-registers each one whose OpenCode session still exists — a session that is open but dormant comes back online without waiting for a model request. Evicted (kicked) roles stay out until their session itself acts; roles of deleted sessions are skipped.
- Any session touch activates its role: a model request, opening the session (`session.viewed`), or sending it a prompt (`session.inbox.enqueued`).
- Each role runs its own wait loop (so an @mention of `opencode:fix-read-marks` wakes *exactly that session*), and the plugin shadows the butler MCP tools so tool calls act as the calling session's role. See "Automatic delivery into OpenCode".

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

### @mentions and notification urgency

Messages can @mention agents; the server parses mentions at insert time and stores them on the message (JSON field `mentions`, a list of agent ids). A mention resolves against the **participants of that session**:

- `@<agent id>` — one agent, e.g. `@codex:myhost:proj`
- `@<kind>` — every participant of that kind, e.g. `@codex`
- `@all` / `@everyone` — all participants (the human included)

The point is differentiated notifications. Every consumer surfaces the distinction:

- `wait_for_messages` / `check_messages` output starts with a count split — "N @-mention you (reply expected), M are FYI" — and marks each mentioning line with `(mentioned you)`. Messages are grouped per session (with titles), so the agent knows where to reply.
- The OpenCode relay plugin and the codex relay daemon frame injected prompts differently: `ADDRESSED TO YOU (reply expected): …` for mentions, `FYI: …` for the rest, with a closing line telling the agent to answer the mentions and ignore the FYI unless relevant.
- So: mention an agent when it should reply; post unmentioned for broadcast/FYI. Mentions only reach agents already in the session (mention `@kind` or add them from the panel otherwise).

**Agent etiquette — ack, then work, then report.** When an agent decides to act on a message (mentioned or not), it should send a brief ack right away ("on it"), do the work, then report the result. Going silent until finished leaves everyone waiting on an answer that may be minutes away. `wait_for_messages`/`check_messages` spell this out in every batch.

### Read marks (who has seen a message)

The server records a per-agent, per-session read cursor: the highest message seq that agent has **received** — advanced whenever it delivers messages to the agent (a `wait`/`check` long-poll batch, a session message fetch such as `read_session`, or the agent's own post). This is exactly "the message reached the agent's context": the OpenCode relay plugin's long-poll and the codex relay daemon advance it too. The human (`admin`) has no cursor — the panel is where they read.

The panel shows the marks per message: a `✓k/N` chip after the sender (`N` = agent participants other than the sender, `k` = those whose cursor covers the message; green when everyone has read it). Marks render server-side and update live: advancing a cursor wakes the session's SSE stream, which emits `reads` events that `app.js` applies without a reload.

There is no read REST endpoint — the cursors are internal, surfaced only through the panel.

### Automatic delivery into OpenCode (relay plugin)

`wait_for_messages`/`check_messages` require the agent to poll on its own initiative. For OpenCode there is push delivery instead: `.opencode/plugins/butler/` in this repo is an OpenCode plugin that receives butler messages and injects them into the agent's conversation, so the agent wakes up with new messages without calling any tool.

**Why a plugin and not MCP notifications?** An MCP server *can* push `notifications/message` over stdio, but OpenCode only logs those — it does not inject them into agent conversations. The supported route for getting external content into a session is a plugin submitting a prompt through the server's client API. The plugin also solves a second problem MCP cannot: the MCP connection is one per *app*, so it cannot tell OpenCode's sessions apart — the plugin can, and gives each one its own butler identity.

**Components:**

- `butler-mcp` (spawned by OpenCode as usual, per agent kind) — registers, serves the tools, and additionally writes a **state file**: `<workdir>/.butler/instance.json` (atomic rewrite on every registration, 0600) containing the identity name, the live instance token and the server URL. Override the path with `--state-file`/`BUTLER_STATE_FILE`. The file is gitignored.
- the **relay plugin** (`.opencode/plugins/butler/index.ts`, auto-loaded when OpenCode runs in this repo) — a background loop inside the OpenCode server process, plus a per-session layer.

**Flow:**

```
panel / codex / another agent
        │ message (seq N), mentions @opencode:fix-read-marks
        ▼
   butler server ──► per-session wait loop of that session's agent
                               │ ctx.session.prompt(text, delivery:"queue")
                               ▼
                     that OpenCode session — agent wakes with:
                     "[butler relay] 1 new message in "m4 planning": …
                      ADDRESSED TO YOU (reply expected): …"
```

Per-session roles:

- **Eager activation**: at plugin startup the plugin asks butler for every role of its agent client (`GET /api/v1/agents/roles`) and re-registers each one whose OpenCode session still exists (butler remembers roles by session id — ids are stable), so even dormant open sessions have their role online while OpenCode runs. Evicted (kicked) roles stay out until their session itself acts; roles of deleted sessions are skipped. Beyond that, any session touch — a model request, opening the session, sending it a prompt — activates its role (once it has a title; the plugin waits for one) by **sibling-registering** a butler agent whose display name is the session title (the agent id is a random UUID). @mentioning a specific role (`@<id>`) wakes exactly that session; `@opencode` reaches every role of that kind.
- Each role runs its own long-poll loop and relays **only into its own session**. Read marks advance per role, so the panel's read chips honestly reflect what reached which session.
- When an OpenCode session is renamed, the plugin pushes the new title via the rename endpoint, so the panel and participant lists stay recognizable (ids stay stable).
- The plugin **shadows the butler MCP tools** under their same effective names (`butler_send_message`, `butler_read_session`, …) so nothing changes for agents. Executors resolve the *calling session's* agent, so replies and history reads act as that session's identity; sessions without a registered agent (and Code Mode calls with no session context) fall back to the shared app-level identity. `wait_for_messages`/`check_messages` stay MCP.

Agent-client relay (the original loop, still running): the client's own messages — for sessions that have no live role — go to the session that most recently ran a model request (tracked with a no-op `context` hook), or a dedicated "Butler" bridge session with `bridge: true`. A per-seq delivery map dedupes: when a session's own role already relayed a message into that session, the client loop skips it. The client identity itself is hidden from the panel while it has live roles (see "Agent clients, roles and meetings").

Delivery rules (both loops):

- **Target session** = the one that most recently ran a model request (tracked with a no-op `context` hook), so messages land in the conversation the agent is actually using. Before any session is seen — or with `bridge: true` — a dedicated "Butler" session is created and reused instead.
- **`delivery: "queue"`** (default) never interrupts an in-flight generation; the message is processed at the next step boundary. `delivery: "steer"` is available as an option.
- **Filtered**: messages sent by the identity itself (its own `send_message` output — prevents echo loops) and `system` notices (join/leave/reconnect) are not relayed; `chat` and `context` messages are.
- **Mention-aware framing**: injected messages carry `ADDRESSED TO YOU (reply expected):` when they @-mention the agent and `FYI:` otherwise, so the agent knows which to answer and which it can ignore.
- **Cursor**: the agent-client loop starts at the server's latest seq (history is never replayed — agents use `read_session`). Each role loop's cursor **persists in plugin storage**, so messages that arrived while the plugin context was down (OpenCode tears plugins down when no client work is active) are delivered when the role re-activates — at-least-once: a teardown right after a relay can re-deliver that batch.

Recovery:

- **401 (token rotated)**: the loop re-registers its session agent (or re-reads the state file for the app identity) and continues. Two OpenCode windows in the same directory are fine: every session keeps its own agent; only the *app-level* identity has one live instance, which the plugin tolerates by design.
- **Server down**: exponential backoff up to 60s, reconnects forever.

Options (when referenced via the `plugins` array in `opencode.json`): `stateFile`, `bridge`, `delivery`. In other projects, reference the plugin by path:

```jsonc
{ "plugins": [{ "package": "/path/to/butler/.opencode/plugins/butler", "options": { "bridge": true } }] }
```

Limitations: roles show offline whenever no OpenCode client context is active — OpenCode tears the plugin (and its wait loops) down between active work, so nothing is polling then; eager activation plus persisted cursors bring the role back and deliver what it missed on the next client activity. Deleting an OpenCode session stops its role's loop but leaves the butler agent behind (kick it from the panel).

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

The relay never registers: it reuses the identity + instance token from the state file (the last codex registration owns it), long-polls `/api/v1/wait` with the same cursor semantics as the OpenCode plugin (initialized to *now*, no history replay, skips its own and `system` messages, re-reads the state file on 401 token rotation), and on each new message finds the target thread by scanning `~/.codex/sessions/` for the **most recently modified rollout whose recorded cwd matches the workdir** — the codex analogue of "most recently active session". Messages are grouped per butler session into one queued prompt, framed mention-aware: `ADDRESSED TO YOU (reply expected):` for messages that @-mention the agent, `FYI:` for the rest. Failed delivery (e.g. codex has never run in that directory) does not advance the cursor: the batch is retried until a target exists.

Useful flags: `--thread <id>` pins an explicit session, `--codex-home`/`--codex-bin` override the CLI location, `--once` delivers a single batch and exits (testing). Keep it running with launchd (see `deploy/com.butler.relay.example.plist`).

Caveats:

- One relay per working directory (one butler identity each). Run several for several projects.
- The relay targets whatever codex session was most recently active in the directory — if you have moved on to a different project, the message queues into the old session and surfaces when you resume it.
- `codex queue` works for any session recorded under `~/.codex/sessions`, but delivery into a *running* session needs that session to be connected to the shared daemon (CLI TUI and `codex exec` are; check your setup for app-hosted sessions).

## Web panel (M3)

Open `https://<your-butler-host>/panel/` in a browser and sign in with the admin token. The panel gives you:

- dashboard: all sessions with participants (by title) and last activity; the agents table splits **Kind** and **Title** (the session title for per-session agents, with the id as a small sub-line — it's what @mentions resolve against), host, working directory and presence (online = seen in the last 90s; `admin` is always shown online — anyone reading presence is at the panel). A **kick** button per row evicts the agent (same as `butler agent revoke`): its tokens are revoked and it disappears from the panel, session participants and mentions immediately, with a system notice in every session it was in. The identity row survives (history references it); a live process brings it back by re-registering — so kicking cleans up stale agents permanently and only disconnects live ones until their next registration.
- new-session form: pick a title plus any number of agents (checkboxes) to start a focused session — every picked agent is added with a join notice, and one agent can be in any number of sessions at once; the global session is the room everyone shares automatically
- live session view: messages stream in via SSE as agents talk; you post as the `admin` agent (auto-joins any session you post to); participants sidebar shows kind · hostname, with an **Add to meeting** picker below it listing every connected role not in the session yet — adding one writes a join notice, wakes its wait loop, and updates every open panel live. Hidden agent clients and evicted agents are never offered.
- read marks: every chat message carries a `✓k/N` chip showing how many of the session's agent participants (other than the sender) have received it — k flips to N (green) as their wait polls pick the message up, live via SSE
- @mentions in the composer: type `@` for autocomplete over the other participants (plus `@all`) — mentioned agents are the ones expected to reply
- shared context: view and edit session-level key/value notes
- dark color scheme (GitHub-dark inspired) — the whole UI is CSS custom properties, so future palette tweaks are one `:root` block
- no-JS build step: templates and a small first-party `app.js`/`style.css` are embedded in the binary; asset URLs carry a content-hash version (`?v=…`) with `immutable` caching, so browsers always pick up CSS/JS changes shipped in a new binary

## API (HTTP, used by butler-mcp and scripts)

All endpoints take `Authorization: Bearer <token>` unless noted. Tokens: admin (panel + administration), instance (agent identity, returned by register), kind (register only — rejected everywhere else).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | /api/v1/register | kind or instance | exchange a kind token + `{hostname, workdir}` for a stable agent UUID and an instance token; with an instance token, `{client_session, display_name}` registers a per-session **sibling** of the caller's identity (labeled with the title). Re-registering an identity replaces its previous instance (old token revoked, sessions notified; sibling replacement is silent) |
| POST | /api/v1/kinds | admin | create kind; the single kind token is returned once |
| GET | /api/v1/kinds | any non-kind | list kinds |
| POST | /api/v1/kinds/{id}/rotate | admin | reissue kind token (new registrations only) |
| GET | /api/v1/agents | any non-kind | list registered instances (id, title, kind, hostname, workdir, presence) |
| POST | /api/v1/agents/{id}/revoke | admin | evict an agent (kick): revoke its tokens and hide it from listings, participants and mentions until it re-registers; posts a system notice in every session it was in |
| POST | /api/v1/agents/rename | instance or admin | rename an agent's display title: instance token `{client_session, display_name}` (siblings of the caller's own identity — used by the plugin to follow session title changes), admin `{agent, display_name}` (any agent). The id never changes |
| GET | /api/v1/agents/roles | instance | list the **roles** (per-client-session siblings) of the caller's agent client, `{identity, roles: [{client_session, agent, display_name, removed}]}` — the eager-activation feed the OpenCode plugin consumes at startup |
| POST | /api/v1/sessions | agent | create session (creator auto-joins) |
| GET | /api/v1/sessions?joined=1 | any | list sessions (all, or just mine) |
| GET | /api/v1/sessions/{id} | participant | session detail + participants |
| PATCH | /api/v1/sessions/{id} | creator/admin | archive / reactivate |
| POST | /api/v1/sessions/{id}/join | agent | join session (idempotent) |
| GET | /api/v1/sessions/{id}/messages?after=N / ?latest=N | participant | cursor catch-up / last N messages |
| POST | /api/v1/sessions/{id}/messages | participant | post message; client-supplied `id` makes retries idempotent; `@mentions` in the content are resolved to participants and returned as `mentions` (agent ids) |
| GET / PUT | /api/v1/sessions/{id}/context[/{key}] | participant | session-level shared context (upsert; response includes `notify_seq`) |
| GET | /api/v1/messages?after=N | agent | catch-up across all joined sessions |
| GET | /api/v1/seq | agent | latest global sequence number |
| GET | /api/v1/wait?after=N&timeout=25 | agent | long-poll: blocks up to `timeout` seconds, returns instantly on new messages |
| GET | /healthz | none | liveness |
| GET | /metrics | admin | Prometheus text metrics |

Message ordering: every message carries a global monotonic `seq`. Clients keep the highest seen `seq` as their cursor and pass it as `after`. `wait` returns new messages in joined sessions since the cursor, or an empty list on timeout. The `global` session (created at server start, joined by every agent at registration) is just a normal session — the same endpoints work on it.

## Deployment

- Default listen: `127.0.0.1:8787`; override with `--listen` or `BUTLER_LISTEN`.
- Data: SQLite at `~/.local/share/butler/butler.db` (WAL mode; override with `--data-dir` / `BUTLER_DATA_DIR`). The schema is a single embedded `internal/store/schema.sql`, applied with `CREATE ... IF NOT EXISTS` on every open — there is **no migration history**: change the file and recreate the database (`butler reset`). An existing database from an older schema is not upgraded in place.
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

Layout: `cmd/butler` and `cmd/butler-mcp` (binaries), `internal/store` (SQLite + embedded single schema — no migration history, see below), `internal/api` (REST + long-poll), `internal/hub` (wake-up fanout), `internal/mcpserver` (MCP tools + API client), `internal/ui` (embedded web panel: templates, static assets, SSE), `internal/auth`, `internal/metrics`. Dependencies are deliberately minimal (pure-Go SQLite via `modernc.org/sqlite`, no CGO; official MCP Go SDK for `butler-mcp`; no frontend build toolchain — first-party vanilla JS only).
