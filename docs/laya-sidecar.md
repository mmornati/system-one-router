# Laya sidecar

[← Docs index](README.md)

`sidecar/laya_server.py` serves Laya (`pip install laya`, Apache-2.0, weights from Hugging Face
`convaiinnovations/laya`) with the same request and response format as Jev's Decisions API, so the gateway uses it
unchanged. Use it to keep decisions on your machine, for example with `decision.provider: auto` so private requests
never leave it (see [decision providers](how-it-works.md#decision-providers)).

Models: `laya` (English, 512 tokens), `laya-multilingual` (1024 tokens, 100+ languages), and `laya-auto` (picks one
by detected language).

```bash
uv venv --python 3.12 sidecar/.venv && uv pip install --python sidecar/.venv/bin/python laya==0.3.6
sidecar/.venv/bin/python sidecar/laya_server.py --preload [--device cpu|mps] [--pad-buckets 64,128,256,512,1024|none]
```

Or `make laya` (the first run downloads ~3 GB of weights). Then enable the provider in `config.yaml`:

```yaml
decision:
  providers:
    laya:
      url: http://127.0.0.1:8788/decisions
      model: convaiinnovations/laya
      local: true
      max_state_chars: 1500   # English checkpoint: 512 tokens
      timeout: 1s
```

> [!NOTE]
> `laya` 0.3.6 ships inference only, with no training/fine-tuning API (see [Roadmap](roadmap.md)).

## Input padding (`--pad-buckets`)

`--pad-buckets` (default `none`) optionally pads every call's tokenized sequence up to the smallest bucket that
fits, so repeated calls reuse the same MPS shape instead of triggering a recompile.

It's implemented as a small wrapper around `laya.agent.collate_items` (the function `Agent.system_one` uses to build
the batch) that extends `input_ids`/`attention_mask` to the bucket length with zero-attention padding, so it doesn't
require patching the `laya` package itself, and it is output-preserving (verified bit-identical, see below).

It's off by default because it measured no latency win on the current torch/macOS stack. Pass e.g.
`--pad-buckets 64,128,256,512,1024` to opt in and measure on your own hardware; with `--preload`, each loaded
checkpoint then also runs one warmup call per bucket at or under its `max_len`, so the first real request at any
bucket size is already fast.

## Latency

Laya on MPS takes about 30–70 ms per call for a repeated input shape, but pays a one-off kernel-compile tax the
first time a call uses a sequence length MPS hasn't seen yet (historically up to 200–350 ms). Padding to fixed
buckets means MPS only ever compiles a handful of shapes, but **on the currently installed stack (torch 2.14,
macOS 26, Apple M4) it measured no latency win, so it defaults to off**.

Steady-state (second-pass) numbers on 40 varied-length prompts from `bench/cases.json`, English checkpoint, MPS:

| | p50 | p95 |
|---|---|---|
| unpadded (default) | 100 ms | 663 ms |
| padded, coarse buckets (64/128/256/512/1024) | 163 ms | 678 ms |
| padded, fine buckets (every 32 to 512, every 64 to 1024) | 141 ms | 837 ms |

Neither bucket set beats unpadded on p50 or p95: the shape-recompile tax on this stack is smaller than the extra
attention compute padding spends on the padded positions, and finer buckets don't recover it either. Opt in on a
stack where the recompile tax is worse. It's exact, not approximate: on 12 varied prompts through the English
checkpoint the padded vs. unpadded probabilities were bit-identical (max diff 0.0), since the attention mask zeroes
out padded positions everywhere they reach the model (encoder attention and the decision head's
`src_key_padding_mask`). The CPU is slower still (517 ms p50) and shows no shape-change penalty at all.
