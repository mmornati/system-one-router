# system-one-router

[![CI](https://github.com/mmornati/system-one-router/actions/workflows/ci.yml/badge.svg)](https://github.com/mmornati/system-one-router/actions/workflows/ci.yml)
[Live benchmark report](https://mmornati.github.io/system-one-router/)

A fast "System One" decision model ([Jev](https://openrouter.ai/docs/guides/community/jev) or a local [Laya](https://huggingface.co/convaiinnovations/laya)) decides which "System Two" LLM should answer each prompt. It is the successor of [ai-dispatch](https://github.com/mmornati/ai-dispatch).

An OpenAI- and Anthropic-compatible gateway that picks the model for each task (`/v1/chat/completions` and `/v1/messages`, so Claude Code works too). Send `model: "auto"` and it will:

1. **Pre-check** the request locally: secrets/PII regexes, tools, images, size.
2. **Ask a decision model**, [Jev](https://openrouter.ai/docs/guides/community/jev) on OpenRouter or a local Laya helper, four typed questions in one call: main topic (with probabilities), complexity 0–3, risk 0–2, and private data yes/no.
3. **Score the models** in `config.yaml`, with no LLM involved:
   - skill = Σ P(topic) × the model's affinity for that topic;
   - quality floor = `min_skill[complexity] + risk_bonus[risk]`, raised one level when the decision model's confidence is low;
   - the cheapest model that clears the floor wins; the cost estimate includes an in-flight load penalty and daily budgets.
4. **Forward** to the chosen model, streaming or not. On a 429 or 5xx it tries the next candidates, and on the chat endpoint it sets `reasoning.effort` from the complexity.
5. **Check the answer** (optional, non-streaming only): one yes/no question to the decision model, "does the answer fully and correctly address the request?". If not, the request goes once to a stronger model and the client gets that answer instead.
6. **Keep the model** for the rest of the conversation. Switching models mid-conversation throws away the prompt cache.
7. **Log** every decision, check and piece of feedback to `data/decisions.jsonl`, for re-fitting the skills in `config.yaml`.

Requests naming any other model are passed through unchanged.

## What it exposes

HTTP endpoints (gateway, default `127.0.0.1:8787`):

| Endpoint | Purpose |
|---|---|
| `POST /v1/chat/completions` | OpenAI-compatible chat, routed when `model: "auto"` |
| `POST /v1/messages` | Anthropic Messages API, routed the same way (see below) |
| `POST /v1/messages/count_tokens` | Local token estimate (`chars/4`); OpenRouter doesn't serve this |
| `POST /route` | Dry-run the routing decision only — no model is called |
| `POST /feedback` | Rate a past request (`good`/`bad`) by its `X-Router-Request-Id` |
| `GET /v1/models` | Model catalog (Anthropic shape with an `anthropic-version` header) |
| `GET /stats` | Aggregated stats as JSON (`?days=N`, `0` = all time) |
| `GET /dashboard` | HTML dashboard over `/stats` |
| `GET /healthz` | Liveness check |

Response headers on routed requests: `X-Router-Model`, `X-Router-Reason`, `X-Router-Topic`, `X-Router-Complexity`, `X-Router-Risk`, `X-Router-Failed`, `X-Router-Request-Id`, and with `routing.check.enabled: true`, `X-Router-Checked` (P(answer ok), 2 decimals) and `X-Router-Escalated` (`<from>-><to>`).

MCP tools ([cmd/mcp](#claude-code--mcp)): `route` (dry-run decision), `delegate` (send a subtask to the picked model), `feedback` (rate a `route`/`delegate` result).

CLIs: `cmd/gateway` (the HTTP server above), `cmd/mcp` (MCP server), `cmd/bench` (decision benchmark), `cmd/refit` (re-fits `config.yaml` skills from the event log), `sidecar/laya_server.py` (local Laya, same API shape as Jev).

## Run

```bash
cp .env.example .env   # then put your OpenRouter key in it
go run ./cmd/gateway            # listens on 127.0.0.1:8787
```

```bash
curl -s localhost:8787/v1/chat/completions -d '{"model":"auto","messages":[{"role":"user","content":"hi"}]}' -D - 
curl -s localhost:8787/v1/messages -H 'anthropic-version: 2023-06-01' -d '{"model":"auto","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}' -D -
curl -s localhost:8787/route -d '{"messages":[{"role":"user","content":"Design a multi-region Postgres failover"}]}'   # dry run: decision only
```

Point clients at it with `OPENAI_BASE_URL=http://127.0.0.1:8787/v1`. For OpenCode, add a provider with that base URL and the model `auto`.

### Anthropic Messages API

`POST /v1/messages` takes the Anthropic Messages format (system as a string or blocks; text, image, tool_use,
tool_result and thinking blocks; tools; streaming) and forwards it unchanged, apart from `model`, to the upstream's
`/messages` endpoint. OpenRouter serves that endpoint for every model, not only Anthropic ones (checked live with
`qwen/qwen3.7-flash`, `deepseek/deepseek-v4.1-flash` and `openai/gpt-5.6-luna`), and reports `cost` in its usage.
Routing, retries, sticky conversations, budgets, response headers and metering are the same as for chat
completions; logged `chat` events carry `data.api: "anthropic"`.

- **Routed models:** `auto`, `router/auto`, an empty model, or a name matching one of `anthropic.auto_models`
  (globs). Any other model name is passed through unchanged, so it must be a valid upstream id
  (e.g. `anthropic/claude-sonnet-5`, not `claude-sonnet-5`).
  ```yaml
  anthropic:
    auto_models: ["claude-*"]   # default []: route Claude Code's hard-coded model names
  ```
  The trade-off: once a pattern matches, the client can no longer pick that model itself; everything matching is
  routed, including Claude Code's background calls (titles, summaries), which usually end up on the cheapest model.
- **What the router sees:** the system prompt; user turns are the text blocks of user messages. Tool results also
  come in user-role messages: they count as turns (so an agent's tool loop stays on its sticky model and is not
  re-decided on every step) and in the input size, but not as the user's words. Image blocks (also inside tool
  results) require a vision model.
- **Privacy:** the secret pre-check scans everything the model would read, tool results included (a `Read` of a
  `.env` file). Under `private: local_only`, a secret that appears mid-conversation also breaks the sticky model if
  it is remote: the request is re-routed to a local model or refused (same for the chat endpoint).
- **Local runtimes:** a model with its own `base_url` is skipped for `/v1/messages` (candidate reason
  `no Anthropic API`) unless it has `anthropic: true`, meaning the runtime serves `/messages` itself. There is no
  Anthropic-to-OpenAI translation, so a private request under `private: local_only` with no such local model gets 422.
- **Thinking:** unlike the chat endpoint's `reasoning.effort`, the gateway never adds `thinking`, since it constrains
  `max_tokens` and `temperature`. The client's own settings are passed through.
- **Answer check:** works for non-streaming requests. The answer is the concatenated text blocks, and turns that
  end in `tool_use` are not checked.
- **`count_tokens`:** OpenRouter does not serve it (404), so `POST /v1/messages/count_tokens` returns a local
  estimate, `{"input_tokens": chars/4}`, without calling anything.
- **Headers:** client credentials (`Authorization`, `x-api-key`) are never forwarded, on either endpoint. Only
  `Accept`, `HTTP-Referer`, `X-Title`, `anthropic-version` and `anthropic-beta` are.
- **Metering:** prompt tokens are `input_tokens` plus cache reads and writes. For streams, usage is read from the
  `message_start` and `message_delta` events (counters are cumulative: the largest value wins).
- **Errors** are in Anthropic's shape, `{"type":"error","error":{"type":…,"message":…}}`, including upstream errors
  that were not (OpenRouter's `{"error":{…}}`, an empty 429).

## Claude Code / MCP

### As a Claude Code model provider

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 ANTHROPIC_AUTH_TOKEN=dummy \
ANTHROPIC_MODEL=auto ANTHROPIC_SMALL_FAST_MODEL=auto claude
```

The token is not checked and never forwarded (the gateway uses its own OpenRouter key). Claude Code warns that
`auto` is not in its model catalog and assumes a 200k context window; set `CLAUDE_CODE_MAX_CONTEXT_TOKENS` if
the models you route to accept more. Instead of setting the model names, you can route Claude Code's own model
names with `anthropic.auto_models: ["claude-*"]` (see above). Claude Code sends ~17k tokens of tool definitions
on every call, so every candidate needs `tools: true`, and a routed request never goes below that input size.

### As an MCP server

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

## Check and escalate

With `routing.check.enabled: true`, a routed (`model: "auto"`) non-streaming answer is shown to the
decision model with one yes/no question: does it fully and correctly address the request? If
P(yes) < `threshold` (default 0.5), the request is re-sent once to a stronger model: the cheapest capable
candidate at least 0.05 more skilled than the first one (the quality floor is ignored here; context,
tools, vision, privacy and budget limits are not), else the most skilled one. The conversation then sticks
to that model. If the second call fails, the client gets the first answer.

```yaml
routing:
  check:
    enabled: true
    threshold: 0.5          # P(answer ok) below this escalates
    max_answer_chars: 3000  # answer trimmed to this (and to the provider's max_state_chars) in the state
    min_complexity: 0       # only check requests at least this complex (0..3)
```

It is skipped for streaming requests, sticky follow-ups, fallback routes, tool-call turns, empty answers,
non-200 responses, and when the model used is already the strongest capable one. Streaming is excluded
because the client already has the answer by the time it can be judged. The check uses the same
provider choice as routing, so a private request goes to the local provider when routing would use it,
and is not checked at all under `private: local_only` without one. An answer that trips the secret
pre-check counts as private too. Under `local_only`, a private request only escalates to a local model.

Cost: one extra decision call per checked answer (about $0.00004 with Jev, ~300 ms), plus a second model
call for the answers that fail.

## Dashboard

`GET /dashboard` is a single self-contained HTML page (no external JS) that fetches `/stats` and
renders it: request/spend/savings/shadow-agreement tiles, "escalated after check" (share of checked
answers re-sent to a stronger model) and "no model cleared the floor" (share of routed requests), a
stacked daily cost-per-model chart, a per-model table (cost, tokens, latency percentiles, errors),
routing-reason and topic/complexity/risk bars, and shadow-agreement, check-escalation and feedback
sections. An escalated request counts once in the request totals, but both calls count in spend and
in the per-model numbers. A day selector
(1 / 7 / 30 / all) re-fetches `/stats` with a different `days` value. It reads `data/decisions.jsonl`
each time it's called, so it always reflects the current log.

![Router dashboard](docs/dashboard.png)

## Event log

Every request to `/v1/chat/completions`, `/v1/messages` and `/route` gets a request id (12 random bytes, hex), returned
in `X-Router-Request-Id` and included in its logged events. Each line in `data/decisions.jsonl` is
`{"ts", "kind", "data"}`; `kind` is one of:

- `chat` — a forwarded request. `data.id`, `data.decision` (the full routing `Decision`, `null` for a
  pass-through request naming a model directly), `data.model`, `data.failed` (models that errored before
  this one), `data.status`, `data.cost_usd`, `data.prompt_tokens`, `data.completion_tokens`,
  `data.reasoning_tokens`, `data.latency_ms` (upstream round trip), `data.stream`, and `data.api: "anthropic"`
  for `/v1/messages` requests (absent for chat completions). An escalated request
  logs a second `chat` event with the same id and `data.escalated_from`.
- `check` — the answer check (see [Check and escalate](#check-and-escalate)): `data.id`, `data.model`
  (the model checked), `data.p_ok`, `data.passed`, `data.escalated_to` (`""` if none), `data.check_cost_usd`,
  `data.check_ms`, `data.provider`; or `data.id`, `data.model`, `data.error` when the check call failed.
- `route_dry` — a `POST /route` dry run: the `Decision` itself, including `data.id`.
- `refused` — a request the routing policy refused (private + `local_only`, no local model): the `Decision`.
- `shadow` — the background shadow-provider check (`decision.shadow` in config): `data.id`,
  `data.provider`, `data.primary`/`data.shadow` signals, `data.agree_topic`, `data.agree_complexity`.
- `feedback` — see below.

The `Decision` logged with `chat`, `route_dry` and `refused` carries `state`: the state map sent to the
decision model (`request`, `conversation_start`, `system_prompt`), kept as training data for re-fitting
skills. **`state` is omitted whenever the request is flagged private** (local
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

## Re-fitting skills

The `skills` in `config.yaml` start as guesses. `cmd/refit` re-estimates them from the outcomes in
`data/decisions.jsonl`. It only reads the log, and it only writes a file when you pass `-write`.

**Signals.** It uses the routed `chat` events (not sticky, not pass-through) that have an outcome:

| signal | label | weight (flag) |
|---|---|---|
| `check` event for that id + model (check-and-escalate) | `p_ok`, a soft label in 0..1 | 1 (`-w-check`) |
| `feedback` for that id | good = 1, bad = 0; on an escalated request it rates the model that answered last | 2 (`-w-feedback`) |
| model in `failed`, or status 429/5xx | availability, **not** quality: reported in a separate reliability table | – |
| other 4xx | skipped | – |

An answer counts toward every topic in proportion to its logged topic probabilities, so a
`{docs: 0.8, writing: 0.2}` answer adds 0.8 of an observation to docs and 0.2 to writing.
`N_EFF` is the sum of these weights.

**Method: compare with peers at the same difficulty.** Two biases make raw success rates misleading:
- The router only sends a model the prompts whose floor it clears, so a cheap model's success rate comes from
  easy prompts.
- Labels have no absolute scale. `p_ok` rarely gets near 1 and ratings skew to "bad", while a model well above
  its floor should succeed about 95% of the time. Read as absolute success rates, labels drag every skill down.

So a label is only compared with the other models' labels from the same source (check or feedback) at the
same difficulty, `d = min_skill[complexity] + risk_bonus[risk]` (the floor the answer was routed against):

```
E[label | skill s] = σ(logit(peer) + (s − seed) / scale)        peer = the other models' mean label there
```

- A model whose labels are as good as its peers' keeps its seed, whatever the absolute label level.
- Better or worse labels move it along a logistic curve: beating peers who already score 0.97 leaves little
  headroom and is weak evidence, while falling behind them is strong evidence.
- A difficulty level where no other model was seen carries no signal and does not count in `N_EFF`. The first
  lines of the report show how much of the data that is. Exploration (below) creates peers.
- The absolute level of the skills stays anchored to the seeds, because judge labels alone cannot identify it.

The seed skill from `config.yaml` is the prior: `-prior` (10) pseudo-observations, so with no data the fit is
exactly the seed. The fitted skill is the posterior mode, the root of one monotone equation, found by
bisection. `default_skill` is fitted the same way, from outcomes on the topics the model has no explicit skill
for, minus any topic that gets its own new skill in this run, so no outcome is counted twice.

Knobs (flags):
- `-scale 0.1`: being 0.1 of skill better multiplies the odds of a good label by e. The floors are about 0.15
  apart, so one complexity tier is worth about 4.5 times the odds.
- `-target 0.8`: the success rate expected at skill = difficulty. It shapes the prior's curvature.
- `-calibrate=false`: reads labels as absolute success rates, `σ((s − d)/scale + logit(target))`. It is kept for
  comparison, and it shows the downward bias.

A change is proposed only when `N_EFF ≥ -min-n` (20) and `|fitted − seed| ≥ -min-delta` (0.02).
`-write <path>` copies the config with those values changed. It edits the original text in place, so
comments, blank lines and order are kept, and new topics are appended to the model's `skills`.
`config.yaml` itself is only overwritten if you name it.

```bash
go run ./cmd/refit                                   # report; -days 30 to use recent events only
go run ./cmd/refit -write config.new.yaml && diff config.yaml config.new.yaml
make refit ARGS="-min-n 40 -prior 20"
```

```
320 chat events: 320 routed with signals, 300 with an outcome (300 checks, 19 feedback)

check    labels: n=300, mean 0.72 vs 0.91 predicted by the seeds; difficulty levels: 3, 20% of labels with no peer model
feedback labels: n=38, mean 0.68 vs 0.89 predicted by the seeds; difficulty levels: 1, 0% of labels with no peer model

MODEL                         TOPIC        N_EFF  SUCCESS  SEED  FITTED  DELTA
qwen/qwen3.7-flash            chat         4.0    71%      0.75  0.74    -0.01
qwen/qwen3.7-flash            code-gen     36.0   71%      0.45  0.43    -0.02  *
deepseek/deepseek-v4.1-flash  debugging    54.0   51%      0.62  0.53    -0.09  *
deepseek/deepseek-v4.1-flash  docs         48.0   78%      0.70  0.72    +0.02  *
deepseek/deepseek-v4.1-flash  writing      12.0   78%      0.65  0.66    +0.01
openai/gpt-5.6-luna           code-review  64.0   75%      0.70  0.80    +0.10  *
openai/gpt-5.6-luna           writing      60.0   76%      0.78  0.78    +0.00

Upstream reliability (429/5xx: availability, not counted against skill):
MODEL                         ATTEMPTS  FAILED  FAIL_RATE
deepseek/deepseek-v4.1-flash  103       3       2.9%
```

This is a synthetic log. Labels average 0.72 while the seeds predict 0.91, and still only relative
differences move skills:
- deepseek's debugging answers score below luna's code-review answers at the same difficulty, so deepseek's
  debugging skill goes down and luna's code-review skill goes up.
- qwen's chat row has almost no weight, because no other model answered those trivial prompts.

With `-calibrate=false`, every skill in this log except qwen/code-gen drops by 0.05 to 0.30.

The fit needs outcome data to mean anything. Without check-and-escalate (`check` events) or `POST /feedback`
ratings, the log holds only routing decisions, and refit reports "not enough data".

**Exploration.** A cheap model never sees prompts above its seed skill, so refit can lower its skill but
hardly raise it. `routing.explore` (default 0, off) is the probability of sending a request to the next-cheaper
capable model just below the floor, instead of the cheapest model that clears it. This happens only for
complexity ≤ 1, risk 0 and non-private requests, with reason `explore: …`. A value of 0.02–0.05 is enough to
collect data. When combined with check-and-escalate, a poor exploratory answer is escalated anyway.

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
| Topic accuracy | **87%** | 59% | 45% | 58% |
| Complexity within ±1 | 100% | 99% | 90% | 99% |
| Answers with confidence ≥ 0.8 | 85% (93% of them right) | 12% | 34% (52% right) | 19% |
| Calibration error (ECE) | **0.067** | 0.171 | 0.264 | 0.137 |
| Route = gold route | **73%** | 20% | 24% | 21% |
| Cheaper / pricier model than gold | 3 / 19 | 3 / 61 | 10 / 51 | 4 / 59 |
| Est. model cost (gold: $1.18; always Opus: $2.40) | $1.37 | $1.74 | $1.12 | $1.64 |
| Decision latency p50 | 366 ms | 304 ms (MPS) | 135 ms | 326 ms |
| Decision cost / 1k requests | $0.04 | $0 | $0 | $0 |

Reading it:
- **Jev is usable as-is.**
- **Laya out of the box is not.** It is rarely confident, so the router plays safe and bumps most requests to Sonnet/Opus. The routes end up more expensive than Jev's, not cheaper.
- **When Laya English is confident, it is right**, which is what makes it a candidate for further tuning once we have a fine-tuning path that doesn't depend on Jev's outputs (see [Roadmap](#roadmap)).
- **Latency:** Laya on MPS takes about 30–70 ms per call for a repeated input shape, but pays a one-off kernel-compile tax the first time a call uses a sequence length MPS hasn't seen yet (historically up to 200–350 ms). `sidecar/laya_server.py --pad-buckets` rounds every call's token length up to a fixed bucket so MPS only ever compiles a handful of shapes (warmed at `--preload` time), which is output-preserving (see below) - but **on the currently installed stack (torch 2.14, macOS 26, Apple M4) it measured no latency win, so it defaults to off (`--pad-buckets none`)**. Steady-state (second-pass) numbers on 40 varied-length prompts from `bench/cases.json`, English checkpoint, MPS:

  | | p50 | p95 |
  |---|---|---|
  | unpadded (default) | 100 ms | 663 ms |
  | padded, coarse buckets (64/128/256/512/1024) | 163 ms | 678 ms |
  | padded, fine buckets (every 32 to 512, every 64 to 1024) | 141 ms | 837 ms |

  Neither bucket set beats unpadded on p50 or p95: the shape-recompile tax on this stack is smaller than the extra attention compute padding spends on the padded positions, and finer buckets don't recover it either. Pass `--pad-buckets 64,128,256,512,1024` (or your own list) to opt in on a stack where the recompile tax is worse - it's exact, not approximate: on 12 varied prompts through the English checkpoint the padded vs. unpadded probabilities were bit-identical (max diff 0.0), since the attention mask zeroes out padded positions everywhere they reach the model (encoder attention and the decision head's `src_key_padding_mask`). The CPU is slower still (517 ms p50) and shows no shape-change penalty at all.

## Laya sidecar

`sidecar/laya_server.py` serves Laya (`pip install laya`, Apache-2.0, weights from Hugging Face `convaiinnovations/laya`) with the same request and response format as Jev's Decisions API, so the gateway uses it unchanged.

Models: `laya` (English, 512 tokens), `laya-multilingual` (1024 tokens, 100+ languages), and `laya-auto` (picks one by detected language).

```bash
uv venv --python 3.12 sidecar/.venv && uv pip install --python sidecar/.venv/bin/python laya==0.3.6
sidecar/.venv/bin/python sidecar/laya_server.py --preload [--device cpu|mps] [--pad-buckets 64,128,256,512,1024|none]
```

`--pad-buckets` (default `none`) optionally pads every call's tokenized sequence up to the smallest bucket that fits, so repeated calls reuse the same MPS shape instead of triggering a recompile. It's implemented as a small wrapper around `laya.agent.collate_items` (the function `Agent.system_one` uses to build the batch) that extends `input_ids`/`attention_mask` to the bucket length with zero-attention padding, so it doesn't require patching the `laya` package itself, and is output-preserving (verified bit-identical, see "Latency" above). It's off by default because it measured no latency win on the current torch/macOS stack - pass e.g. `--pad-buckets 64,128,256,512,1024` to opt in and measure on your own hardware; with `--preload`, each loaded checkpoint then also runs one warmup call per bucket at or under its `max_len`, so the first real request at any bucket size is already fast.

Note: `laya` 0.3.6 ships inference only — no training/fine-tuning API (see [Roadmap](#roadmap)).

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
cmd/refit         re-fits config.yaml skills from logged checks and feedback
sidecar/          local Laya server (Decisions API shape)
internal/decision Decisions API client + provider selection (jev / laya / auto)
internal/router   request summary, privacy pre-check, scoring, answer check, sticky/load/budget state
internal/gateway  OpenAI + Anthropic Messages handlers, retry, streaming + cost metering, dashboard/stats
internal/upstream upstream client + live price refresh
internal/store    JSONL event log
internal/stats    aggregates decisions.jsonl for the dashboard
bench/            labelled cases (cases.json) + TypeScript Jev/LLM check
site/             published benchmark report (GitHub Pages)
```

## Roadmap

- [x] Re-fit model skills from logged outcomes (`cmd/refit`: check results, user feedback; retries reported as reliability).
- [x] Check-and-escalate for non-streaming or background requests (a Jev yes/no on the answer).
- [x] Laya sidecar (Python, MPS).
- [x] Optional fixed-length padding for Laya inputs (`--pad-buckets`, off by default: measured no steady-state latency gain on torch 2.14 / macOS 26 MPS; opt in and re-measure on other hardware).
- [ ] ~~Fine-tune Laya on logged Jev decisions~~ — blocked: TypeSafe's terms ([MCA §2.3(b)](https://typesafe.ai/legal/mca)) forbid distilling Jev. Alternative: fine-tune on our own labels (bench gold labels, check/feedback outcomes).
- [x] Anthropic Messages API endpoint, so Claude Code-style clients can use the gateway.
- [x] MCP server exposing `route` / `delegate` to agents.
- [x] Dashboard over `decisions.jsonl` (cost per model, agreement, escalations).

## License

Apache-2.0. Laya weights are Apache-2.0 (Convai Innovations); Jev is a hosted TypeSafe model used through OpenRouter.
