# system-one-router documentation

[← Project README](../README.md)

| | Page | What's inside |
|---|---|---|
| 🚀 | [Getting started](getting-started.md) | Install, run, first requests, connecting clients |
| 🧠 | [How it works](how-it-works.md) | The 7-step pipeline, scoring, decision providers, exploration |
| ⚙️ | [Configuration](configuration.md) | Every `config.yaml` key |
| 🔌 | [HTTP API](api.md) | Endpoints, `X-Router-*` headers, feedback |
| 🅰️ | [Anthropic Messages API](anthropic-messages.md) | `/v1/messages`: routed models, privacy, metering, errors |
| 🤖 | [Claude Code & MCP](claude-code-mcp.md) | Use the router as Claude Code's provider, or as an MCP server |
| ✅ | [Check and escalate](check-and-escalate.md) | Judge the answer, retry once on a stronger model |
| 📊 | [Dashboard & event log](observability.md) | `/dashboard`, `decisions.jsonl` event kinds |
| 🔁 | [Re-fitting skills](refit.md) | `cmd/refit`: learn model skills from logged outcomes |
| 📏 | [Benchmark](benchmark.md) | Jev vs Laya on 80 labelled prompts |
| 🦙 | [Laya sidecar](laya-sidecar.md) | Local decision model, input padding, latency |
| 🏗️ | [Architecture & development](architecture.md) | Code layout, binaries, make targets |
| 🗺️ | [Roadmap](roadmap.md) | Done and next |
