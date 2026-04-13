# VoidMCP

One MCP to orchestrate them all.

VoidMCP is a standalone MCP server that gives your AI agent **Code Mode** - the ability to write and execute JavaScript that orchestrates multiple MCP tools in a single sandboxed execution. No round-trips, no token waste, no setup complexity.

Add MCPs at runtime. Search across tools. Let your agent write scripts that chain GitHub + Notion + Slack + anything with an MCP server.

Built by the team behind [VoidLLM](https://voidllm.ai).

## Why

When your AI agent has access to 10 MCP servers with 50+ tools, two problems emerge:

1. **Token waste** - every tool schema is sent to the LLM on every request
2. **Round-trip overhead** - chaining 5 tool calls means 5 LLM inference cycles

VoidMCP solves both. It exposes a single `execute_code` tool. The LLM writes a JavaScript script that calls multiple tools in one execution. One inference, one sandbox, multiple tool calls.

```javascript
// One script, three tool calls, zero round-trips
const issues = await tools.github.search_issues({ q: "is:open label:bug" });
for (const issue of issues.items.slice(0, 5)) {
  await tools.notion.create_page({ title: issue.title, content: issue.html_url });
}
await tools.slack.post_message({ channel: "#bugs", text: `Synced ${issues.items.length} issues` });
```

## Install

```bash
go install github.com/voidmind-io/voidmcp/cmd/voidmcp@latest
```

Or install the latest release binary:

macOS / Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/voidmind-io/voidmcp/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/voidmind-io/voidmcp/main/install.ps1 | iex
```

Manual downloads are also available on [Releases](https://github.com/voidmind-io/voidmcp/releases).

## Quick start

### Claude Code

```bash
claude mcp add --transport stdio voidmcp -- voidmcp serve --stdio
```

### Claude Desktop

Add to your `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "voidmcp": {
      "type": "stdio",
      "command": "voidmcp",
      "args": ["serve", "--stdio"]
    }
  }
}
```

### Cursor / Windsurf

Same config format. Point to the full voidmcp binary path if it's not in your PATH.

## Adding MCP servers

Once voidmcp is running, your AI agent can register HTTP MCP servers directly:

> "Add the weather MCP at https://weather.mcp.example.com"

For local stdio MCP servers (child processes), use the CLI:

```bash
voidmcp add filesystem "npx -y @modelcontextprotocol/server-filesystem /home/user/docs"
voidmcp add sqlite "uvx mcp-server-sqlite --db /tmp/test.db"
```

You can also manage servers via CLI:

```bash
voidmcp list              # show registered servers + tools
voidmcp remove filesystem # unregister a server
```

## Tools

VoidMCP exposes 5 tools to the LLM:

| Tool | Description |
|---|---|
| `add_mcp` | Register an HTTP MCP server (discovers its tools automatically) |
| `remove_mcp` | Unregister a server |
| `list_mcps` | List all registered servers with their tools and status |
| `search` | Find tools by keyword across all registered servers |
| `execute_code` | Run JavaScript in a WASM sandbox with access to all registered tools |

## How Code Mode works

The workflow is always: **search first, then execute**.

1. The LLM calls `search("your goal")` to discover relevant tools with full TypeScript signatures
2. The LLM calls `execute_code` with JavaScript that chains the discovered tools
3. VoidMCP injects tool bindings into a WASM-sandboxed QuickJS runtime
4. The script runs with `await` support, calling tools across multiple servers
5. VoidMCP returns the result, console logs, and a summary of all tool calls made

The sandbox has no access to the host filesystem, network, or environment. Tool calls go through a Go bridge that routes to the correct MCP transport.

### Schema inference

The first time a tool is called, VoidMCP captures the response and infers its return type. On subsequent `search` calls, the TypeScript definitions show concrete return types instead of `Promise<any>`:

```typescript
// Before first call:
function read_query(args: { query: string }): Promise<any>;

// After first call:
function read_query(args: { query: string }): Promise<Array<{ id: number; name: string; email: string }>>;
```

Inferred schemas are stored in SQLite and refresh after 7 days (configurable via `--schema-ttl`).

## HTTP mode

For shared or remote deployments:

```bash
voidmcp serve --port 8090
```

Binds to `127.0.0.1` by default. A bearer token is generated at startup and printed to stderr. Use `--host 0.0.0.0` to expose on the network, `--no-auth` to disable the token.

## Configuration

All settings are available as CLI flags:

```
voidmcp serve [flags]
  --stdio              Use stdio transport (default: HTTP)
  --port int           HTTP port (default: 8090)
  --host string        Bind address (default: 127.0.0.1)
  --no-auth            Disable bearer token authentication
  --db string          Database path (default: ~/.voidmcp/voidmcp.db)
  --pool-size int      WASM runtime pool size (default: 4)
  --memory int         Per-execution memory limit in MB (default: 16)
  --timeout duration   Per-execution timeout (default: 30s)
  --max-tool-calls int Maximum tool calls per execution (default: 50)
```

Registered servers and their tools persist in SQLite across restarts.

## Security

- **WASM sandbox**: JavaScript runs in QuickJS compiled to WebAssembly. No filesystem, network, or environment access. Runtimes are discarded after each execution.
- **Localhost only**: HTTP server binds to `127.0.0.1` by default
- **Bearer auth**: Random 256-bit token generated at startup for HTTP mode
- **No command injection via LLM**: The `add_mcp` tool only accepts HTTP URLs. Local stdio servers can only be registered via the CLI (direct user action). This prevents prompt injection from triggering arbitrary command execution.
- **Encrypted credentials**: Auth tokens stored with AES-256-GCM in SQLite. Encryption key at `~/.voidmcp/key` with 0600 permissions.
- **Restricted child env**: stdio MCP servers receive only PATH, HOME, TMPDIR, LANG from the parent environment

## Data storage

```
~/.voidmcp/
  voidmcp.db    SQLite database (registered servers, cached tool schemas)
  key           AES-256-GCM encryption key (auto-generated, chmod 0600)
```

## License

MIT

## Related

- [VoidLLM](https://voidllm.ai) - Privacy-first LLM proxy with built-in Code Mode for teams
