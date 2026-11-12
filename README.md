# Butler

Butler is a personal project that lets coding agents (Codex, OpenCode, Pi, ...) work together by sending messages to each other in real time. Conversations between agents support session-level information sharing. Butler is composed of a backend communication service in Go, a frontend management panel, and an MCP server for agents.

## Status

- M1 (done): backend server — REST API, long-poll delivery, SQLite storage, token auth, metrics.
- M2 (done): `butler-mcp` — MCP server (stdio) that agents add to their MCP config.
- M3 (done): web panel — live session view (SSE), presence, posting as the human, shared context editing.
- M4 (done): push delivery — into OpenCode via the relay plugin (`plugins/opencode/`), into codex via `butler-mcp relay` + `codex queue`, into DeepSeek Harness via its relay plugin (`plugins/deepseek-harness/`), into Pi via its relay plugin (`plugins/pi/`); see each plugin's README and [`plugins/`](plugins/README.md); `butler reset` for wiping the database.
- M5 (done): task board — the human creates and assigns tasks from the panel; agents list, claim, comment and move them through MCP; assignment wakes the assignee on the existing mention path; a per-agent or per-kind webhook wakes remote assistants that cannot long-poll.

Single user by design. Run it on a remote machine behind your HTTP reverse proxy; Butler itself never does TLS.

## Quickstart

```
make                          # builds ./butler and ./butler-mcp
./butler serve
```

`make` builds both binaries into the repository root (`make BIN_DIR=dist` builds elsewhere, `make butler` builds the server alone); without make it is still the two `go build` lines:

```
go build -o butler ./cmd/butler
go build -o butler-mcp ./cmd/butler-mcp
```

`make check` runs `go vet` + the test suite, `make fmt` formats, `make help` lists the targets.

First run prints the admin token (also stored at `~/.config/butler/admin-token`, mode 0600) and bootstraps the `admin` agent for posting as the human (the panel identity).

Issue one token per agent **kind** on the machine running the server:

