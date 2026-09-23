package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect starts srv and a client talking to it over an in-memory transport, returning a
// session ready to call tools. Both sides are closed on test cleanup.
func connect(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return res
}

func textOf(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return ""
	}
	return tc.Text
}

func TestRouteTool(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/route" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		msgs, _ := body["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("expected 1 message (no system), got %d", len(msgs))
		}
		w.Header().Set("X-Router-Request-Id", "req-1")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model": "gpt-mini", "reason": "cheapest above floor", "required_skill": 0.4,
			"signals":    map[string]any{"primary": "code", "confidence": 0.9, "complexity": 1, "risk": 0, "private": false},
			"candidates": []map[string]any{{"id": "gpt-mini", "skill": 0.5, "est_cost_usd": 0.001, "eligible": true}},
		})
	}))
	defer gw.Close()

	cs := connect(t, newServer(&client{baseURL: gw.URL, hc: &http.Client{Timeout: 5 * time.Second}}))
	res := callTool(t, cs, "route", routeIn{Prompt: "write a function"})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", textOf(res))
	}
	var out routeOut
	sc, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(sc, &out); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
	if out.Model != "gpt-mini" || out.RequestID != "req-1" || out.Topic != "code" {
		t.Fatalf("unexpected out: %+v", out)
	}
	if len(out.Candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(out.Candidates))
	}
}

func TestRouteToolMissingPrompt(t *testing.T) {
	cs := connect(t, newServer(&client{baseURL: "http://unused", hc: http.DefaultClient}))
	res := callTool(t, cs, "route", routeIn{})
	if !res.IsError {
		t.Fatalf("expected tool error for missing prompt")
	}
}

func TestDelegateTool(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "auto" {
			t.Fatalf("expected model auto, got %v", body["model"])
		}
		w.Header().Set("X-Router-Model", "claude-haiku")
		w.Header().Set("X-Router-Reason", "cheap enough")
		w.Header().Set("X-Router-Request-Id", "req-2")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model":   "claude-haiku",
			"choices": []map[string]any{{"message": map[string]any{"content": "hello world"}}},
			"usage":   map[string]any{"cost": 0.0002},
		})
	}))
	defer gw.Close()

	cs := connect(t, newServer(&client{baseURL: gw.URL, hc: &http.Client{Timeout: 5 * time.Second}}))
	res := callTool(t, cs, "delegate", delegateIn{Prompt: "summarize this"})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", textOf(res))
	}
	var out delegateOut
	sc, _ := json.Marshal(res.StructuredContent)
	json.Unmarshal(sc, &out)
	if out.Answer != "hello world" || out.Model != "claude-haiku" || out.Reason != "cheap enough" || out.RequestID != "req-2" {
		t.Fatalf("unexpected out: %+v", out)
	}
}

func TestDelegateToolUpstreamError(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":{"message":"upstream down"}}`))
	}))
	defer gw.Close()

	cs := connect(t, newServer(&client{baseURL: gw.URL, hc: &http.Client{Timeout: 5 * time.Second}}))
	res := callTool(t, cs, "delegate", delegateIn{Prompt: "hi"})
	if !res.IsError {
		t.Fatalf("expected tool error on upstream failure")
	}
	if text := textOf(res); text == "" {
		t.Fatalf("expected error text, got empty")
	}
}

func TestFeedbackTool(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/feedback" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["id"] != "req-1" || body["rating"] != "bad" {
			t.Fatalf("unexpected body: %+v", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer gw.Close()

	cs := connect(t, newServer(&client{baseURL: gw.URL, hc: &http.Client{Timeout: 5 * time.Second}}))
	res := callTool(t, cs, "feedback", feedbackIn{RequestID: "req-1", Rating: "bad", Comment: "wrong answer"})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", textOf(res))
	}
}

func TestFeedbackToolInvalidRating(t *testing.T) {
	cs := connect(t, newServer(&client{baseURL: "http://unused", hc: http.DefaultClient}))
	res := callTool(t, cs, "feedback", feedbackIn{RequestID: "req-1", Rating: "meh"})
	if !res.IsError {
		t.Fatalf("expected tool error for invalid rating")
	}
}
