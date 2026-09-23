# system-one-router

[![CI](https://github.com/mmornati/system-one-router/actions/workflows/ci.yml/badge.svg)](https://github.com/mmornati/system-one-router/actions/workflows/ci.yml)
[Live benchmark report](https://mmornati.github.io/system-one-router/)

A fast "System One" decision model ([Jev](https://openrouter.ai/docs/guides/community/jev) or a local [Laya](https://huggingface.co/convaiinnovations/laya)) decides which "System Two" LLM should answer each prompt. It is the successor of [ai-dispatch](https://github.com/mmornati/ai-dispatch).

An OpenAI-compatible gateway that picks the model for each task. Send `model: "auto"` and it will:

1. **Pre-check** the request locally: secrets/PII regexes, tools, images, size.
2. **Ask a decision model**, [Jev](https://openrouter.ai/docs/guides/community/jev) on OpenRouter or a local Laya helper, four typed questions in one call: main topic (with probabilities), complexity 0–3, risk 0–2, and private data yes/no.
3. **Score the models** in `config.yaml`, with no LLM involved:
   - skill = Σ P(topic) × the model's affinity for that topic;
   - quality floor = `min_skill[complexity] + risk_bonus[risk]`, raised one level when the decision model's confidence is low;
   - the cheapest model that clears the floor wins; the cost estimate includes an in-flight load penalty and daily budgets.
4. **Forward** to the chosen model, streaming or not. On a 429 or 5xx it tries the next candidates, and it sets `reasoning.effort` from the complexity.
5. **Keep the model** for the rest of the conversation. Switching models mid-conversation throws away the prompt cache.
6. **Log** every decision and its cost to `data/decisions.jsonl`, for re-fitting the skills and for fine-tuning Laya later.

Requests naming any other model are passed through unchanged.

## Run

```bash
cp .env.example .env   # then put your OpenRouter key in it
go run ./cmd/gateway            # listens on 127.0.0.1:8787
```

```bash
curl -s localhost:8787/v1/chat/completions -d '{"model":"auto","messages":[{"role":"user","content":"hi"}]}' -D - 
curl -s localhost:8787/route -d '{"messages":[{"role":"user","content":"Design a multi-region Postgres failover"}]}'   # dry run: decision only
```

Response headers: `X-Router-Model`, `X-Router-Reason`, `X-Router-Topic`, `X-Router-Complexity`, `X-Router-Risk`, `X-Router-Failed`, `X-Router-Request-Id`.

Point clients at it with `OPENAI_BASE_URL=http://127.0.0.1:8787/v1`. For OpenCode, add a provider with that base URL and the model `auto`.

## Benchmark

`cmd/bench` runs the 80 labelled prompts in `bench/cases.json` through each decision provider. Groups: core dev work, multilingual, inputs longer than 512 tokens, tricky/ambiguous, private data. It records:
- the decision: topic + confidence, complexity, risk, private-data probability;
- the model the router would pick;
- the model the router would pick from the human labels ("gold").

**No prompt is sent to a chat model.** Output goes to `bench/results/bench-*.json` and `bench-*.html` (`latest.html` links to the newest). `make site` copies the newest report to `site/`, which GitHub Pages publishes.

```bash
sidecar/.venv/bin/python sidecar/laya_server.py --preload &    # local Laya on :8788 (Apple GPU / MPS)
go run ./cmd/bench                                             # jev,laya,laya-multilingual,laya-auto
go run ./cmd/bench -providers jev                              # Jev only
go test -race ./...
node --env-file=.env bench/jev-check.ts --baseline             # raw Jev vs an LLM router (same cases)
```

Results from 2026-09-23 (80 prompts, M4 16 GB for Laya):

| | Jev 1.13 | Laya English | Laya multilingual | Laya auto |
|---|---|---|---|---|
| Topic accuracy | **89%** | 59% | 45% | 58% |
| Complexity within ±1 | 100% | 99% | 90% | 99% |
| Answers with confidence ≥ 0.8 | 82% (96% of them right) | 12% | 34% (52% right) | 19% |
| Calibration error (ECE) | **0.080** | 0.171 | 0.264 | 0.137 |
| Route = gold route | **70%** | 20% | 24% | 21% |
| Cheaper / pricier model than gold | 5 / 19 | 3 / 61 | 10 / 51 | 4 / 59 |
| Est. model cost (gold: $1.18; always Opus: $2.35) | $1.35 | $1.74 | $1.12 | $1.64 |
| Decision latency p50 | 308 ms | 287 ms (MPS) | 122 ms | 287 ms |
| Decision cost / 1k requests | $0.04 | $0 | $0 | $0 |

Reading it:
- **Jev is usable as-is.**
- **Laya out of the box is not.** It is rarely confident, so the router plays safe and bumps most requests to Sonnet/Opus. The routes end up more expensive than Jev's, not cheaper.
- **When Laya English is confident, it is right**, which is what makes it a candidate for fine-tuning on Jev-labelled traffic (shadow mode).
- **Latency:** Laya on MPS takes about 30–70 ms per call for a repeated input shape, but 200–350 ms when the sequence length changes. The CPU is slower still (517 ms p50).

## Laya sidecar

`sidecar/laya_server.py` serves Laya (`pip install laya`, Apache-2.0, weights from Hugging Face `convaiinnovations/laya`) with the same request and response format as Jev's Decisions API, so the gateway uses it unchanged.

Models: `laya` (English, 512 tokens), `laya-multilingual` (1024 tokens, 100+ languages), and `laya-auto` (picks one by detected language).

```bash
uv venv --python 3.12 sidecar/.venv && uv pip install --python sidecar/.venv/bin/python laya==0.3.6
sidecar/.venv/bin/python sidecar/laya_server.py --preload [--device cpu|mps]
```

## Decision providers

`decision.provider`:
- `jev`: always use Jev.
- `laya`: always use Laya.
- `auto`: use the local provider for requests the pre-check flags as private, Jev otherwise.

`private: local_only` never sends a private request off the machine; with no local option it answers 422.

`shadow: laya` asks a second provider in the background and logs whether it agrees. Use it to decide when Laya is good enough.

Any local service that accepts `POST {model, state, questions}` and returns `{answers, usage}` works unchanged; see the Laya sidecar above. Laya's English checkpoint sees only 512 tokens, so set `max_state_chars` to about 1500.

## Layout

```
cmd/gateway       HTTP server
cmd/bench         decision benchmark (Jev / Laya) → JSON + HTML report
cmd/mcp           MCP server exposing route / delegate / feedback to agents
sidecar/          local Laya server (Decisions API shape)
internal/decision Decisions API client + provider selection (jev / laya / auto)
internal/router   request summary, privacy pre-check, scoring, sticky/load/budget state
internal/gateway  OpenAI-compatible handlers, retry, streaming + cost metering
internal/upstream upstream client + live price refresh
internal/store    JSONL event log
bench/            labelled cases (cases.json) + TypeScript Jev/LLM check
site/             published benchmark report (GitHub Pages)
```

## Event log

Every request to `/v1/chat/completions` and `/route` gets a request id (12 random bytes, hex), returned
in `X-Router-Request-Id` and included in its logged events. Each line in `data/decisions.jsonl` is
`{"ts", "kind", "data"}`; `kind` is one of:

- `chat` — a forwarded request. `data.id`, `data.decision` (the full routing `Decision`, `null` for a
  pass-through request naming a model directly), `data.model`, `data.failed` (models that errored before
  this one), `data.status`, `data.cost_usd`, `data.prompt_tokens`, `data.completion_tokens`,
  `data.reasoning_tokens`, `data.latency_ms` (upstream round trip), `data.stream`.
- `route_dry` — a `POST /route` dry run: the `Decision` itself, including `data.id`.
- `refused` — a request the routing policy refused (private + `local_only`, no local model): the `Decision`.
- `shadow` — the background shadow-provider check (`decision.shadow` in config): `data.id`,
  `data.provider`, `data.primary`/`data.shadow` signals, `data.agree_topic`, `data.agree_complexity`.
- `feedback` — see below.

The `Decision` logged with `chat`, `route_dry` and `refused` carries `state`: the state map sent to the
decision model (`request`, `conversation_start`, `system_prompt`), kept as training data for re-fitting
skills and fine-tuning Laya. **`state` is omitted whenever the request is flagged private** (local
pre-check or the decision model's own `private_data` answer), so secrets never end up in the log twice —
though note the log otherwise contains prompts and responses' cost/token metadata, not the responses
themselves.

### Feedback

`POST /feedback` records a rating against a request id, as a `feedback` event (`data.id`, `data.rating`,
`data.comment`). `rating` must be `"good"` or `"bad"`; the id isn't checked against the log.

```bash
curl -s localhost:8787/feedback -d '{"id":"<X-Router-Request-Id>","rating":"bad","comment":"picked a model too weak for this"}'
# 204 No Content
```

## MCP server

`cmd/mcp` is a thin client that exposes a running gateway to agents (Claude Code and others) over
[MCP](https://modelcontextprotocol.io), so an agent can route or delegate work without shelling out to `curl`.

Tools:

- `route` — dry-run the decision for a prompt (model, reason, topic, confidence, complexity, risk,
  private, required skill, top 3 candidates). No model is called.
- `delegate` — send a self-contained subtask (summary, boilerplate, docs, simple code) to the model
  the router picks, and get the answer back as text. Cheaper than doing it in the calling agent.
- `feedback` — rate a `route`/`delegate` result (`good`/`bad`, by its `request_id`) for later skill re-fitting.

It talks to the gateway over HTTP (`-gateway`/`ROUTER_URL`, default `http://127.0.0.1:8787`); start the
gateway first.

```bash
go run ./cmd/mcp                 # stdio (default), for launching from an agent
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

## License

Apache-2.0. Laya weights are Apache-2.0 (Convai Innovations); Jev is a hosted TypeSafe model used through OpenRouter.

## Roadmap

- [ ] Re-fit model skills from logged outcomes (retries, check failures, user feedback).
- [ ] Check-and-escalate for non-streaming or background requests (a Jev yes/no on the answer).
- [x] Laya sidecar (Python, MPS).
- [ ] Fine-tune Laya on logged Jev decisions (check Jev's terms first); pad inputs to fixed lengths to avoid MPS recompiles.
- [ ] Anthropic Messages API endpoint, so Claude Code-style clients can use the gateway.
- [x] MCP server exposing `route` / `delegate` to agents.
- [ ] Dashboard over `decisions.jsonl` (cost per model, agreement, escalations).
