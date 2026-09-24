# Check and escalate

[← Docs index](README.md)

With `routing.check.enabled: true`, a routed (`model: "auto"`) non-streaming answer is shown to the decision model
with one yes/no question: does it fully and correctly address the request?

If P(yes) < `threshold` (default 0.5), the request is re-sent once to a stronger model: the cheapest capable
candidate at least 0.05 more skilled than the first one (the quality floor is ignored here; context, tools, vision,
privacy and budget limits are not), else the most skilled one. The conversation then sticks to that model. If the
second call fails, the client gets the first answer.

```yaml
routing:
  check:
    enabled: true
    threshold: 0.5          # P(answer ok) below this escalates
    max_answer_chars: 3000  # answer trimmed to this (and to the provider's max_state_chars) in the state
    min_complexity: 0       # only check requests at least this complex (0..3)
```

## Cost

One extra decision call per checked answer (about $0.00004 with Jev, ~300 ms), plus a second model call for the
answers that fail.

## When the check is skipped

- streaming requests: the client already has the answer by the time it can be judged;
- sticky follow-ups and fallback routes;
- tool-call turns and empty answers;
- answers truncated by the client's own `max_tokens` (OpenAI `finish_reason: "length"`, Anthropic
  `stop_reason: "max_tokens"`). An incomplete answer would otherwise almost always fail the check and escalate,
  for no reason other than a small `max_tokens`;
- non-200 responses;
- when the model used is already the strongest capable one.

A skipped check logs nothing.

## Privacy

The check uses the same provider choice as routing, so a private request goes to the local provider when routing
would use it, and is not checked at all under `private: local_only` without one. An answer that trips the secret
pre-check counts as private too. Under `local_only`, a private request only escalates to a local model.

Check results are logged as `check` events (see [Event log](observability.md#event-log)) and are the main signal
for [re-fitting skills](refit.md).
