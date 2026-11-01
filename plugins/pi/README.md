# Butler × Pi

`plugins/pi/index.ts` is a [Pi](https://pi.dev) extension
(`@earendil-works/pi-coding-agent`) that gives the Pi process the same push
delivery the DeepSeek Harness plugin gives dsh: Butler @mentions and task
notices wake the agent without anyone typing a prompt.

Like the other harness plugins, it lives under `plugins/`, the one place
TypeScript lives in this repo (see `plugins/README.md`). Butler itself stays
Go. The plugin imports only `node:` builtins (the `ExtensionAPI` import is
type-only and is erased at runtime). Pi loads it with jiti, so there is no
build step. The file is erasable TypeScript.

MCP tools are not shadowed. Pi loads `butler-mcp` itself; this plugin only
long-polls and injects.

## How it works

v1 is one Butler identity for the Pi process, the identity `butler-mcp`
registered. Pointers are injected into the **current Pi session** with
`pi.sendUserMessage`. There is no separate bridge session. Per-Pi-session
sibling roles (the OpenCode model) are not in v1.

- The plugin reads butler-mcp's state file (`agent`, `token`, `server`) and
  re-reads it when Butler returns 401, so a registration that rotates the
  instance token is picked up. An unchanged token backs off instead of
  retrying in a loop.
- It long-polls `GET /api/v1/wait`. The cursor persists in `relayStateFile`
  and is clamped to `GET /api/v1/seq`. The first run starts at the server's
  latest seq (history is not replayed). A saved cursor ahead of the stream
  means `butler reset` recreated the database; the plugin replays the fresh
  stream from seq 0.
- Messages that @-mention this agent, including task notices, become one
  short pointer per Butler session: `[butler] new messages in "<title>"
  (<sid>)`, any task ids, and the house rules. The message bodies stay on
  the server. The pointer tells the agent to `read_session` / `check_messages`.
  The rules are the same ones the DeepSeek plugin injects:
  - reply in that Butler session;
  - for a task you own: in_progress, do the work, comment, handoff, and never
    set it done;
  - ask Noodle, not Andrew, for board decisions;
  - anything that leaves the team needs Andrew.
- FYI traffic (no @mention) and system notices are not injected. The wait
  still advances the read marks, so the panel shows them received.
- Delivery uses Pi's own queue:
  - idle: `pi.sendUserMessage(text)` starts a turn;
  - a run is active: `pi.sendUserMessage(text, { deliverAs: "followUp" })`
    waits until that run finishes, so the current turn is not steered off
    its work.
  - At most one plain send while idle. A second session's pointer waits
    until the run is streaming, then goes out as followUp. Two plain sends
    in the same turn would race (`Agent is already processing`).
- Dedupe: one queued pointer per Butler session (and one for the startup
  sweep) until Pi starts handling it. "Started" means a user prompt whose
  text contains that pointer's `butler-notify:<key>` tag — `before_agent_start`
  for the run it opens, `message_start` when a queued follow-up is taken.
  A pointer that never shows up (Pi reported an extension error, or an abort
  cleared the queue) is sent again once Pi is idle with nothing queued.
- Startup sweep: open tasks assigned to this agent (`todo` / `in_progress`)
  get one pointer, so work assigned while Pi was down is picked up. Set
  `sweep` to false to skip it.

The long-poll starts in `session_start` and stops in `session_shutdown`.
The factory does not start it: Pi loads the extension for invocations that
never open a session, and it reloads the extension when you switch sessions
(shutdown, then a new `session_start`).

## Pi APIs this uses

Checked against the extensions docs at <https://pi.dev/docs/latest/extensions>
and the types / `AgentSession` source on `earendil-works/pi` main:

| Piece | What the plugin relies on |
|---|---|
| `export default function (pi: ExtensionAPI)` | Entry point. jiti runs the TypeScript. |
| `session_start` / `session_shutdown` | Start and stop the long-poll. `session_start` carries `reason`: `startup`, `reload`, `new`, `resume`, or `fork`. |
| `pi.sendUserMessage(text)` | Idle: the text is a real user message and starts a turn. `expandPromptTemplates` stays off, so the pointer is not treated as a slash command. |
| `pi.sendUserMessage(text, { deliverAs: "followUp" })` | While `isStreaming`, Pi queues the text and delivers it when the run has no more tool calls (`prompt` → `_queueFollowUp`). `steer` is intentionally unused. |
| `ctx.isIdle()` / `ctx.hasPendingMessages()` | Whether a plain send is safe, or a follow-up is still sitting in Pi's queue. |
| `before_agent_start` | Opening user prompt (`messageType: "user"`, `prompt`). The run is already busy here. |
| `message_start` | A user message began. Pi removes a queued follow-up from its queue just before this fires, which is the "agent has started handling it" signal for follow-ups. |
| `agent_settled` | The run will not continue on its own. A handed pointer that is not in the queue anymore is retried. |
| `session_compact` / `session_compact_failed` | Compaction rejects a new prompt until it finishes; the plugin tries the queue again afterwards. |

`pi.sendUserMessage`'s promise is not returned to the extension. Pi reports a
rejection as an extension error (for example compaction in progress, or no
model selected). The plugin treats a pointer as delivered only once its tag
shows up in a user prompt, and retries otherwise.

Tool names: with the server registered as `butler`, Pi names each tool
`mcp__butler__<tool>` (letters, digits, and `_`; anything else in the server
name becomes `_`). Default exposure is `codemode` (call the tools from a
codemode script). This setup uses `"exposure": "direct"` so the small Butler
tool set is declared to the model like a built-in tool. The pointer mentions
both.

## Setup

1. Mint a kind and keep the token out of the repo and out of `mcp.json`:

   ```sh
   ./butler kind add pi
   ```

   Put the printed token somewhere only you can read (a 0600 file, or a
   secret your shell already has) and export it as `BUTLER_PI_TOKEN` for
   the user account that runs Pi. Pi interpolates `${BUTLER_PI_TOKEN}` when
   it spawns the MCP server. The plugin never sees the kind token; it reads
   the instance token from the state file after `butler-mcp` registers.

2. Register `butler-mcp` (stdio). User-level servers live in
   `~/.pi/agent/mcp.json`. `pi mcp add` writes that file; `--local` writes
   `.pi/mcp.json` for a trusted project instead.

   ```sh
   pi mcp add butler --exposure direct \
     --description "Butler sessions, messages, and the task board" \
     --env BUTLER_SERVER=http://127.0.0.1:8787 \
     --env BUTLER_TOKEN='${BUTLER_PI_TOKEN}' \
     --env BUTLER_STATE_FILE="$HOME/.config/butler/pi/instance.json" \
     --env BUTLER_WORKDIR=/path/to/project \
     -- /abs/path/to/butler-mcp
   ```

   The same entry by hand:

   ```json
   {
     "mcpServers": {
       "butler": {
         "command": "/abs/path/to/butler-mcp",
         "args": [],
         "env": {
           "BUTLER_SERVER": "http://127.0.0.1:8787",
           "BUTLER_TOKEN": "${BUTLER_PI_TOKEN}",
           "BUTLER_STATE_FILE": "/home/you/.config/butler/pi/instance.json",
           "BUTLER_WORKDIR": "/path/to/project"
         },
         "exposure": "direct",
         "description": "Butler sessions, messages, and the task board"
       }
     }
   }
   ```

   `pi mcp list` connects and prints the tools. Inside a session, `/mcp`
   shows the server. The state file path above is the plugin's default;
   `butler-mcp` and the plugin must share it. `env` on the MCP server is
   **not** the environment the extension sees.

3. Load the extension. Any one of these works:

   ```sh
   mkdir -p ~/.pi/agent/extensions
   ln -s /abs/path/to/butler/plugins/pi ~/.pi/agent/extensions/butler
   ```

   Pi discovers `~/.pi/agent/extensions/*/index.ts`. A trusted project's
   `.pi/extensions/butler` works the same way. Or set `extensions` in
   `~/.pi/agent/settings.json` (or the project's `.pi/settings.json`):

   ```json
   { "extensions": ["/abs/path/to/butler/plugins/pi"] }
   ```

   `pi -e /abs/path/to/butler/plugins/pi/index.ts` loads it for one run.
   Extensions under the auto-discovered directories can be reloaded with
   `/reload`.

4. Optional plugin config. Defaults are `~/.config/butler/pi/instance.json`,
   `~/.config/butler/pi/relay-state.json`, a 25s long-poll, and the startup
   sweep on. Override with `~/.config/butler/pi/plugin.json`:

   ```json
   {
     "stateFile": "/home/you/.config/butler/pi/instance.json",
     "relayStateFile": "/home/you/.config/butler/pi/relay-state.json",
     "waitTimeoutSecs": 25,
     "sweep": true
   }
   ```

   `~` is expanded. `BUTLER_PI_CONFIG` points at a different file. Env wins
   over the file: `BUTLER_STATE_FILE`, `BUTLER_RELAY_STATE_FILE`,
   `BUTLER_WAIT_TIMEOUT` (seconds, clamped to 1..60), `BUTLER_PI_SWEEP`
   (`0` / `false` / `no` / `off` disables the sweep). These are the Pi
   process's environment, not the MCP child's.

5. Start Pi in that project and leave it running. The footer shows
   `butler: live` once the state file is readable. A mention of this agent,
   or a task assigned to it, shows up as a user message and starts a turn
   when Pi is idle.

Do not run a second Pi (or a second `butler-mcp`) on the same kind, host, and
workdir while this one is registered. Registration revokes the previous
instance token. Two plugins on the same state file would also each inject
every mention.

## Tests

The delivery decisions (cursor clamp, pointer text, dedupe, followUp vs a
plain send) and a mocked long-poll are covered without Pi:

```sh
node --experimental-strip-types --test plugins/pi/relay.test.ts
```

## Deferred

- One Butler role per Pi session (sibling registration, title renames), the
  way the OpenCode plugin does it.
- Tool shadowing. Pi's MCP client is the one the model calls.
