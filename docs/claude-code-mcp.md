# Claude Code & MCP

[← Docs index](README.md)

There are two ways to use the router from Claude Code: as its **model provider** (every call is routed), or as an
**MCP server** (the agent decides when to route or delegate a subtask).

## As a Claude Code model provider

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ANTHROPIC_AUTH_TOKEN=dummy \
ANTHROPIC_MODEL=auto ANTHROPIC_SMALL_FAST_MODEL=auto claude
```

- The token is not checked and never forwarded (the gateway uses its own OpenRouter key).
- Claude Code warns that `auto` is not in its model catalog and assumes a 200k context window; set
  `CLAUDE_CODE_MAX_CONTEXT_TOKENS` if the models you route to accept more.
- Instead of setting the model names, you can route Claude Code's own model names with
  `anthropic.auto_models: ["claude-*"]` (see [Anthropic Messages API](anthropic-messages.md#routed-models)).
- Claude Code sends ~17k tokens of tool definitions on every call, so every candidate needs `tools: true`, and a
  routed request never goes below that input size.

## As an MCP server

`cmd/mcp` is a thin client that exposes a running gateway to agents (Claude Code and others) over
[MCP](https://modelcontextprotocol.io), so an agent can route or delegate work without shelling out to `curl`.

| Tool | What it does |
|---|---|
| `route` | Dry-run the decision for a prompt (model, reason, topic, confidence, complexity, risk, private, required skill, top 3 candidates). No model is called. |
| `delegate` | Send a self-contained subtask (summary, boilerplate, docs, simple code) to the model the router picks, and get the answer back as text. Cheaper than doing it in the calling agent. |
| `feedback` | Rate a `route`/`delegate` result (`good`/`bad`, by its `request_id`) for later [skill re-fitting](refit.md). |

It talks to the gateway over HTTP (`-gateway`/`ROUTER_URL`, default `http://127.0.0.1:8787`); start the gateway
first.

```bash
go run ./cmd/mcp                        # stdio (default), for launching from an agent
go run ./cmd/mcp -http 127.0.0.1:8790   # streamable HTTP instead (no auth: keep it on localhost)
```

Register it with Claude Code:

```bash
claude mcp add router -- go run ./cmd/mcp
# or, after `make build`:
claude mcp add router -- /path/to/bin/mcp
```

Generic `mcpServers` config (Claude Desktop, other MCP clients):

```json
{
  "mcpServers": {
    "router": {
      "command": "/path/to/bin/mcp",
      "env": { "ROUTER_URL": "http://127.0.0.1:8787" }
    }
  }
}
```
