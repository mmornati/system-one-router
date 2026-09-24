# HTTP API

[← Docs index](README.md)

The gateway listens on `127.0.0.1:8787` by default.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `POST /v1/chat/completions` | OpenAI-compatible chat, routed when `model: "auto"` |
| `POST /v1/messages` | Anthropic Messages API, routed the same way. See [Anthropic Messages API](anthropic-messages.md) |
| `POST /v1/messages/count_tokens` | Local token estimate (`chars/4`); OpenRouter doesn't serve this |
| `POST /route` | Dry-run the routing decision only. No model is called |
| `POST /feedback` | Rate a past request (`good`/`bad`) by its `X-Router-Request-Id` |
| `GET /v1/models` | Model catalog (Anthropic shape with an `anthropic-version` header) |
| `GET /stats` | Aggregated stats as JSON (`?days=N`, `0` = all time) |
| `GET /dashboard` | HTML dashboard over `/stats`. See [Observability](observability.md) |
| `GET /healthz` | Liveness check |

```bash
curl -s localhost:8787/v1/chat/completions -d '{"model":"auto","messages":[{"role":"user","content":"hi"}]}' -D -
curl -s localhost:8787/v1/messages -H 'anthropic-version: 2023-06-01' -d '{"model":"auto","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}' -D -
curl -s localhost:8787/route -d '{"messages":[{"role":"user","content":"Design a multi-region Postgres failover"}]}'   # dry run: decision only
```

## Response headers

On routed requests:

| Header | Content |
|---|---|
| `X-Router-Model` | Model that answered |
| `X-Router-Reason` | Why it was picked |
| `X-Router-Topic` | Main topic |
| `X-Router-Complexity` | 0–3 |
| `X-Router-Risk` | 0–2 |
| `X-Router-Failed` | Models that errored before this one |
| `X-Router-Request-Id` | Id to use with `POST /feedback` and to find the request in the log |
| `X-Router-Checked` | P(answer ok), 2 decimals (with `routing.check.enabled: true`) |
| `X-Router-Escalated` | `<from>-><to>` (with `routing.check.enabled: true`) |

## Feedback

`POST /feedback` records a rating against a request id, as a `feedback` event (`data.id`, `data.rating`,
`data.comment`). `rating` must be `"good"` or `"bad"`; the id isn't checked against the log.

```bash
curl -s localhost:8787/feedback -d '{"id":"<X-Router-Request-Id>","rating":"bad","comment":"picked a model too weak for this"}'
# 204 No Content
```

Ratings are one of the outcome signals used by [`cmd/refit`](refit.md).
