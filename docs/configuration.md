# Configuration

[← Docs index](README.md)

Everything lives in [`config.yaml`](../config.yaml). The OpenRouter key comes from the environment
(`OPENROUTER_KEY`, see [`.env.example`](../.env.example)) and is used both for Jev decisions and for the upstream
chat models.

## Top level

| Key | Default | Meaning |
|---|---|---|
| `listen` | `127.0.0.1:8787` | Gateway address |
| `log_path` | `data/decisions.jsonl` | [Event log](observability.md#event-log) |
| `upstream.base_url` | `https://openrouter.ai/api/v1` | Where chat requests go |
| `upstream.api_key_env` | `OPENROUTER_KEY` | Env var holding the upstream key |
| `upstream.refresh_prices` | `true` | Overwrite model prices with live OpenRouter prices at startup |

## `decision`: the System One model

| Key | Default | Meaning |
|---|---|---|
| `provider` | `jev` | `jev` \| `laya` \| `auto` (local for private requests, remote otherwise). See [decision providers](how-it-works.md#decision-providers) |
| `shadow` | `""` | e.g. `laya`: also ask it in the background and log agreement |
| `private` | `prefer_local` | `prefer_local` \| `local_only` \| `ignore` |
| `confidence_threshold` | `0.8` | Below this, the quality floor is raised one level |
| `risk_offset` | `-0.3` | Correction applied to the risk answer (bench: Jev scores risk ~+1 high on harmless tasks) |
| `providers.<name>` | | `url`, `model`, `api_key_env`, `local`, `max_state_chars`, `timeout` |

## `routing`: scoring and forwarding

| Key | Default | Meaning |
|---|---|---|
| `min_skill` | `[0.40, 0.55, 0.72, 0.86]` | Quality floor by complexity 0..3 |
| `risk_bonus` | `[0.0, 0.04, 0.08]` | Added to the floor by risk 0..2 |
| `est_output_tokens` | `[300, 800, 2000, 4000]` | Output size used in the cost estimate, by complexity |
| `load_penalty` | `0.25` | +25% effective cost per in-flight request on a model |
| `sticky_ttl` | `2h` | How long a conversation keeps its model |
| `fallback_model` | `anthropic/claude-sonnet-5` | Used when the decision call fails or times out |
| `explore` | `0` | Chance to try a cheaper model just below the floor. See [exploration](how-it-works.md#exploration) |
| `retries` | `2` | On 429/5xx, try the next-best models |
| `reasoning_effort` | `[low, low, medium, high]` | By complexity, only if the client didn't set one |
| `check.*` | disabled | See [Check and escalate](check-and-escalate.md) |

## `anthropic`

| Key | Default | Meaning |
|---|---|---|
| `auto_models` | `[]` | Globs routed like `auto`, e.g. `["claude-*"]` for clients with hard-coded model names. See [Anthropic Messages API](anthropic-messages.md) |

## `topics` and `models`

`topics` maps each topic id to a one-line description sent to the decision model.

Each entry in `models` has:

```yaml
- id: deepseek/deepseek-v4.1-flash
  price: {in: 0.04, out: 0.64}   # $ per million tokens (refreshed live if refresh_prices)
  context: 1048576
  tools: true
  vision: true
  default_skill: 0.60            # used for topics without an explicit skill
  skills: {chat: 0.80, docs: 0.70, code-gen: 0.70, debugging: 0.62}
  # optional:
  # output_multiplier: 3         # model that thinks a lot even on trivial prompts
  # daily_budget_usd: 10
  # base_url: http://127.0.0.1:11434/v1   # local OpenAI-compatible runtime (Ollama, LM Studio, mlx_lm.server)
  # local: true
  # anthropic: true              # the runtime also serves /messages; else skipped for /v1/messages
```

Skill values start as **seed guesses** (0..1), not measurements. Re-fit them from logged outcomes with
[`cmd/refit`](refit.md).
