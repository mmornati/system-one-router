# Architecture & development

[← Docs index](README.md)

## Layout

```
cmd/gateway       HTTP server
cmd/bench         decision benchmark (Jev / Laya) → JSON + HTML report
cmd/mcp           MCP server exposing route / delegate / feedback to agents
cmd/refit         re-fits config.yaml skills from logged checks and feedback
sidecar/          local Laya server (Decisions API shape)
internal/config   config.yaml loading, defaults and validation
internal/decision Decisions API client + provider selection (jev / laya / auto)
internal/router   request summary, privacy pre-check, scoring, answer check, sticky/load/budget state
internal/gateway  OpenAI + Anthropic Messages handlers, retry, streaming + cost metering, dashboard/stats
internal/upstream upstream client + live price refresh
internal/store    JSONL event log
internal/stats    aggregates decisions.jsonl for the dashboard
bench/            labelled cases (cases.json) + TypeScript Jev/LLM check
site/             published benchmark report (GitHub Pages)
docs/             this documentation
```

## Binaries

| Binary | Purpose | Docs |
|---|---|---|
| `cmd/gateway` | The HTTP server | [HTTP API](api.md) |
| `cmd/mcp` | MCP server | [Claude Code & MCP](claude-code-mcp.md) |
| `cmd/bench` | Decision benchmark | [Benchmark](benchmark.md) |
| `cmd/refit` | Re-fits `config.yaml` skills from the event log | [Re-fitting skills](refit.md) |
| `sidecar/laya_server.py` | Local Laya, same API shape as Jev | [Laya sidecar](laya-sidecar.md) |

## Make targets

| Target | Does |
|---|---|
| `make test` | `go vet` + `go test -race ./...` |
| `make build` | Builds `bin/gateway` and `bin/mcp` |
| `make gateway` | Runs the gateway |
| `make laya` | Local Laya sidecar on `:8788` (first run downloads ~3 GB of weights) |
| `make bench` | Decisions only, no chat model is called |
| `make refit` | Proposes skill updates (report only; add `ARGS="-write config.new.yaml"`) |
| `make site` | Publishes the newest benchmark report to `site/` (deployed by `.github/workflows/pages.yml`) |
