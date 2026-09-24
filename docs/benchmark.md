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
go test -race ./...
node --env-file=.env bench/jev-check.ts --baseline             # raw Jev vs an LLM router (same cases)
```

## Results

2026-09-23, 80 prompts, M4 16 GB for Laya:

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

## Reading it

- **Jev is usable as-is.**
- **Laya out of the box is not.** It is rarely confident, so the router plays safe and bumps most requests to
  Sonnet/Opus. The routes end up more expensive than Jev's, not cheaper.
- **When Laya English is confident, it is right**, which is what makes it a candidate for further tuning once we
  have a fine-tuning path that doesn't depend on Jev's outputs (see [Roadmap](roadmap.md)).
- **Latency:** see [Laya sidecar → Latency](laya-sidecar.md#latency) for the MPS numbers and the input-padding
  experiment.
