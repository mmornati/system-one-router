# Benchmark

[← Docs index](README.md) · [Live report](https://mmornati.github.io/system-one-router/)

`cmd/bench` runs the 80 labelled prompts in `bench/cases.json` through each decision provider. Groups: core dev
work, multilingual, inputs longer than 512 tokens, tricky/ambiguous, private data. It records:

- the decision: topic + confidence, complexity, risk, private-data probability;
- the model the router would pick;
- the model the router would pick from the human labels ("gold").

**No prompt is sent to a chat model.** Output goes to `bench/results/bench-*.json` and `bench-*.html`
(`latest.html` links to the newest). `make site` copies the newest report to `site/`, which GitHub Pages publishes.

```bash
sidecar/.venv/bin/python sidecar/laya_server.py --preload &    # local Laya on :8788 (Apple GPU / MPS)
go run ./cmd/bench                                             # jev,laya,laya-multilingual,laya-auto
go run ./cmd/bench -providers jev                              # Jev only
go run ./cmd/bench -providers jev,laya-typed-decisions,von,kev # other open decision models (servers below)
go run ./cmd/bench -providers jev,kev -temperature kev=0.47    # with a confidence temperature
go run ./cmd/bench -providers jev,clef-flash                   # Clef-flash (server below)
go run ./cmd/bench -providers jev,vega                         # VegaML (sidecar below)
go test -race ./...
node --env-file=.env bench/jev-check.ts --baseline             # raw Jev vs an LLM router (same cases)
```

Von and Kev run as their own local servers, which speak the same `/v1/systemone` request/response shape (no
adapter needed):

```bash
uv pip install "von-sdk>=1.3.7" && von serve --host 127.0.0.1 --port 8790 --device mps --noul-decision raw
git clone https://github.com/jaredpalmer/kev && cd kev && uv sync --extra serve \
  && uv run --extra serve python -m kev.serve --run jaredpalmer/kev-0.8b --host 127.0.0.1 --port 8791
```

Clef-flash runs from the [`mlx-community/clef-flash-4bit`](https://huggingface.co/mlx-community/clef-flash-4bit)
conversion, which ships the joint schema head and an MLX loader with a `/v1/systemone` server. The GGUF builds and
`mlx_lm`/LM Studio load only the Qwen backbone and produce meaningless output.

```bash
uv venv -p 3.12 clef-venv && uv pip install -p clef-venv/bin/python "mlx==0.32.3" "mlx-lm>=0.32,<0.33" \
  "mlx-vlm>=0.7.4,<0.8" huggingface_hub pillow
SNAP=$(clef-venv/bin/python -c "from huggingface_hub import snapshot_download as d; \
  print(d('mlx-community/clef-flash-4bit', ignore_patterns=['__pycache__/*']))")
clef-venv/bin/python $SNAP/clef_mlx.py serve --model $SNAP --name clef-flash --port 8792 --quiet
```

VegaML has a Python API only, so `sidecar/vega_server.py` wraps it in the same `/v1/systemone` shape (`noul` ↔
`boolean`, `expected_score` → `score`; VegaML's own `abstain`, `prediction_set` and `unbound` fields are passed
through):

```bash
uv venv --python 3.12 sidecar/.venv-vega && uv pip install --python sidecar/.venv-vega/bin/python vegaml==0.8.0
sidecar/.venv-vega/bin/python sidecar/vega_server.py                      # :8793, zero-shot (mode "engine")
sidecar/.venv-vega/bin/python sidecar/vega_server.py --mode auto --fit examples.jsonl   # with fitted heads
```

## Results

2026-10-02, 80 prompts, M4 16 GB for the local models, `laya` 0.3.24 (re-run on 0.4.2: identical decisions):

| | Jev 1.13 | Laya English | Laya multilingual | Laya auto | Laya typed-decisions | Von 1.3 | Kev-0.8B |
|---|---|---|---|---|---|---|---|
| Topic accuracy | **89%** | 59% | 45% | 56% | 71% | 70% | 81% |
| Complexity exact / within ±1 | **72%** / 100% | 45% / 99% | 39% / 90% | 45% / 99% | 41% / 92% | 55% / 94% | 42% / 98% |
| Risk exact | **61%** | 31% | 26% | 30% | 34% | 46% | 49% |
| Answers with confidence ≥ 0.8 | 84% (94% right) | 12% | 34% (52% right) | 19% | 0% | 74% (75% right) | 5% |
| Calibration error (ECE) | **0.068** | 0.171 | 0.264 | 0.140 | 0.509 | 0.226 | 0.349 |
| Route = gold route | **72%** | 20% | 24% | 21% | 12% | 48% | 22% |
| Cheaper / pricier model than gold | 3 / 19 | 3 / 61 | 10 / 51 | 4 / 59 | 0 / 70 | 15 / 27 | 4 / 58 |
| Est. model cost (gold: $1.18; always Opus: $2.40) | $1.37 | $1.74 | $1.12 | $1.64 | $2.32 | $0.99 | $1.16 |
| Decision latency p50 | 268 ms | 300 ms | 126 ms | 300 ms | 334 ms | 216 ms | 371 ms (MLX) |
| Decision cost / 1k requests | $0.04 | $0 | $0 | $0 | $0 | $0 | $0 |

### Laya 0.3.24 vs 0.3.6

The 18 releases since the first benchmark (2026-09-23) are runtime-only: MPS autocast, tokenizer reuse, length
grouping, language-routing fixes, temperature tooling. The three checkpoints' `model.safetensors` and
`rl_agent_config.json` are byte-identical. The numbers match: English 59% → 59% topic, multilingual 45% → 45%,
auto 58% → 56% (one prompt), ECE within 0.003, latency within 10 ms. The upgrade is safe and changes nothing
for routing.

### Laya 0.4.2 vs 0.3.24

2026-10-11, the same four checkpoints on `laya` 0.4.2 (eight releases). The weights on Hugging Face have not
changed since 2026-09-19, so this was a check that the runtime still gives the same answers, not a new benchmark.
It does: every topic, complexity level, risk level and topic confidence is **identical** to the 0.3.24 run, on all
80 prompts and all four checkpoints (largest confidence difference: 0.0000).

What changed upstream and why it does not reach us:

- **0.4.0 (breaking):** `laya.Router` now sends text whose language it cannot detect to the multilingual checkpoint
  instead of the English one. The sidecar does its own `laya-auto` routing with `laya.detect_language()` and does not
  use `Router`.
- `detect_language` fixes (English acronyms read as French, English/foreign collision words): no prompt in the bench
  is affected.
- Histogram-binning recalibration, stricter `min_confidence` maps, an opt-in order-invariant option layout: no
  published checkpoint ships them.
- New: `laya-train` (fine-tuning CLI with abstention fitting and before/after reports), `LAYA_EXTRA_MODELS`, idle
  unload, Java and .NET SDKs. `laya-train` is the useful one for the [Roadmap](roadmap.md) fine-tuning item.

Some routed models differ from the 2026-10-02 run (6 to 19 prompts per checkpoint) even though the decisions are
identical. That is OpenRouter pricing: DeepSeek V4.1 Flash went from $0.02/$0.60 to $0.30/$1.20 per million tokens,
so GPT-5.6 Luna now wins those slots, for every provider and for the gold route too.

### Other decision models

Many open "Jev-like" models have appeared since Jev launched. We picked the ones that are Apache-2.0, run on a
16 GB Mac, and say they were not trained on Jev outputs (TypeSafe's terms forbid distilling Jev):

- **Laya typed-decisions**: a third checkpoint in the same `convaiinnovations/laya` repo, fine-tuned on the
  [typed-decisions](https://huggingface.co/datasets/LocalLLaMA/typed-decisions) workflows. Served by the sidecar as
  `laya-typed-decisions`.
- **[Von 1.3](https://huggingface.co/wfzyx/von)**: 395M ModernBERT-large with an option-marker head, order-invariant
  options, 8k context, English only.
- **[Kev-0.8B](https://huggingface.co/jaredpalmer/kev-0.8b)**: LoRA plus pointer head on Qwen3.5-0.8B-Base, served
  through MLX. Kev-4B is the recommended size but needs a 32 GB Mac.
- **[Clef-flash](https://huggingface.co/Cloudflare/clef-flash)** (Cloudflare, 2026-10-01): Qwen3.5-9B backbone plus a
  joint schema head, 64k context, vision encoder, Jev-compatible API. Run as the 4-bit MLX conversion (6.2 GB, ~7 GB
  peak). The model card does not say whether Jev outputs were in the training data, so check before relying on it.
  The 27B **Clef** needs 17 GB or more even at 4-bit and was not run.
- **[VegaML 0.8B](https://github.com/NandhaKishorM/vegaml)** (by Laya's author, 2026-10-09): a *frozen*
  Qwen3.5-0.8B read once per question, plus a 14M-parameter "physics engine" that turns the pooled features into a
  calibrated choice, score or boolean, with conformal sets and an abstain flag. It can also fit small per-question
  heads from a few labelled examples (`fit`). Apache-2.0. The model card compares against Jev but does not say whether
  Jev outputs were in the training data. The 4B runs in fp32 only (~16 GB) and was not run.

Not benchmarked: OpenJev (CC-BY-NC, ~15 GB even at 4-bit), `autotrust/JEV-*` (trained on a Jev-distilled corpus),
and smaller unreviewed community checkpoints.

### Clef-flash

2026-10-04, same 80 prompts, Jev re-run in the same session for reference, Clef-flash 4-bit on the M4 16 GB:

| | Jev 1.13 | Clef-flash 9B (MLX 4-bit) |
|---|---|---|
| Topic accuracy | 89% | **90%** |
| Topic: core / multilingual / long / tricky | 95% / 88% / 75% / 43% | 89% / **100%** / **100%** / **71%** |
| Complexity exact / within ±1 | **70%** / 100% | 64% / 100% |
| Risk exact | 61% | **76%** |
| Answers above the confidence threshold | 86% (93% right) | 56% (98% right) |
| Calibration error (ECE) | **0.080** | 0.101 |
| Route = gold route | **75%** | 52.5% |
| Cheaper / pricier model than gold | 3 / 17 | **0** / 38 |
| Est. model cost (gold: $1.19; always Opus: $2.35) | **$1.34** | $1.57 |
| Decision latency p50 / p90 | **258 / 332 ms** | 3,854 / 5,477 ms |
| Decision cost / 1k requests | $0.04 | $0 |

Clef-flash agrees with Jev on the topic 85% of the time and on the routed model 60% of the time.

Latency is set by the hardware. A router request is about 750 tokens (4 questions, 19 options), and the M4's GPU
prefills that in about 3.6 s. A 128-token request takes about 0.57 s. The model card's 0.31 s at 1k tokens was
measured on an M5 Max. Cloudflare's 39 ms median is on Workers AI.

### VegaML

2026-10-11, same 80 prompts, M4 16 GB, `vegaml` 0.8.0 on MPS (fp32), Jev re-run in the same session.

| | Jev 1.13 | VegaML zero-shot | VegaML zero-shot, T=0.60 | VegaML few-shot heads¹ | few-shot, T=0.72¹ |
|---|---|---|---|---|---|
| Topic accuracy | **89%** | 51% | 51% | 65% | 65% |
| Topic: core / multilingual / long / tricky | 95% / 88% / 75% / 43% | 56% / 62% / 75% / 0% | same | 63% / 62% / 100% / 57% | same |
| Complexity exact / within ±1 | **71%** / 100% | 42.5% / 94% | same | 54% / 96% | same |
| Risk exact | 61% | 54% | 54% | **64%** | **64%** |
| Answers with confidence ≥ 0.8 | 84% (94% right) | 1% | 19% (93% right) | 10% (88% right) | 24% (84% right) |
| Calibration error (ECE) | **0.074** | 0.162 | 0.092 | 0.186 | 0.138 |
| Route = gold route | **70%** | 17.5% | 20% | 39% | 42.5% |
| Cheaper / pricier model than gold | 4 / 20 | 7 / 59 | 11 / 53 | 2 / 47 | 6 / 40 |
| Decision latency p50 / p90 | **364 / 436 ms** | 1,344 / 1,703 ms | same | 862 / 977 ms | same |

¹ Held-out: the prompts were split into two topic-stratified halves; heads fitted on one half (`--fit`, 40 labelled
prompts, about 1 minute) answered the other, and the two halves are pooled. `private_data` got a head on one half
only (too few private examples on the other), so the engine answered it there. The T=0.72 column uses a temperature
fitted on those pooled held-out answers, so it is in-sample for T, like the "full bench" rows below.

- **Zero-shot it does not fit this task.** 51% topic is below every open model here except Laya multilingual (45%), and 0% on the
  tricky group. Its published wins (phishing, spam, news topic) are binary or few-class screening tasks; ours is a 10-way
  topic choice with overlapping categories.
- **Its uncertainty signals do not help the router.** It answers with confidence ≥ 0.8 once in 80 prompts.
  `abstain` flags 72 of 80 topic answers (47% right; the 8 it keeps are 88% right), which is what the confidence
  threshold already says. `unbound` carries no signal (51% vs 52% right), and the α = 0.1 conformal sets hold
  one option and cover the gold topic 54% of the time instead of 90%: the shipped calibration does not transfer to
  our questions.
- **Few-shot heads are the interesting part.** 40 labelled prompts take topic from 51% to 65% and gold-route match
  from 17.5% to 39% on prompts the heads never saw, and risk (64%) beats Jev (61%). No other model here can do that
  without a training run, and `cmd/refit`'s logged outcomes could feed it. It is still well short of Jev and of
  Kev-0.8B's 81% zero-shot topic.
- **Latency:** four questions are four passes through the backbone (prefix sharing starts at 4,096 tokens), about
  1.3 s on the M4 zero-shot and 0.9 s with heads (a head replaces the engine for that question). The card's 280 ms is
  per decision; a router request is four.

### Recalibrating confidence

The router raises the quality floor when topic confidence is below `confidence_threshold`, so a model that is right
but unsure still over-provisions. `providers.<name>.temperature` rescales choice probabilities to fix that. We fit
T by minimising log-loss on half of the prompts and measured on the other half (20 random 2-fold splits):

| | Kev-0.8B | Laya typed-decisions | Von 1.3 | Clef-flash | VegaML (zero-shot) |
|---|---|---|---|---|---|
| Fitted T (cross-validated median) | 0.47 | 0.43 | 2.47 | 0.64 | 0.60 |
| Confident ≥ 0.8, held-out | 5% → 60% (89% right) | 0% → 33% (92% right) | 75% → 26% (69% right) | 56% → 81% (95% right) | 1% → 16% (93% right) |
| Route = gold route with that T (full bench) | 22% → 39% | 12% → 25% | 48% → 36% | 52.5% → **64%** | 17.5% → 20% |
| ECE with that T (full bench, in-sample) | 0.349 → **0.059** | 0.509 → 0.072 | 0.226 → 0.236 | 0.101 → 0.072 | 0.162 → 0.092 |

A temperature fixes calibration for Kev, typed-decisions and Clef-flash but not for Von, whose errors are confident
ones that no monotone rescaling can separate. With 80 prompts, treat the fitted values as a starting point, not a tuned
setting.

## Reading it

- **Jev is still the only provider usable as-is.** It is 7 to 40 points ahead of Laya, Von and Kev on topic
  accuracy and well ahead on complexity and risk, and it is calibrated. Clef-flash closes the accuracy gap (below)
  but not the routing or latency gap.
- **Upgrading Laya doesn't change this** (0.3.6 → 0.3.24 → 0.4.2 give identical decisions). It is rarely confident, so the router plays safe and bumps most requests
  to Sonnet/Opus. The routes end up more expensive than Jev's, not cheaper.
- **Kev-0.8B is the best small open topic classifier** (81%, and 100% on the multilingual group, at 0.8B), and with
  T = 0.47 it is as well calibrated as Jev. Its routes still match gold only 39% of the time, and run cheaper than
  gold (17 under-provisioned), because it gets complexity and risk wrong more often.
- **Clef-flash is the first open model to match Jev on topic** (90% vs 89%) and it beats Jev on risk (76% vs 61%)
  and on the tricky, long and multilingual groups. It is cautious, though: only 56% of answers clear the confidence
  threshold (98% of those are right), so the router plays safe and over-provisions 38 prompts. It never routes to a
  cheaper model than gold. With T = 0.64 (see above), 78% of answers clear the threshold (98% right), routes match
  gold 64% of the time (Jev: 75%), and over-provisioning drops from 38 to 27 prompts, with 2 under-provisioned
  (`-temperature clef-flash=0.64`). At ~4 s per decision on an M4, it is too slow to sit in the request path here;
  it fits a faster Mac, Workers AI, or offline use (`shadow`, `cmd/refit`).
- **VegaML 0.8B is weak zero-shot** (51% topic, 17.5% gold route) but the only model here that learns from a handful of labels
  without a training run: 40 labelled prompts lift held-out gold-route match from 17.5% to 39%.
- **Von routes closest to gold out of the box** among the smaller models (48%), but it is over-confident and
  under-provisions 15 prompts.
- **Bottleneck:** for every open model, complexity and risk are now the weak part, not topic. Fine-tuning on our
  own complexity/risk labels is the next step (see [Roadmap](roadmap.md)). A larger Kev (4B/9B) on a bigger
  machine is the other.
- **Latency:** see [Laya sidecar → Latency](laya-sidecar.md#latency) for the MPS numbers and the input-padding
  experiment.