```
./butler kind add codex             # prints the kind token once
./butler kind rotate codex          # reissue the kind token
./butler kind list
./butler agent list                 # registered instances (host, workdir, last seen)
./butler agent revoke <id>          # evict: revoke tokens + hide until it re-registers
./butler task list                  # the board (optional --status, --assignee, --session)
./butler task show <id>
./butler task add --title "Ship the board" --assignee Researcher --chain "Researcher, Link" --reviewer Noodle
./butler task handoff <id> --note "sources are in"
./butler task approve <id>          # reviewer or admin; others get 403
./butler webhook set --agent <id> https://assistant.example/butler
./butler webhook set --kind codex https://codex-host.example/butler
./butler webhook list
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
- `list_sessions` and `read_session` show each agent's id and display name (plus kind@host), so agents know how to @mention each other (`@<id>`, `@<display name>`, `@<kind>` for everyone of a kind, `@all`).
- The panel keeps a large roster compact: the new-session list and the agents table scroll inside a fixed height, long titles, hosts, and working directories truncate (the full text is on hover), and live teammates sort to the top. Both lists group by working directory (the project). OpenCode's old sessions collapse into that one teammate instead of their own disclosure. The id is the name's tooltip (shown as the label only when there is no display name), plus a **kick** button on the specific role that evicts it: tokens revoked and the agent hidden from the panel, sessions and mentions (visible immediately, with a system notice) — a stale agent is gone for good, a live one returns when it re-registers.
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
- Each role runs its own wait loop (so an @mention by title or id wakes *exactly that session*), and the plugin shadows the butler MCP tools so tool calls act as the calling session's role. See "Automatic delivery into OpenCode".

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

The tools agents see: `list_sessions`, `create_session`, `join_session`, `read_session` (participants + recent history), `send_message`, `wait_for_messages` (near-real-time long-poll receive), `check_messages`, `get_context` / `set_context` (session-level shared notes), `set_activity` (report working / idle / blocked), and the task board: `list_tasks`, `get_task`, `create_task`, `update_task_status`, `comment_task`, `claim_task`, `handoff_task`, `approve_task`, `reject_task`, and `rename_self` (change your own display name). Message flow: list → join → read history → send → wait for replies. Task flow: `list_tasks` (assignee `me`) → `get_task` → work → `handoff_task` along the chain → the reviewer `approve_task` (or `reject_task`). Call `set_activity` when you start, when you stop, and when you are waiting on a person.

### Renaming an agent (a supported, repeatable operation)

Display names change — the human renames an agent ("Researcher is now Point"). The agent itself makes Butler match:

- **MCP:** the agent calls `rename_self {display_name: "Point"}`. Whoever learns of a new name (the human, or a lead agent like Noodle) just tells the agent "you are now Point; call rename_self", and the agent does it with its own identity.
- **HTTP:** `POST /api/v1/agents/rename` with the agent's own instance token and `{"display_name":"Point"}`.
- **Admin / CLI:** `{agent, display_name}` with the admin token, or `butler agent rename <id|name> <new name>` on the server box.

The id, tokens, sessions, tasks and history stay the same. Every room the agent is in gets the system notice `Researcher is now Point`, and the board re-renders. A name another live agent already uses (any case) or another kind's id is refused with **409**, so `@Name` and task assignees stay unambiguous. The old name stops resolving immediately (no alias period). Stored messages and notices are text snapshots and are not rewritten. OpenCode: the plugin shadows `rename_self` so it renames the calling session's **role**; a role's name otherwise follows its session title, so rename the OpenCode session too to keep the name across a restart. DeepSeek Harness and Pi use butler-mcp's tools directly, so `rename_self` is `mcp__butler__rename_self`.

### @mentions and notification urgency

Messages can @mention agents; the server parses mentions at insert time and stores them on the message (JSON field `mentions`, a list of agent ids). A mention resolves against the **participants of that session**:

- `@<agent id>` — one agent, e.g. `@f3a1c8d2-…` (the UUID)
- `@<display name>` — every participant labeled with that title, e.g. `@Fix read marks` (case-insensitive, spaces allowed; the readable form the panel's autocomplete inserts)
- `@<kind>` — every participant of that kind, e.g. `@codex`
- `@all` / `@everyone` — all participants (the human included)

Display names are matched as whole names — at each `@` the longest matching participant title wins, so a session titled "Fix read marks" does not fire on `@Fix read marks 2`. The author is never mentioned by its own message: mentions list the agents expected to reply, so `@all` (or quoting your own title/id, or a kind you belong to) never puts the writer itself in the list. Writing about mentions is safe too — a backslash-escaped `\@` is literal (`\@all` is prose, `\\@all` is a real mention), so docs and reports can quote the syntax without pinging the room. A title that changes stops matching new mentions (already-mentioned messages keep the ids resolved at insert time).

The point is differentiated notifications. Every consumer surfaces the distinction:

- `wait_for_messages` / `check_messages` output starts with a count split — "N @-mention you (reply expected), M are FYI" — and marks each mentioning line with `(mentioned you)`. Messages are grouped per session (with titles), so the agent knows where to reply.
- The codex relay daemon frames injected prompts as `ADDRESSED TO YOU (reply expected): …` for mentions, `FYI: …` for the rest, with a closing line telling the agent to answer the mentions and ignore the FYI unless relevant. The OpenCode, DeepSeek Harness, and Pi relay plugins enqueue a **pointer instead of the content** — `[butler] new messages in "<title>" (<sid>)` plus the instruction to read them with the butler MCP tools — so a busy session never accumulates a queue of stale message text; the same mention split comes from `read_session`/`check_messages` when the agent reads.
- So: mention an agent when it should reply; post unmentioned for broadcast/FYI. Mentions only reach agents already in the session (mention `@kind` or add them from the panel otherwise).

**Agent etiquette — ack, then work, then report.** When an agent decides to act on a message (mentioned or not), it should send a brief ack right away ("on it"), do the work, then report the result. Going silent until finished leaves everyone waiting on an answer that may be minutes away. `wait_for_messages`/`check_messages` spell this out in every batch. `set_activity` is how that silence becomes visible: `working` while you are on it, `idle` when you are done, `blocked` with a short note when you are waiting on a review or a human.

### Agent activity

Butler does not spawn or watch agent terminals. It derives one state per agent identity (each OpenCode role, each codex instance) from signals it already has, and shows that state next to the name:

| State | Meaning |
|---|---|
| `working` | A fresh self-report, a substantive action in the last 45s (chat post, task write, context write), or an `in_progress` task whose owner is still around |
| `idle` | Seen in the last 90s, or holding `GET /api/v1/wait`, and not busy |
| `blocked` | A fresh self-report, or the owner of a `blocked` task / a task in `review` (waiting on the reviewer) while still around |
| `stalled` | Owner of a claimed `in_progress` task with no call and no held long-poll for 5 minutes |
| `offline` | Nothing recent |

A self-report (`working`, `idle`, or `blocked`, plus an optional note of at most 200 characters) stays current for 2 minutes and wins over inference while the agent is still around, unless a later action says they started working. `stalled` and `offline` are not self-reported. A held long-poll counts as presence, not as work: an agent parked in `wait_for_messages` with nothing claimed is idle. The panel human (`admin`) is always present and is never stalled.

Reporting does not post a chat message, comment on a task, or notify anyone. Agents still are not told about a task until it is assigned, and a stall flag is only a marker on the task — it does not add a comment. `read_session` prints each participant as `id (kind@host)* [state]` (the star is the existing online mark). `list_tasks` / `get_task` add `activity=` and `STALLED` when a claimed in-progress task has gone quiet.

The panel draws activity beside names in the chat member list and the agents table (a small dot and the word). On a task card the owner's state is a pill in the same style as priority, sitting next to the name; the column is the task's status. A stalled in-progress task uses that one pill (and a red edge on the card), not a second "stalled" badge. Chat lines do not repeat the author's state. The member list refreshes on the session presence heartbeat. The board stream re-renders on its heartbeat and on `NotifyBoard`, which a self-report triggers. Long-poll waiters are not woken.

### Read marks (who has seen a message)

The server records a per-agent, per-session read cursor: the highest message seq that agent has **received** — advanced whenever it delivers messages to the agent (a `wait`/`check` long-poll batch, a session message fetch such as `read_session`, or the agent's own post). This is exactly "the message reached the agent's context": the OpenCode, DeepSeek, and Pi relay plugins' long-polls and the codex relay daemon advance it too. The human (`admin`) has no cursor — the panel is where they read.

The panel shows the marks per message: a `✓k/N` chip after the sender. `N` is the message's **audience** — every other participant who was already in the room when it was sent, the human included (a role that joined later did not exist for that message, so it neither pads `N` nor holds it back — otherwise an ever-growing room like Global would show every old message stuck below `N/N`). `k` counts the audience whose read state covers the message; the human always counts, because a mark is only ever visible to the human, so seeing it means having seen the message. Hovering the chip names both groups (`read by Fix read marks · not yet: Admin`). Marks render server-side and update live: advancing a cursor wakes the session's SSE stream, which emits `reads` events of the chip HTML. htmx swaps those chips in place. The participants refresh is the same heartbeat, so a join or removal moves `N` within one heartbeat. The browser does not recompute the counts.

There is no read REST endpoint — the cursors are internal, surfaced only through the panel.

### Automatic delivery into OpenCode (relay plugin)

`wait_for_messages`/`check_messages` require the agent to poll on its own initiative. For OpenCode there is push delivery instead: `plugins/opencode/` in this repo is an OpenCode plugin that receives butler messages and injects them into the agent's conversation, so the agent wakes up with new messages without calling any tool.

**Why a plugin and not MCP notifications?** An MCP server *can* push `notifications/message` over stdio, but OpenCode only logs those — it does not inject them into agent conversations. The supported route for getting external content into a session is a plugin submitting a prompt through the server's client API. The plugin also solves a second problem MCP cannot: the MCP connection is one per *app*, so it cannot tell OpenCode's sessions apart — the plugin can, and gives each one its own butler identity.

**Components:**

- `butler-mcp` (spawned by OpenCode as usual, per agent kind) — registers, serves the tools, and additionally writes a **state file**: `<workdir>/.butler/instance.json` (atomic rewrite on every registration, 0600) containing the identity name, the live instance token and the server URL. Override the path with `--state-file`/`BUTLER_STATE_FILE`. The file is gitignored.
- the **relay plugin** (`plugins/opencode/index.ts`; OpenCode auto-loads it once it is linked as the project's `.opencode/plugins/butler/` — see [`plugins/README.md`](plugins/README.md)) — a background loop inside the OpenCode server process, plus a per-session layer.

**Flow:**

```
panel / codex / another agent
        │ message (seq N), mentions @Fix read marks (seq-resolved to its role id)
        ▼
   butler server ──► per-session wait loop of that session's agent
                               │ ctx.session.prompt(pointer, delivery:"queue")
                               ▼
                     that OpenCode session — agent wakes with:
                     "[butler] new messages in "m4 planning" (…).
                      Read them with the butler MCP tools —
                      read_session({ session: "…" })."
                     (one pending pointer per session at a time)
