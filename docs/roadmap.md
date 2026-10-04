# Roadmap

[← Docs index](README.md)

- [x] Re-fit model skills from logged outcomes (`cmd/refit`: check results, user feedback; retries reported as
      reliability). → [Re-fitting skills](refit.md)
- [x] Check-and-escalate for non-streaming or background requests (a Jev yes/no on the answer).
      → [Check and escalate](check-and-escalate.md)
- [x] Laya sidecar (Python, MPS). → [Laya sidecar](laya-sidecar.md)
- [x] Optional fixed-length padding for Laya inputs (`--pad-buckets`, off by default: measured no steady-state
      latency gain on torch 2.14 / macOS 26 MPS; opt in and re-measure on other hardware).
- [ ] ~~Fine-tune Laya on logged Jev decisions~~: blocked, because TypeSafe's terms
      ([MCA §2.3(b)](https://typesafe.ai/legal/mca)) forbid distilling Jev. Alternative: fine-tune on our own labels
      (bench gold labels, check/feedback outcomes).
- [x] Benchmark other open decision models (Laya typed-decisions, Von 1.3, Kev-0.8B, Clef-flash) and per-provider confidence
      `temperature`. → [Other decision models](benchmark.md#other-decision-models)
- [ ] Fine-tune an open model on our own labels. Laya (0.3.20+ notebook) and Kev (`kev-finetune`) now ship a
      fine-tuning path. The bench shows complexity and risk, not topic, are what the open models get wrong, so
      label those first. Needs a held-out set larger than the 80 bench prompts.
- [x] Anthropic Messages API endpoint, so Claude Code-style clients can use the gateway.
      → [Anthropic Messages API](anthropic-messages.md)
- [x] MCP server exposing `route` / `delegate` to agents. → [Claude Code & MCP](claude-code-mcp.md)
- [x] Dashboard over `decisions.jsonl` (cost per model, agreement, escalations). → [Dashboard](observability.md)
