# Re-fitting skills

[← Docs index](README.md)

The `skills` in `config.yaml` start as guesses. `cmd/refit` re-estimates them from the outcomes in
`data/decisions.jsonl`. It only reads the log, and it only writes a file when you pass `-write`.

```bash
go run ./cmd/refit                                   # report; -days 30 to use recent events only
go run ./cmd/refit -write config.new.yaml && diff config.yaml config.new.yaml
make refit ARGS="-min-n 40 -prior 20"
```

> [!NOTE]
> The fit needs outcome data to mean anything. Without [check-and-escalate](check-and-escalate.md) (`check`
> events) or `POST /feedback` ratings, the log holds only routing decisions, and refit reports "not enough data".

## Signals

It uses the routed `chat` events (not sticky, not pass-through) that have an outcome:

| Signal | Label | Weight (flag) |
|---|---|---|
| `check` event for that id + model (check-and-escalate) | `p_ok`, a soft label in 0..1 | 1 (`-w-check`) |
| `feedback` for that id | good = 1, bad = 0; on an escalated request it rates the model that answered last | 2 (`-w-feedback`) |
| model in `failed`, or status 429/5xx | availability, **not** quality: reported in a separate reliability table | – |
| other 4xx | skipped | – |

An answer counts toward every topic in proportion to its logged topic probabilities, so a
`{docs: 0.8, writing: 0.2}` answer adds 0.8 of an observation to docs and 0.2 to writing. `N_EFF` is the sum of
these weights.

## Method: compare with peers at the same difficulty

Two biases make raw success rates misleading:

- The router only sends a model the prompts whose floor it clears, so a cheap model's success rate comes from easy
  prompts.
- Labels have no absolute scale. `p_ok` rarely gets near 1 and ratings skew to "bad", while a model well above its
  floor should succeed about 95% of the time. Read as absolute success rates, labels drag every skill down.

So a label is only compared with the other models' labels from the same source (check or feedback) at the same
difficulty, `d = min_skill[complexity] + risk_bonus[risk]` (the floor the answer was routed against):

```
E[label | skill s] = σ(logit(peer) + (s − seed) / scale)        peer = the other models' mean label there
```

- A model whose labels are as good as its peers' keeps its seed, whatever the absolute label level.
- Better or worse labels move it along a logistic curve: beating peers who already score 0.97 leaves little
  headroom and is weak evidence, while falling behind them is strong evidence.
- A difficulty level where no other model was seen carries no signal and does not count in `N_EFF`. The first lines
  of the report show how much of the data that is. [Exploration](how-it-works.md#exploration) creates peers.
- The absolute level of the skills stays anchored to the seeds, because judge labels alone cannot identify it.

The seed skill from `config.yaml` is the prior: `-prior` (10) pseudo-observations, so with no data the fit is
exactly the seed. The fitted skill is the posterior mode, the root of one monotone equation, found by bisection.
`default_skill` is fitted the same way, from outcomes on the topics the model has no explicit skill for, minus any
topic that gets its own new skill in this run, so no outcome is counted twice.

## Knobs

| Flag | Default | Meaning |
|---|---|---|
| `-scale` | `0.1` | Being 0.1 of skill better multiplies the odds of a good label by e. The floors are about 0.15 apart, so one complexity tier is worth about 4.5 times the odds. |
| `-target` | `0.8` | The success rate expected at skill = difficulty. It shapes the prior's curvature. |
| `-calibrate=false` | | Reads labels as absolute success rates, `σ((s − d)/scale + logit(target))`. Kept for comparison; it shows the downward bias. |
| `-prior` | `10` | Pseudo-observations of the seed |
| `-min-n` | `20` | Minimum `N_EFF` to propose a change |
| `-min-delta` | `0.02` | Minimum `\|fitted − seed\|` to propose a change |
| `-w-check` / `-w-feedback` | `1` / `2` | Weight of an answer check / a user rating |
| `-write <path>` | | Write the config with the proposed values |
| `-days` | `0` (all) | Only use events from the last N days |
| `-config` / `-log` | `config.yaml` / the config's `log_path` | Inputs |

`-write` copies the config with the changed values. It edits the original text in place, so comments, blank lines
and order are kept, and new topics are appended to the model's `skills`. `config.yaml` itself is only overwritten
if you name it.

## Example report

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

This is a synthetic log. Labels average 0.72 while the seeds predict 0.91, and still only relative differences
move skills:

- deepseek's debugging answers score below luna's code-review answers at the same difficulty, so deepseek's
  debugging skill goes down and luna's code-review skill goes up.
- qwen's chat row has almost no weight, because no other model answered those trivial prompts.

With `-calibrate=false`, every skill in this log except qwen/code-gen drops by 0.05 to 0.30.
