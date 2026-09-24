# Getting started

[← Docs index](README.md)

## Requirements

- Go (see [`go.mod`](../go.mod) for the version)
- An [OpenRouter](https://openrouter.ai) key, used for Jev decisions and for the upstream chat models
- Optional: Python 3.12 + [uv](https://docs.astral.sh/uv/) for the local [Laya sidecar](laya-sidecar.md)

## Run the gateway

```bash
cp .env.example .env   # then put your OpenRouter key in it
go run ./cmd/gateway   # listens on 127.0.0.1:8787
```

## Try it

```bash
curl -s localhost:8787/v1/chat/completions -d '{"model":"auto","messages":[{"role":"user","content":"hi"}]}' -D -
curl -s localhost:8787/v1/messages -H 'anthropic-version: 2023-06-01' -d '{"model":"auto","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}' -D -
curl -s localhost:8787/route -d '{"messages":[{"role":"user","content":"Design a multi-region Postgres failover"}]}'   # dry run: decision only
```

The `X-Router-*` response headers tell you which model answered and why (see [HTTP API](api.md#response-headers)).
Open <http://127.0.0.1:8787/dashboard> to watch spend and routing (see [Dashboard](observability.md)).

## Connect a client

| Client | How |
|---|---|
| Any OpenAI SDK / tool | `OPENAI_BASE_URL=http://127.0.0.1:8787/v1`, model `auto` |
| OpenCode | Add a provider with that base URL and the model `auto` |
| Anthropic SDK / Claude Code | `ANTHROPIC_BASE_URL=http://127.0.0.1:8787`. See [Claude Code & MCP](claude-code-mcp.md) |
| Agents over MCP | `claude mcp add router -- go run ./cmd/mcp`. See [Claude Code & MCP](claude-code-mcp.md#as-an-mcp-server) |

## Next steps

- Tune the model list, prices and skills in [`config.yaml`](configuration.md).
- Turn on [check-and-escalate](check-and-escalate.md) to catch weak answers and to collect data for
  [re-fitting skills](refit.md).
- Keep private requests on your machine with the [Laya sidecar](laya-sidecar.md) and `decision.provider: auto`.
