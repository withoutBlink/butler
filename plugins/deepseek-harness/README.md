# Butler × DeepSeek Harness

`plugins/deepseek-harness/index.ts` is a [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)
(dsh, Cordis runtime) plugin that gives a long-lived dsh process the same push
delivery the OpenCode plugin (`plugins/opencode/`) gives OpenCode: the
agent handles Butler messages and tasks on its own, with nobody typing prompts.

Like the OpenCode plugin, it lives under `plugins/`, the one place TypeScript
lives in this repo (see `plugins/README.md`); Butler itself stays Go. The plugin imports no dsh packages (only `node:`
builtins and services dsh injects), so it loads by absolute path. Needs dsh
≥ 0.2.1 on Node ≥ 24.9; the file uses only erasable TypeScript.

## How it works

- dsh's own MCP client spawns `butler-mcp` (with `--state-file`), which
  registers the agent and gives the model the `mcp__butler__*` tools.
- The plugin reads that state file for the live identity and token, re-reads it
  on 401 (registration rotates the token), and long-polls
  `GET /api/v1/wait`. The cursor persists in `relayStateFile` and is clamped
  to `/api/v1/seq`.
- Messages that @-mention the agent, including task notices for assignment,
  handoff, reject and comment, become one short pointer per Butler session:
  `[butler] new messages in "<title>" (<sid>)` plus any task ids. The pointer
  also carries the house rules:
  - reply in the session;
  - for a task, in_progress → work → comment → handoff, and never set it done;
  - ask Noodle, not Andrew, for board decisions;
  - anything leaving the team needs Andrew.

  The pointer is queued with `agent.followup()` into a single dsh Session
  ("Butler bridge"). That Session is created on first start and resumed by id
  afterwards.
- Dedupe: one queued pointer per Butler session. A pointer counts as queued
  until dsh emits `agent/inbox/claimed` or `agent/inbox/discarded` for it.
- Startup sweep: the open (todo/in_progress) tasks assigned to the agent get
  one pointer, so work assigned while dsh was down is picked up.

Deferred (v1):
- per-dsh-Session Butler roles (sibling registration, title renames);
- per-session tool shadowing;
- routing to whichever Session the human is using.

v1 is one identity and one bridge Session.

## Setup

1. Mint a kind and set up the wrapper:
   - Run `./butler kind add deepseek` and store the kind token in a 0600 file.
   - Write a wrapper script that reads the file and execs
     `butler-mcp --server … --hostname … --workdir <agent dir> --state-file <file>`.
     dsh's MCP stdio bridge scrubs credential-looking env vars, so the wrapper
     has to read the token itself.
2. Add an overlay for the model route and the MCP server (`--patch`):

   ```yaml
   - insert:
       - id: butler-mcp
         name: '@deepseek-ai/dsh-mcp-client'
         config: { serverName: butler, transport: stdio, command: /path/to/butler-mcp-wrapper.sh, args: [], env: {}, cwd: /path/to/agent-dir }
   ```

3. Add a service overlay. The relay Session needs a permission preset that
   never asks, because nobody is there to approve:

   ```yaml
   - id: permission
     config:
       presets:
         read-only: { sandbox: read-only, approval: ask }
         workspace-write: { sandbox: workspace-write, approval: ask }
         danger-full-access: { sandbox: danger-full-access, approval: never }
         butler-auto: { sandbox: workspace-write, approval: never, name: Butler (unattended) }
   - id: sandbox-policy
     config:
       mode: workspace-write
       workspaceRoot: /path/to/agent-dir
       additionalWritableRoots: [~/.cache/go-build, ~/go, /tmp]   # e.g. for go test
   - insert:
       - id: butler-relay
         name: /abs/path/to/butler/plugins/deepseek-harness/index.ts
         config:
           stateFile: ~/.config/butler/dsh/deepseek-instance.json    # same file as --state-file
           relayStateFile: ~/.config/butler/dsh/relay-state.json
           cwd: /path/to/agent-dir
           permissionPreset: butler-auto
   ```

   Spell out the paths (`~` is shown for brevity). Config keys and their
   defaults are listed in the `Config` interface at the top of `index.ts`.
4. Run a long-lived Host:
   `dsh --profile web --no-open --host 127.0.0.1 --port 3080 --patch … --patch …`.
   Keep it under a restart loop, such as tmux plus a supervise script.
   - Set `DSH_TELEMETRY_MODE=DISABLED` if you don't want telemetry.
   - The web UI shows the "Butler bridge" Session, so you can watch or steer it.

Do not run a second dsh (for example `dsh --profile headless`) with the same
butler-mcp registration key while the service runs. Registration revokes the
previous instance token, and butler-mcp does not recover from that.