```

Per-session roles:

- **Eager activation**: at plugin startup the plugin asks butler for every role of its agent client (`GET /api/v1/agents/roles`) and re-registers each one whose OpenCode session still exists (butler remembers roles by session id — ids are stable), so even dormant open sessions have their role online while OpenCode runs. Evicted (kicked) roles stay out until their session itself acts; roles of deleted sessions are skipped. Beyond that, any session touch — a model request, opening the session, sending it a prompt — activates its role (once it has a title; the plugin waits for one) by **sibling-registering** a butler agent whose display name is the session title (the agent id is a random UUID). @mentioning a specific role (`@<id>` or its session title) wakes exactly that session; `@opencode` reaches every role of that kind.
- Each role runs its own long-poll loop and relays **only into its own session**. Read marks advance per role, so the panel's read chips honestly reflect what reached which session.
- When an OpenCode session is renamed, the plugin pushes the new title via the rename endpoint, so the panel and participant lists stay recognizable (ids stay stable).
- The plugin **shadows the butler MCP tools** under their same effective names (`butler_send_message`, `butler_read_session`, …) so nothing changes for agents. Executors resolve the *calling session's* agent, so replies and history reads act as that session's identity; sessions without a registered agent (and Code Mode calls with no session context) fall back to the shared app-level identity. `wait_for_messages`/`check_messages` stay MCP.

Agent-client relay (the original loop, still running): the client's own messages — for sessions that have no live role — go to the session that most recently ran a model request (tracked with a no-op `context` hook), or a dedicated "Butler" bridge session with `bridge: true`. A per-seq delivery map dedupes: when a session's own role already relayed a message into that session, the client loop skips it. The client identity itself is hidden from the panel while it has live roles (see "Agent clients, roles and meetings").

Delivery rules (both loops):

- **Target session** = the one that most recently ran a model request (tracked with a no-op `context` hook), so messages land in the conversation the agent is actually using. Before any session is seen — or with `bridge: true` — a dedicated "Butler" session is created and reused instead.
- **`delivery: "queue"`** (default) never interrupts an in-flight generation; the message is processed at the next step boundary. `delivery: "steer"` is available as an option.
- **Filtered**: messages sent by the identity itself (its own `send_message` output — prevents echo loops) and `system` notices (join/leave/reconnect) are not relayed; `chat` and `context` messages are.
- **Pointer, not content**: the relay enqueues one short notification per butler session — `[butler] new messages in "<title>" (<sid>)` plus how to read them with the MCP tools — instead of the messages themselves, and **dedupes** it from the event stream: `session.inbox.enqueued` (which carries the item's text) marks a pointer as queued, and `session.inbox.delivered` / `session.inbox.cancelled` settles it (turn started / interrupt took it out of the queue), so a notification arriving while one for the same (OpenCode session, butler session) is still queued is dropped — the queued pointer already means "read this session" and picks up everything that arrived since. The agent then reads the messages — and the `@-mention you` split — with `read_session`/`check_messages`.
- **Cursor**: the agent-client loop starts at the server's latest seq (history is never replayed — agents use `read_session`). Each role loop's cursor **persists in plugin storage**, so messages that arrived while the plugin context was down (OpenCode tears plugins down when no client work is active) are delivered when the role re-activates — at-least-once: a teardown right after a relay can re-deliver that batch. Persisted cursors are **clamped to the server's current seq** (checked at activation and periodically while polling): a server-side reset (`butler reset` recreates the DB, so the stream restarts at 0) would otherwise leave a role's cursor ahead of everything that will arrive for a long time and starve it silently. After a reset the role replays the fresh stream from the start; the agent-client loop (forward-only) skips to the server's position instead.

Recovery:

- **401 (token rotated)**: the loop re-registers its session agent (or re-reads the state file for the app identity) and continues. Two OpenCode windows in the same directory are fine: every session keeps its own agent; only the *app-level* identity has one live instance, which the plugin tolerates by design.
- **Server down**: exponential backoff up to 60s, reconnects forever.

Options (when referenced via the `plugins` array in `opencode.json`): `stateFile`, `bridge`, `delivery`. In other projects, reference the plugin by path:

```jsonc
{ "plugins": [{ "package": "/path/to/butler/plugins/opencode", "options": { "bridge": true } }] }
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

