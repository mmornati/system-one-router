package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/mmornati/system-one-router/internal/config"
	"github.com/mmornati/system-one-router/internal/router"
)

// The Anthropic Messages API (POST /v1/messages). Requests are forwarded as-is to the upstream's
// /messages endpoint (OpenRouter serves it for every model), so only models that speak it qualify:
// see config.Model.ServesAnthropic. No Anthropic<->OpenAI translation happens here.

var anthropicDialect = &dialect{name: "anthropic", path: "/messages", usage: anthropicUsage, text: anthropicText, error: anthropicError,
	errBody: anthropicErrorBody}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	body, req, err := readMessages(r)
	if err != nil {
		anthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	model, _ := body["model"].(string)
	s.serve(w, r, anthropicDialect, body, req, isAuto(model) || s.autoAlias(model))
}

// autoAlias reports whether model matches one of anthropic.auto_models, and is routed like "auto".
func (s *Server) autoAlias(model string) bool {
	for _, p := range s.Cfg.Anthropic.AutoModels {
		if ok, _ := path.Match(p, model); ok {
			return true
		}
	}
	return false
}

// countTokens answers POST /v1/messages/count_tokens with a local estimate (chars/4): OpenRouter
// does not serve that endpoint, and the routed model is not known yet anyway.
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	_, req, err := readMessages(r)
	if err != nil {
		anthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": req.EstTokens()})
}

// readMessages parses an Anthropic Messages request, keeping unknown fields intact, and summarises it
// for routing. Tool results arrive in user-role messages: every user message counts as a turn (so a
// tool loop stays on its sticky model), but only the text blocks of user messages that have some
// are the user's words (FirstUser/LastUser). Chars counts everything the model reads, and Rest holds it
// for the privacy pre-check (tool results are where secrets such as a read .env file show up).
func readMessages(r *http.Request) (map[string]any, router.Request, error) {
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body); err != nil {
		return nil, router.Request{}, err
	}
	req := router.Request{AnthropicAPI: true}
	all, sys, _ := blocks(body["system"])
	req.System, req.Chars = strings.TrimSpace(sys), len(all)
	msgs, _ := body["messages"].([]any)
	var rest strings.Builder
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		all, text, img := blocks(m["content"])
		req.Chars += len(all)
		rest.WriteString(all + "\n")
		req.Vision = req.Vision || img
		if m["role"] != "user" {
			continue
		}
		req.UserTurns++
		if strings.TrimSpace(text) == "" {
			continue // tool results only
		}
		if req.FirstUser == "" {
			req.FirstUser = text
		}
		req.LastUser = text
	}
	req.Rest = rest.String()
	if tools, _ := body["tools"].([]any); len(tools) > 0 {
		req.Tools = true
		tb, _ := json.Marshal(tools)
		req.Chars += len(tb)
	}
	return body, req, nil
}

// blocks reads Anthropic content (a string or content blocks): all the text the model reads (text,
// thinking, tool inputs and results), the text blocks alone, and whether there is an image.
func blocks(c any) (all, text string, img bool) {
	switch v := c.(type) {
	case string:
		return v, v, false
	case []any:
		var a, t strings.Builder
		for _, p := range v {
			b, _ := p.(map[string]any)
			switch b["type"] {
			case "text":
				s, _ := b["text"].(string)
				t.WriteString(s)
				t.WriteByte('\n')
			case "image":
				img = true
			case "thinking":
				s, _ := b["thinking"].(string)
				a.WriteString(s + "\n")
			case "tool_use":
				in, _ := json.Marshal(b["input"])
				a.Write(in)
				a.WriteByte('\n')
			case "tool_result":
				s, _, i := blocks(b["content"])
				a.WriteString(s + "\n")
				img = img || i
			}
		}
		return a.String() + t.String(), t.String(), img
	}
	return "", "", false
}

// anthropicUsage reads Anthropic usage from a response body, a message_start event (message.usage) or
// a message_delta event (usage). Prompt tokens include cache reads and writes, like OpenAI's
// prompt_tokens; cost is OpenRouter's addition.
func anthropicUsage(b []byte) usage {
	type au struct {
		Cost        float64 `json:"cost"`
		Input       int     `json:"input_tokens"`
		Output      int     `json:"output_tokens"`
		CacheRead   int     `json:"cache_read_input_tokens"`
		CacheCreate int     `json:"cache_creation_input_tokens"`
		Details     struct {
			Thinking int `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	}
	var v struct {
		Usage   *au `json:"usage"`
		Message struct {
			Usage *au `json:"usage"`
		} `json:"message"`
	}
	json.Unmarshal(b, &v)
	u := v.Usage
	if u == nil {
		u = v.Message.Usage
	}
	if u == nil {
		return usage{}
	}
	return usage{CostUSD: u.Cost, PromptTokens: u.Input + u.CacheRead + u.CacheCreate, CompletionTokens: u.Output, ReasoningTokens: u.Details.Thinking}
}

// anthropicText returns the answer's text blocks and whether it is a final answer worth checking:
// non-empty and not a tool-use turn.
func anthropicText(b []byte) (string, bool) {
	var v struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(b, &v) != nil || v.StopReason == "tool_use" {
		return "", false
	}
	var sb strings.Builder
	for _, c := range v.Content {
		switch c.Type {
		case "tool_use", "server_tool_use":
			return "", false
		case "text":
			sb.WriteString(c.Text)
		}
	}
	return sb.String(), strings.TrimSpace(sb.String()) != ""
}

func anthropicError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, anthropicErrorJSON(code, msg))
}

func anthropicErrorJSON(code int, msg string) map[string]any {
	typ := map[int]string{401: "authentication_error", 403: "permission_error", 404: "not_found_error",
		413: "request_too_large", 429: "rate_limit_error", 529: "overloaded_error"}[code]
	switch {
	case typ != "":
	case code >= 500:
		typ = "api_error"
	default:
		typ = "invalid_request_error"
	}
	return map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}}
}

// anthropicErrorBody rewrites an upstream error body that is not in Anthropic's shape (OpenRouter's
// {"error":{"message"}}, or an empty 429) so Anthropic clients can read it.
func anthropicErrorBody(code int, b []byte) []byte {
	var v struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &v) == nil && v.Type == "error" {
		return b
	}
	msg := v.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(b))
	}
	if msg == "" {
		msg = http.StatusText(code)
	}
	out, _ := json.Marshal(anthropicErrorJSON(code, "upstream: "+msg))
	return out
}

// anthropicModels lists the models in the Anthropic GET /v1/models shape (asked for by clients that
// send anthropic-version).
func anthropicModels(w http.ResponseWriter, cfg *config.Config) {
	const created = "2026-01-01T00:00:00Z"
	data := []map[string]any{{"type": "model", "id": "auto", "display_name": "auto (router)", "created_at": created}}
	for _, m := range cfg.Models {
		if m.ServesAnthropic() {
			data = append(data, map[string]any{"type": "model", "id": m.ID, "display_name": m.ID, "created_at": created})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "has_more": false,
		"first_id": data[0]["id"], "last_id": data[len(data)-1]["id"]})
}
