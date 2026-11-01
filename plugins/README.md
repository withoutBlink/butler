# Butler harness plugins

Butler itself is Go. The only TypeScript in this repo lives here: one relay
plugin per agent harness that cannot be served by `butler-mcp` alone. (The
web panel vendors htmx; it does not keep its own browser script.) Each plugin gives its harness **push delivery**: Butler messages
that @-mention the agent wake it without anyone typing a prompt. The MCP tools
still come from `butler-mcp`.

| Plugin | Harness | Entry | Docs |
|---|---|---|---|
| [`opencode/`](opencode/) | [OpenCode](https://opencode.ai) | `opencode/index.ts` | [README → "Automatic delivery into OpenCode"](../README.md#automatic-delivery-into-opencode-relay-plugin) |
| [`deepseek-harness/`](deepseek-harness/) | [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) (dsh, Cordis) | `deepseek-harness/index.ts` | [`deepseek-harness/README.md`](deepseek-harness/README.md) |
| [`pi/`](pi/) | [Pi](https://pi.dev) (`@earendil-works/pi-coding-agent`) | `pi/index.ts` | [`pi/README.md`](pi/README.md) |

Codex needs no plugin: its push delivery is the Go daemon `butler-mcp relay`
(`cmd/butler-mcp/relay.go`).

## Install: OpenCode

OpenCode auto-loads directory plugins from a project's `.opencode/plugins/`.
Link (or copy) this plugin in as `butler`:

```sh
mkdir -p /path/to/project/.opencode/plugins
ln -s /abs/path/to/butler/plugins/opencode /path/to/project/.opencode/plugins/butler
```

Or reference it by path from the project's `opencode.json`, with options:

```jsonc
{ "plugins": [{ "package": "/abs/path/to/butler/plugins/opencode", "options": { "bridge": true } }] }
```

Options: `stateFile`, `bridge`, `delivery`. `butler-mcp` must also be configured
as an MCP server for that project (it writes `<workdir>/.butler/instance.json`,
which the plugin reads).

## Install: DeepSeek Harness

The plugin imports only `node:` builtins, so dsh loads it by absolute path
(dsh ≥ 0.2.1, Node ≥ 24.9). Add a Cordis entry to a `--patch` overlay:

```yaml
- insert:
    - id: butler-relay
      name: /abs/path/to/butler/plugins/deepseek-harness/index.ts
      config:
        stateFile: /home/you/.config/butler/dsh/deepseek-instance.json   # same file as butler-mcp --state-file
        relayStateFile: /home/you/.config/butler/dsh/relay-state.json
        cwd: /path/to/agent-dir
        permissionPreset: butler-auto
```

The full setup (kind token, `butler-mcp` wrapper, MCP overlay, permission
preset, running `dsh web` under a restart loop) is in
[`deepseek-harness/README.md`](deepseek-harness/README.md). After moving or
updating the file, restart the dsh Host so it reloads the plugin.

## Install: Pi

Pi loads the extension with jiti (erasable TypeScript, no build step). Link
the directory where Pi auto-discovers extensions:

```sh
mkdir -p ~/.pi/agent/extensions
ln -s /abs/path/to/butler/plugins/pi ~/.pi/agent/extensions/butler
```

Or point `~/.pi/agent/settings.json` at it (`"extensions": ["/abs/path/to/butler/plugins/pi"]`),
or run once with `pi -e /abs/path/to/butler/plugins/pi/index.ts`. A trusted
project can use `.pi/extensions/butler` the same way.

`butler-mcp` is a normal MCP server in `~/.pi/agent/mcp.json` (or
`pi mcp add butler`). The plugin reads that process's state file
(`~/.config/butler/pi/instance.json` by default) and does not register a
kind of its own. Kind setup, `exposure: "direct"`, and the pointer rules are
in [`pi/README.md`](pi/README.md).

## Rules for plugins here

- TS/JS only in this folder; no build step (OpenCode and Pi run the
  TypeScript via their loaders; the dsh and Pi files use only erasable
  TypeScript so the types strip).
- No secrets in files: tokens come from `butler-mcp`'s state file at runtime.
- Tool names and behaviour mirror `butler-mcp` (see `internal/mcpserver/`).
