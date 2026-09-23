package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/mmornati/system-one-router/internal/router"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// errResult builds a tool-level error result: visible to the calling agent as a failed
// tool call, not a protocol error, so it can see the message and adjust.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// client is a thin HTTP client for a running gateway instance.
type client struct {
	baseURL string
	hc      *http.Client
}

func (c *client) post(ctx context.Context, path string, body any) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.hc.Do(req)
}

func messages(system, prompt string) []map[string]string {
	var msgs []map[string]string
	if system != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": system})
	}
	return append(msgs, map[string]string{"role": "user", "content": prompt})
}

// --- route ---

type routeIn struct {
	Prompt string `json:"prompt" jsonschema:"the user prompt to route"`
	System string `json:"system,omitempty" jsonschema:"optional system prompt"`
}

type routeCandidate struct {
	ID       string  `json:"id"`
	Skill    float64 `json:"skill"`
	EstCost  float64 `json:"est_cost_usd"`
	Eligible bool    `json:"eligible"`
}

type routeOut struct {
	Model         string           `json:"model"`
	Reason        string           `json:"reason"`
	Topic         string           `json:"topic,omitempty"`
	Confidence    float64          `json:"confidence,omitempty"`
	Complexity    int              `json:"complexity,omitempty"`
	Risk          int              `json:"risk,omitempty"`
	Private       bool             `json:"private,omitempty"`
	RequiredSkill float64          `json:"required_skill,omitempty"`
	Candidates    []routeCandidate `json:"candidates,omitempty"`
	RequestID     string           `json:"request_id,omitempty"`
}

func (c *client) route(ctx context.Context, _ *mcp.CallToolRequest, in routeIn) (*mcp.CallToolResult, routeOut, error) {
	if in.Prompt == "" {
		return errResult("prompt is required"), routeOut{}, nil
	}
	res, err := c.post(ctx, "/route", map[string]any{"messages": messages(in.System, in.Prompt)})
	if err != nil {
		return errResult(err.Error()), routeOut{}, nil
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return errResult(fmt.Sprintf("gateway returned %d: %s", res.StatusCode, body)), routeOut{}, nil
	}
	var d router.Decision
	if err := json.Unmarshal(body, &d); err != nil {
		return errResult("decoding /route response: " + err.Error()), routeOut{}, nil
	}
	out := routeOut{Model: d.Model, Reason: d.Reason, RequiredSkill: d.Required, RequestID: res.Header.Get("X-Router-Request-Id")}
	if d.Signals != nil {
		out.Topic = d.Signals.Primary
		out.Confidence = d.Signals.Confidence
		out.Complexity = d.Signals.Complexity
		out.Risk = d.Signals.Risk
		out.Private = d.Signals.Private
	}
	for i, cand := range d.Candidates {
		if i >= 3 {
			break
		}
		out.Candidates = append(out.Candidates, routeCandidate{ID: cand.ID, Skill: cand.Skill, EstCost: cand.EstCost, Eligible: cand.Eligible})
	}
	return nil, out, nil
}

// --- delegate ---

type delegateIn struct {
	Prompt    string `json:"prompt" jsonschema:"the subtask to delegate"`
	System    string `json:"system,omitempty" jsonschema:"optional system prompt"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"optional max output tokens"`
	Model     string `json:"model,omitempty" jsonschema:"model to use, default auto (router picks it)"`
}

type delegateOut struct {
	Answer    string  `json:"answer"`
	Model     string  `json:"model,omitempty"`
	Reason    string  `json:"reason,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
	RequestID string  `json:"request_id,omitempty"`
}

func (c *client) delegate(ctx context.Context, _ *mcp.CallToolRequest, in delegateIn) (*mcp.CallToolResult, delegateOut, error) {
	if in.Prompt == "" {
		return errResult("prompt is required"), delegateOut{}, nil
	}
	model := in.Model
	if model == "" {
		model = "auto"
	}
	body := map[string]any{"model": model, "messages": messages(in.System, in.Prompt)}
	if in.MaxTokens > 0 {
		body["max_tokens"] = in.MaxTokens
	}
	res, err := c.post(ctx, "/v1/chat/completions", body)
	if err != nil {
		return errResult(err.Error()), delegateOut{}, nil
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return errResult(fmt.Sprintf("gateway returned %d: %s", res.StatusCode, raw)), delegateOut{}, nil
	}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Cost float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return errResult("decoding chat completion response: " + err.Error()), delegateOut{}, nil
	}
	out := delegateOut{
		Model:     firstNonEmpty(res.Header.Get("X-Router-Model"), resp.Model),
		Reason:    res.Header.Get("X-Router-Reason"),
		CostUSD:   resp.Usage.Cost,
		RequestID: res.Header.Get("X-Router-Request-Id"),
	}
	if len(resp.Choices) > 0 {
		out.Answer = resp.Choices[0].Message.Content
	}
	return nil, out, nil
}

// --- feedback ---

type feedbackIn struct {
	RequestID string `json:"request_id" jsonschema:"the request_id from route or delegate"`
	Rating    string `json:"rating" jsonschema:"good or bad"`
	Comment   string `json:"comment,omitempty" jsonschema:"optional comment"`
}

type feedbackOut struct {
	OK bool `json:"ok"`
}

func (c *client) feedback(ctx context.Context, _ *mcp.CallToolRequest, in feedbackIn) (*mcp.CallToolResult, feedbackOut, error) {
	if in.RequestID == "" || (in.Rating != "good" && in.Rating != "bad") {
		return errResult(`request_id is required and rating must be "good" or "bad"`), feedbackOut{}, nil
	}
	res, err := c.post(ctx, "/feedback", map[string]any{"id": in.RequestID, "rating": in.Rating, "comment": in.Comment})
	if err != nil {
		return errResult(err.Error()), feedbackOut{}, nil
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(res.Body)
		return errResult(fmt.Sprintf("gateway returned %d: %s", res.StatusCode, body)), feedbackOut{}, nil
	}
	return nil, feedbackOut{OK: true}, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
