# Connecting your editor

Your editor talks to Veska over MCP by launching **`veska-mcp`** — the thin
stdio shim that proxies JSON-RPC frames to the running daemon's Unix socket.
Point your MCP client at the **absolute path** to `bin/veska-mcp`.

!!! note "The daemon must be running"
    `veska-mcp` is only a proxy. Start the daemon first — see
    **[Running the daemon](running-the-daemon.md)**.

## Claude Desktop

`~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or
`%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "veska": {
      "command": "/abs/path/to/bin/veska-mcp"
    }
  }
}
```

## Cursor, Zed, Continue

**Cursor** (`~/.cursor/mcp.json`) and **Zed** (`~/.config/zed/settings.json`,
under `context_servers`) accept the same `command` shape as above.

**Continue** (`~/.continue/config.yaml`):

```yaml
mcpServers:
  - name: veska
    command: /abs/path/to/bin/veska-mcp
```

## Non-default `VESKA_HOME`

If your data root isn't `~/.veska`, pass it through so the shim finds the right
socket:

```json
{
  "mcpServers": {
    "veska": {
      "command": "/abs/path/to/bin/veska-mcp",
      "env": { "VESKA_HOME": "/path/to/veska/home" }
    }
  }
}
```

## Tell the agent the tools exist

`veska init --agent <name>` writes (or safely appends to) an agent-specific
instruction file in the current project, so the agent knows the Veska tool
surface is available. Supported: `claude`, `codex`, `copilot`, `cursor`,
`gemini`, `kiro`, `opencode`.

```sh
cd /path/to/your/repo
veska init --agent claude    # creates or updates CLAUDE.md
```

The snippet is bracketed with `<!-- veska:init -->` markers, so re-running
updates only the Veska section and leaves the rest of the file alone.

## Verify without an editor

You can drive `veska-mcp` straight from the shell — handy for debugging:

```sh
printf '{"jsonrpc":"2.0","id":1,"method":"eng_find_symbol","params":{"symbol":"Run"}}\n' \
  | ./bin/veska-mcp | jq '.result.nodes[0]'
```

See the **[MCP tools reference](../reference/mcp-tools.md)** for the full tool
surface.
