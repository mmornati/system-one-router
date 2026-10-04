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

## Results

2026-10-02, 80 prompts, M4 16 GB for the local models, `laya` 0.3.24:

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

### Recalibrating confidence

The router raises the quality floor when topic confidence is below `confidence_threshold`, so a model that is right
but unsure still over-provisions. `providers.<name>.temperature` rescales choice probabilities to fix that. We fit
T by minimising log-loss on half of the prompts and measured on the other half (20 random 2-fold splits):

| | Kev-0.8B | Laya typed-decisions | Von 1.3 | Clef-flash |
|---|---|---|---|---|
| Fitted T (cross-validated median) | 0.47 | 0.43 | 2.47 | 0.64 |
| Confident ≥ 0.8, held-out | 5% → 60% (89% right) | 0% → 33% (92% right) | 75% → 26% (69% right) | 56% → 81% (95% right) |
| Route = gold route with that T (full bench) | 22% → 39% | 12% → 25% | 48% → 36% | 52.5% → **64%** |
| ECE with that T (full bench, in-sample) | 0.349 → **0.059** | 0.509 → 0.072 | 0.226 → 0.236 | 0.101 → 0.072 |

A temperature fixes calibration for Kev, typed-decisions and Clef-flash but not for Von, whose errors are confident
ones that no monotone rescaling can separate. With 80 prompts, treat the fitted values as a starting point, not a tuned
setting.

## Reading it

- **Jev is still the only provider usable as-is.** It is 7 to 40 points ahead of Laya, Von and Kev on topic
  accuracy and well ahead on complexity and risk, and it is calibrated. Clef-flash closes the accuracy gap (below)
  but not the routing or latency gap.
- **Upgrading Laya doesn't change this.** It is rarely confident, so the router plays safe and bumps most requests
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
- **Von routes closest to gold out of the box** among the smaller models (48%), but it is over-confident and
  under-provisions 15 prompts.
- **Bottleneck:** for every open model, complexity and risk are now the weak part, not topic. Fine-tuning on our
  own complexity/risk labels is the next step (see [Roadmap](roadmap.md)). A larger Kev (4B/9B) on a bigger
  machine is the other.
- **Latency:** see [Laya sidecar → Latency](laya-sidecar.md#latency) for the MPS numbers and the input-padding
  experiment.