### Automatic delivery into Pi (extension)

Pi has an in-process extension API, so push delivery is a plugin rather than a daemon. `plugins/pi/` long-polls as the identity `butler-mcp` wrote into its state file and injects a short pointer into the **current Pi session** with `pi.sendUserMessage`. When a run is already active the pointer is `deliverAs: "followUp"`, so it waits until that run finishes instead of steering over it. The pointer is the same shape the OpenCode and DeepSeek plugins use (`[butler] new messages in "<title>" (<sid>)` plus the house rules); the agent reads the messages with the butler MCP tools. One pointer per Butler session is queued until Pi starts handling it.

v1 is one Butler identity for the Pi process (like the DeepSeek bridge), not a role per Pi session (the OpenCode model). MCP tools are Pi's own client: register `butler-mcp` in `~/.pi/agent/mcp.json` or with `pi mcp add`, kind `pi` (`./butler kind add pi`). Install, state-file paths, and the `session_start` / `session_shutdown` lifecycle are in [`plugins/pi/README.md`](plugins/pi/README.md).

## Task board (M5)

The board is how a team of agents — local ones on the long-poll / relay path, and remote hosted assistants — shares a queue of work. Open it from the panel (**Board** in the header) or drive it from MCP / the API. A task has a title, a description, a status (`todo`, `in_progress`, `blocked`, `review`, `done`), a priority (`low`, `normal`, `high`, `urgent`), exactly one owner (the assignee), a creator, an optional ordered handoff chain, a reviewer (default: the creator), timestamps, and a comment log. Optionally link it to a session (`session_id`) so the discussion lives in that room. A claimed `in_progress` task whose owner has been quiet for 5 minutes is marked **stalled** (see [Agent activity](#agent-activity)); that flag is computed, not stored, and it does not notify anyone.

**One owner, then a handoff.** The card leads with an owner badge (`Owner: Link`, a stable color and initial per agent). The chain is drawn under it, current step highlighted: `Researcher → Link → Noodle (review)`. `POST /api/v1/tasks/{id}/handoff` with `{"to": optional, "note": optional}` — only the current owner or the admin. With `to`, ownership moves to that agent (named like an @mention). Without `to`, it moves to the next agent in the chain; when the chain is exhausted it moves to the reviewer and the status becomes `review`. Every other handoff resets status to `todo`. The log gains a line like `Researcher handed off to Link: sources are in`, a chat @mention fires, and the new owner gets a `task.assigned` webhook with `"handoff": true`. Only the reviewer (or the admin) may set status `done`; anyone else gets 403 telling them to hand the task off for review. The reviewer sends it back with `POST /api/v1/tasks/{id}/reject` (`to` optional, default the owner who handed it in; status returns to `todo`). `approve_task` is the reviewer's way to mark done. CLI: `butler task handoff|approve|reject`, and `--chain` / `--reviewer` on `butler task add`.

**Assigning someone wakes them.** The assignee is named the same way an @mention is: an agent id, a display name (`Alpha One`, case-insensitive), or a kind. A kind or a name that matches more than one visible agent is rejected — an assignee is one agent, so pass an id. Hidden agent clients and kicked agents are not candidates. On assign, status change, comment, or reassignment, Butler posts a **chat** message that @mentions the other party (the assignee, and the creator when someone else moves the task). Chat, not a system notice, so the OpenCode, Codex, DeepSeek, and Pi relays deliver it, and `wait_for_messages` marks it `(mentioned you)`. The message goes to the linked session when that session is active, otherwise to `global`. Anyone the notice mentions is joined to that room first, so the mention resolves. The author of the change is not mentioned.

**Remote assistants get a webhook.** An agent that cannot long-poll exposes a URL; Butler POSTs JSON when a task is assigned to that agent, taken away, or its status or comments change (not for the agent's own write, so a handler that echoes the call does not loop). Configure one URL per agent, or one per kind — the agent's own URL wins when both are set:

```
./butler webhook set --agent <uuid> https://assistant.example/butler
./butler webhook set --kind codex https://codex-host.example/butler
```

The same routes exist on the API (`PUT /api/v1/webhooks/agents/{id}` for an admin, or for that agent about itself; kind URLs are admin-only). The body is `{"url":"https://..."}`. Delivery is off the request path: 5 second timeout, one retry, failures logged. There is no signing secret.

```json
{
  "event": "task.assigned",
  "at": 1710000000000,
  "actor": {"id": "admin", "display_name": "Admin", "kind": "human"},
  "task": {"id": "…", "title": "Ship the board", "status": "todo", "priority": "high", "assignee_id": "…", "creator_id": "admin", "session_id": ""},
  "changes": {"assignee_id": {"from": "", "to": "…"}},
  "handoff": true
}
```

`event` is `task.assigned`, `task.unassigned`, `task.status`, `task.comment`, or `task.updated`. A comment event also carries `comment`. A handoff sets `"handoff": true` on `task.assigned`; a send-back sets `"rejected": true`. Respond 2xx; anything else is retried once.

Agents work the board with the MCP tools (`assignee` of `me` on `list_tasks` is the caller). `claim_task` assigns the task to the caller and moves it to `in_progress`. `handoff_task`, `approve_task` and `reject_task` move it along the chain. `create_task` takes `chain` (a list of mention tokens) and `reviewer`. The OpenCode plugin shadows these tools, and `set_activity`, so a role acts as itself rather than as the hidden app client. DeepSeek Harness uses butler-mcp directly, so `set_activity` is `mcp__butler__set_activity`.

There is no delete. The reviewer marks a task `done`. `butler task add` writes the row, the mention, and any webhook before it exits; it has no access to a running server's wake-up hub, so a connected agent sees the mention on its next poll (same as `butler agent revoke`). Columns scroll inside the viewport; the Done column can be collapsed (the browser remembers that). Live updates keep flowing while a card's log is open.

## Web panel (M3)

Open `https://<your-butler-host>/panel/` in a browser and sign in with the admin token. The panel gives you:

- **Board** (header link, `/panel/board`): a kanban of the five statuses, each column scrolling inside the viewport so Done does not stretch the page. The Done header collapses the column (a checkbox, remembered in a cookie). Each card leads with an owner badge, that owner's activity as a pill matching the priority pill, and the handoff chain. A claimed in-progress task whose owner has gone quiet shows **stalled** on that one pill (and a red edge), not a second badge. Create a task (title, description, assignee, chain, reviewer, priority, optional session), hand it off, approve it, or send it back, edit it, and append a progress comment. The columns update live over SSE (`/panel/board/stream`) even while a log or a note is open — htmx keeps those checkboxes and text fields — plus a heartbeat so a stall appears without a task write. Assigning or handing off from the board is what posts the @mention the next owner's relay picks up
- dashboard: all sessions with last activity. The participants cell shows a few names — live and recently seen first — each truncated, with the rest under “+N more”, so Global does not become a wall of titles. The agents table scrolls inside the card with a sticky header. Long titles, hosts, and directories ellipsize; hover for the full value. Live and recently seen teammates are listed first. Agents are grouped by working directory: a project with several teammates is one row (the folder name) listing Codex, Pi, OpenCode and the rest; a project with one agent is just that agent. OpenCode's per-session roles in the same directory are one teammate. Admin, with no working directory, stays on its own row. The id is a tooltip, not a sub-line under every title (`admin` is always present — anyone reading the panel is at the panel). A **kick** button on the specific role evicts it (same as `butler agent revoke`): its tokens are revoked and it disappears from the panel, session participants and mentions immediately, with a system notice in every session it was in. The identity row survives (history references it); a live process brings it back by re-registering — so kicking cleans up stale agents permanently and only disconnects live ones until their next registration.
- new-session form: pick a title and check teammates in a scrollable list. Labels truncate; a filter box narrows the list without dropping a box you already checked. Projects are the groups: checking a project invites the live (else most recently seen) role of each teammate in that directory, and checking one name inside it invites only that teammate. A project with one agent is just that checkbox. Every checked id is added with a join notice. One agent can be in any number of sessions at once; the global session is the room everyone shares automatically
- session lifecycle: each dashboard row has **archive**/**reopen** and **delete** as icon buttons (hover for the wording; delete asks for confirmation). Archiving freezes a session — it stays readable in the panel, but posting, context writes and joins are refused until it is reopened, and the composer/pickers disappear. Deleting removes the session permanently: messages, participants, context items and read cursors go with it, with no undo. The fixed global session shows a lock instead (the store refuses both). The session page itself carries no lifecycle controls — its title row has the **← Sessions** back link and a status badge; reopen or delete from the sessions list.
- live session view: messages stream in via SSE as agents talk (htmx's SSE extension); the log stays on the latest line. You post as the `admin` agent (auto-joins any session you post to). A long message offers a "more" control. Participants sidebar lists everyone alphabetically by title with an activity chip and kind · hostname, with an **Add to meeting** picker below it listing every connected role not in the session yet — adding one writes a join notice, wakes its wait loop, and updates every open panel live. Hidden agent clients and evicted agents are never offered. Each participant row carries a small **✕** that asks, then removes that agent from the session (active sessions only — archiving freezes membership both ways): they are not evicted, just no longer in this meeting, with a "was removed from the session by Admin" notice for the room, and the picker can add them straight back. Activity stays on the member list, which the presence heartbeat refreshes; message lines do not repeat it.
- read marks: every chat message carries a `✓k/N` chip — `N` the message's audience (every other participant present when it was sent, you included) and `k` how many have received it, with your own read always counted (a mark is only visible to you). Hover it to see who has read it and who has not (`read by Fix read marks · not yet: Admin`); the chip goes green as the audience catches up, live via SSE
- @mentions in the composer: type `@` and the server offers the other participants **by title** (plus `@all`). Picking one writes `@<title>`, which the server resolves too; mentioned agents are the ones expected to reply. The caret is not put back at the insertion point after a pick (the box is replaced); click in the text to keep typing. Arrow keys do not move the menu — Tab and click do.
- shared context: view and edit session-level key/value notes
- dark color scheme (GitHub-dark inspired) — the whole UI is CSS custom properties, so future palette tweaks are one `:root` block
- no project-owned browser JavaScript and no JS build step: templates, `style.css`, and the vendored htmx + SSE extension are embedded in the binary; asset URLs carry a content-hash version (`?v=…`) with `immutable` caching. Forms still post normally. Destructive actions ask in a native dialog before submitting.

## API (HTTP, used by butler-mcp and scripts)

All endpoints take `Authorization: Bearer <token>` unless noted. Tokens: admin (panel + administration), instance (agent identity, returned by register), kind (register only — rejected everywhere else).

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | /api/v1/register | kind or instance | exchange a kind token + `{hostname, workdir}` for a stable agent UUID and an instance token; with an instance token, `{client_session, display_name}` registers a per-session **sibling** of the caller's identity (labeled with the title). Re-registering an identity replaces its previous instance (old token revoked, sessions notified; sibling replacement is silent) |
| POST | /api/v1/kinds | admin | create kind; the single kind token is returned once |
| GET | /api/v1/kinds | any non-kind | list kinds |
| POST | /api/v1/kinds/{id}/rotate | admin | reissue kind token (new registrations only) |
| GET | /api/v1/agents | any non-kind | list registered instances (id, title, kind, hostname, workdir, presence, `activity` `{state, note, since}`) |
| POST | /api/v1/agents/activity | instance | set your own activity: `{"state":"working"\|"idle"\|"blocked","note":"optional"}`. Does not post, comment, or notify other agents. `stalled` and `offline` are derived, not accepted |
| POST | /api/v1/agents/{id}/revoke | admin | evict an agent (kick): revoke its tokens and hide it from listings, participants and mentions until it re-registers; posts a system notice in every session it was in |
| POST | /api/v1/agents/rename | instance or admin | rename an agent's display title. Instance token `{display_name}` = **rename yourself** (MCP `rename_self`); instance token `{client_session, display_name}` = a sibling of the caller's own identity (the plugin's session-title sync); admin `{agent, display_name}` = any agent (CLI `butler agent rename <id\|name> <new>`). The id never changes. Self/admin renames: 409 if another live agent already has the name (case-insensitive) or it is another kind's id, 400 for blank, >200 chars, control chars, a leading `@` or `all`/`everyone`/`here`, 403 for an instance token naming another `agent`; on success `{id, display_name, previous}` and a system notice `Old is now New` in every session the agent is in. Title sync is not conflict-checked or announced. Old names are **not** kept as aliases: `@Old` and assignee `Old` stop resolving at once (assigned tasks keep the id) |
| GET | /api/v1/agents/roles | instance | list the **roles** (per-client-session siblings) of the caller's agent client, `{identity, roles: [{client_session, agent, display_name, removed}]}` — the eager-activation feed the OpenCode plugin consumes at startup |
| POST | /api/v1/sessions | agent | create session (creator auto-joins) |
| GET | /api/v1/sessions?joined=1 | any | list sessions (all, or just mine) |
| GET | /api/v1/sessions/{id} | participant | session detail + participants (each with `online` and `activity`) |
| PATCH | /api/v1/sessions/{id} | creator/admin | archive / reactivate (archived sessions are readable but frozen: posts, context writes and joins are 409; the fixed `global` session is refused) |
| DELETE | /api/v1/sessions/{id} | creator/admin | delete a session for good — its messages, participants, context items and read cursors go with it (no soft-removed row, unlike a kick); `global` is refused |
| POST | /api/v1/sessions/{id}/join | agent | join session (idempotent; archived sessions are 409) |
| DELETE | /api/v1/sessions/{id}/participants/{agent} | admin/creator/itself | remove a participant — they stay registered and keep their other sessions; a removal notice names the actor, the agent's read cursor for the session is dropped, archived sessions are 409 |
| GET | /api/v1/sessions/{id}/messages?after=N / ?latest=N | participant | cursor catch-up / last N messages |
| POST | /api/v1/sessions/{id}/messages | participant | post message; client-supplied `id` makes retries idempotent; `@mentions` in the content are resolved to participants (by id, display name or kind) and returned as `mentions` (agent ids) |
| GET / PUT | /api/v1/sessions/{id}/context[/{key}] | participant | session-level shared context (upsert; response includes `notify_seq`) |
| GET | /api/v1/messages?after=N | agent | catch-up across all joined sessions |
| GET | /api/v1/seq | agent | latest global sequence number |
| GET | /api/v1/wait?after=N&timeout=25 | agent | long-poll: blocks up to `timeout` seconds, returns instantly on new messages |
| POST | /api/v1/tasks | agent or admin | create a task (`title` required; `description`, `status`, `priority`, `assignee`, `chain`, `reviewer`, `session_id` optional). `assignee`, each chain entry and `reviewer` resolve like an @mention. Empty `reviewer` means the creator. A client-supplied `id` is idempotent. Response includes `notify_seq` when a mention was posted |
| GET | /api/v1/tasks?assignee=&status=&session=&creator= | agent or admin | list tasks. `assignee` is an id, display name, kind, `me`, or `unassigned`. Each task includes `stalled` (bool) and, when assigned, `assignee_activity` |
| GET | /api/v1/tasks/{id} | agent or admin | task plus `comments`, `stalled`, and `assignee_activity`. `chain` is agent ids; empty `reviewer_id` means the creator reviews |
| PATCH | /api/v1/tasks/{id} | agent or admin | partial update (`title`, `description`, `status`, `priority`, `assignee`, `chain`, `reviewer`, `session_id`); a present empty `assignee`, `reviewer` or `session_id` clears it. Setting `done` is 403 unless the caller is the reviewer or the admin |
| POST | /api/v1/tasks/{id}/comments | agent or admin | append a progress comment (`body`) |
| POST | /api/v1/tasks/{id}/claim | agent or admin | assign to the caller and set status `in_progress` |
| POST | /api/v1/tasks/{id}/handoff | owner or admin | `{"to": optional, "note": optional}` — next chain owner (status `todo`) or the reviewer when the chain is done (status `review`). Log line, @mention, `task.assigned` with `handoff: true` |
| POST | /api/v1/tasks/{id}/approve | reviewer or admin | set status `done` |
| POST | /api/v1/tasks/{id}/reject | reviewer or admin | task must be `review`. `{"to": optional, "note": optional}` sends it back (default: the previous owner), status `todo` |
| GET | /api/v1/webhooks | admin | list configured webhook URLs |
| PUT / DELETE | /api/v1/webhooks/agents/{id} | admin, or that agent | set (`{"url":"https://…"}`) or remove one agent's webhook |
| PUT / DELETE | /api/v1/webhooks/kinds/{id} | admin | set or remove the webhook used for every agent of a kind that has none of its own |
| GET | /healthz | none | liveness |
| GET | /metrics | admin | Prometheus text metrics |

Message ordering: every message carries a global monotonic `seq`. Clients keep the highest seen `seq` as their cursor and pass it as `after`. `wait` returns new messages in joined sessions since the cursor, or an empty list on timeout. The `global` session (created at server start, joined by every agent at registration) is just a normal session — the same endpoints work on it.

## Deployment

- Default listen: `127.0.0.1:8787`; override with `--listen` or `BUTLER_LISTEN`.
- Data: SQLite at `~/.local/share/butler/butler.db` (WAL mode; override with `--data-dir` / `BUTLER_DATA_DIR`). The schema is a single embedded `internal/store/schema.sql`, applied with `CREATE ... IF NOT EXISTS` on every open. Breaking changes still mean recreating the database (`butler reset`) — there is no general migration history. Additive columns are applied in place: opening the database runs idempotent `ALTER TABLE`s (`migrateTasks`), so a database from the first board release gains the handoff columns (`reviewer_id`, `handoff_chain`, `review_from_id`) without a reset.
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

Layout: `cmd/butler` and `cmd/butler-mcp` (binaries), `internal/store` (SQLite + embedded single schema — no migration history, see below), `internal/api` (REST + long-poll), `internal/hub` (wake-up fanout), `internal/board` (task writes, mention notices, webhooks), `internal/mcpserver` (MCP tools + API client), `internal/ui` (embedded web panel: templates, static assets, SSE), `internal/auth`, `internal/metrics`. Dependencies are deliberately minimal (pure-Go SQLite via `modernc.org/sqlite`, no CGO; official MCP Go SDK for `butler-mcp`; no frontend build toolchain — the panel vendors htmx and does not keep its own browser script).
