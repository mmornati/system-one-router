# Anthropic Messages API

[← Docs index](README.md)

`POST /v1/messages` takes the Anthropic Messages format (system as a string or blocks; text, image, tool_use,
tool_result and thinking blocks; tools; streaming) and forwards it unchanged, apart from `model`, to the upstream's
`/messages` endpoint. OpenRouter serves that endpoint for every model, not only Anthropic ones (checked live with
`qwen/qwen3.7-flash`, `deepseek/deepseek-v4.1-flash` and `openai/gpt-5.6-luna`), and reports `cost` in its usage.

Routing, retries, sticky conversations, budgets, response headers and metering are the same as for chat
completions; logged `chat` events carry `data.api: "anthropic"`.

To use it from Claude Code, see [Claude Code & MCP](claude-code-mcp.md).

## Routed models

`auto`, `router/auto`, an empty model, or a name matching one of `anthropic.auto_models` (globs). Any other model
name is passed through unchanged, so it must be a valid upstream id (e.g. `anthropic/claude-sonnet-5`, not
`claude-sonnet-5`).

```yaml
anthropic:
  auto_models: ["claude-*"]   # default []: route Claude Code's hard-coded model names
```

The trade-off: once a pattern matches, the client can no longer pick that model itself. Everything matching is
routed, including Claude Code's background calls (titles, summaries), which usually end up on the cheapest model.

## Behaviour

| Topic | Behaviour |
|---|---|
| **What the router sees** | The system prompt; user turns are the text blocks of user messages. Tool results also come in user-role messages: they count as turns (so an agent's tool loop stays on its sticky model and is not re-decided on every step) and in the input size, but not as the user's words. Image blocks (also inside tool results) require a vision model. |
| **Privacy** | The secret pre-check scans everything the model would read, tool results included (a `Read` of a `.env` file). Under `private: local_only`, a secret that appears mid-conversation also breaks the sticky model if it is remote: the request is re-routed to a local model or refused (same for the chat endpoint). |
| **Local runtimes** | A model with its own `base_url` is skipped for `/v1/messages` (candidate reason `no Anthropic API`) unless it has `anthropic: true`, meaning the runtime serves `/messages` itself. There is no Anthropic-to-OpenAI translation, so a private request under `private: local_only` with no such local model gets 422. |
| **Thinking** | Unlike the chat endpoint's `reasoning.effort`, the gateway never adds `thinking`, since it constrains `max_tokens` and `temperature`. The client's own settings are passed through. |
| **Answer check** | Works for non-streaming requests. The answer is the concatenated text blocks, and turns that end in `tool_use` are not checked. See [Check and escalate](check-and-escalate.md). |
| **`count_tokens`** | OpenRouter does not serve it (404), so `POST /v1/messages/count_tokens` returns a local estimate, `{"input_tokens": chars/4}`, without calling anything. |
| **Headers** | Client credentials (`Authorization`, `x-api-key`) are never forwarded, on either endpoint. Only `Accept`, `HTTP-Referer`, `X-Title`, `anthropic-version` and `anthropic-beta` are. |
| **Metering** | Prompt tokens are `input_tokens` plus cache reads and writes. For streams, usage is read from the `message_start` and `message_delta` events (counters are cumulative: the largest value wins). |
| **Errors** | In Anthropic's shape, `{"type":"error","error":{"type":…,"message":…}}`, including upstream errors that were not (OpenRouter's `{"error":{…}}`, an empty 429). |
