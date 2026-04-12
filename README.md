# voidmcp

One MCP to orchestrate them all.

voidmcp is a standalone MCP server that gives your AI agent **Code Mode** - the ability to write and execute JavaScript that orchestrates multiple MCP tools in a single sandboxed execution. No round-trips, no token waste, no setup complexity.

Add MCPs at runtime. Search across tools. Let your agent write scripts that chain GitHub + Notion + Slack + anything with an MCP server.

Built by the team behind [VoidLLM](https://voidllm.ai).

## Why

When your AI agent has access to 10 MCP servers with 50+ tools, two problems emerge:

1. **Token waste** - every tool schema is sent to the LLM on every request
2. **Round-trip overhead** - chaining 5 tool calls means 5 LLM inference cycles

voidmcp solves both. It exposes a single `execute_code` tool. The LLM writes a JavaScript script that calls multiple tools in one execution. One inference, one sandbox, multiple tool calls.

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

Or download a binary from [Releases](https://github.com/voidmind-io/voidmcp/releases).

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
      "command": "voidmcp",
      "args": ["serve", "--stdio"]
    }
  }
}
```

### Cursor / Windsurf

Same config format as Claude Desktop. Point to the voidmcp binary path.

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

voidmcp exposes 5 tools to the LLM:

| Tool | Description |
|---|---|
| `add_mcp` | Register an HTTP MCP server (discovers its tools automatically) |
| `remove_mcp` | Unregister a server |
| `list_mcps` | List all registered servers with their tools and status |
| `search` | Find tools by keyword across all registered servers |
| `execute_code` | Run JavaScript in a WASM sandbox with access to all registered tools |

## How Code Mode works

When the LLM calls `execute_code`, voidmcp:

1. Collects all tools from all registered MCP servers
2. Generates a JavaScript SDK with typed function stubs (`tools.github.create_issue(...)`)
3. Injects the SDK into a WASM-sandboxed QuickJS runtime
4. Executes the user's script with `await` support
5. Returns the result, console logs, and a summary of all tool calls made

The sandbox has no access to the host filesystem, network, or environment. Tool calls go through a Go bridge that routes to the correct MCP transport.

### Schema threshold

When you have few tools (default: 20 or fewer), full TypeScript definitions are embedded in the `execute_code` tool description. The LLM sees exactly what's available.

When you have many tools, voidmcp switches to a summary mode and instructs the LLM to use `search("your goal")` first. This keeps token usage flat regardless of how many tools are registered.

Configure the threshold:

```bash
voidmcp serve --stdio --schema-threshold 30   # inline up to 30 tools
voidmcp serve --stdio --schema-threshold 0    # always use search-first
voidmcp serve --stdio --schema-threshold -1   # always inline everything
```

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
  --schema-threshold int  Tools before switching to search-first mode (default: 20)
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
