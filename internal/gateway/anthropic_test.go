package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mmornati/system-one-router/internal/config"
	"github.com/mmornati/system-one-router/internal/decision"
	"github.com/mmornati/system-one-router/internal/router"
	"github.com/mmornati/system-one-router/internal/store"
	"github.com/mmornati/system-one-router/internal/upstream"
)

// upCall is one request the fake upstream received.
type upCall struct {
	path   string
	header http.Header
	body   map[string]any
}

type anthropicEnv struct {
	gw    *httptest.Server
	rt    *router.Router
	log   string
	mu    sync.Mutex
	calls []upCall
}

func (e *anthropicEnv) seen() []upCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]upCall(nil), e.calls...)
}

// newAnthropicServer runs the gateway against an upstream serving both /chat/completions and
// /messages (Anthropic shape, with OpenRouter's cost in usage). limited answers 429.
func newAnthropicServer(t *testing.T, limited string, tweak func(*config.Config, *decision.Selector)) *anthropicEnv {
	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	e := &anthropicEnv{log: filepath.Join(t.TempDir(), "decisions.jsonl")}
	jev := fakeJev(t)
	t.Cleanup(jev.Close)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		e.mu.Lock()
		e.calls = append(e.calls, upCall{r.URL.Path, r.Header.Clone(), body})
		e.mu.Unlock()
		model, _ := body["model"].(string)
		if model == limited {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.URL.Path != "/messages" {
			json.NewEncoder(w).Encode(map[string]any{"model": model, "choices": []any{}})
			return
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: message_start\ndata: "+`{"type":"message_start","message":{"model":"`+model+`","content":[],"usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":0}}}`+"\n\n")
			io.WriteString(w, "event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`+"\n\n")
			io.WriteString(w, "event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20,"output_tokens_details":{"thinking_tokens":4},"cost":0.003}}`+"\n\n")
			io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"type": "message", "role": "assistant", "model": model,
			"content": []any{map[string]any{"type": "text", "text": "answer from " + model}}, "stop_reason": "end_turn",
			"usage": map[string]any{"input_tokens": 12, "cache_creation_input_tokens": 3, "cache_read_input_tokens": 0, "output_tokens": 34, "cost": 0.001}})
	}))
	t.Cleanup(up.Close)
	sel := &decision.Selector{Mode: "jev", Providers: map[string]decision.Provider{
		"jev": decision.NewHTTPProvider("jev", jev.URL, "typesafe/jev-1.13", "k", false, 4000, cfg.Decision.Providers["jev"].Timeout),
	}}
	if tweak != nil {
		tweak(cfg, sel)
	}
	e.rt = router.New(cfg, sel)
	client := upstream.New(up.URL, "gateway-key")
	log, err := store.Open(e.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	e.gw = httptest.NewServer((&Server{Cfg: cfg, Router: e.rt, Upstream: func(string) *upstream.Client { return client }, Log: log, LogPath: e.log}).Handler())
	t.Cleanup(e.gw.Close)
	return e
}

func postH(t *testing.T, url, body string, hdr map[string]string) *http.Response {
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestReadMessages(t *testing.T) {
	body := `{"model":"auto","max_tokens":100,"metadata":{"user_id":"u1"},"x_custom":1,
		"system":[{"type":"text","text":"You are Claude Code."},{"type":"text","text":"Be terse."}],
		"tools":[{"name":"Read","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"fix the bug"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"let me read"},{"type":"tool_use","id":"t1","name":"Read","input":{"path":"main.go"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"package main"}]}]},
			{"role":"assistant","content":"Found it."},
			{"role":"user","content":"now add a test"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	b, req, err := readMessages(r)
	if err != nil {
		t.Fatal(err)
	}
	if req.System != "You are Claude Code.\nBe terse." || req.FirstUser != "fix the bug\n" || req.LastUser != "now add a test" ||
		req.UserTurns != 3 || !req.Vision || !req.Tools || !req.AnthropicAPI {
		t.Fatalf("summary: %+v", req)
	}
	if req.Chars < len("fix the bug")+len("let me read")+len("package main")+len("Found it.")+len("now add a test") {
		t.Fatalf("chars %d too low", req.Chars)
	}
	if b["x_custom"] != 1.0 || b["metadata"] == nil {
		t.Fatalf("unknown fields dropped: %v", b)
	}
	if _, req, _ := readMessages(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"system":"sys","messages":[{"role":"user","content":"hi"}]}`))); req.System != "sys" || req.LastUser != "hi" || req.Vision || req.Tools {
		t.Fatalf("string system/content: %+v", req)
	}
}

func TestMessagesAutoRoutes(t *testing.T) {
	e := newAnthropicServer(t, "", nil)
	res := postH(t, e.gw.URL+"/v1/messages", `{"model":"auto","max_tokens":32,"messages":[{"role":"user","content":"hello!"}]}`, nil)
	b, _ := io.ReadAll(res.Body)
	h := res.Header
	if res.StatusCode != 200 || h.Get("X-Router-Model") != "qwen/qwen3.7-flash" || h.Get("X-Router-Reason") == "" || h.Get("X-Router-Topic") != "chat" ||
		h.Get("X-Router-Complexity") != "0" || h.Get("X-Router-Risk") != "1" || h.Get("X-Router-Request-Id") == "" ||
		!strings.Contains(string(b), "answer from qwen/qwen3.7-flash") {
		t.Fatalf("status %d headers %v body %s", res.StatusCode, h, b)
	}
	c := e.seen()[0]
	if c.path != "/messages" || c.body["model"] != "qwen/qwen3.7-flash" || c.body["reasoning"] != nil || c.body["usage"] != nil || c.body["thinking"] != nil {
		t.Fatalf("upstream call: %s %v", c.path, c.body)
	}
	chats := events(t, e.log, "chat")
	if len(chats) != 1 || chats[0]["api"] != "anthropic" || chats[0]["prompt_tokens"] != 15.0 || chats[0]["completion_tokens"] != 34.0 ||
		chats[0]["cost_usd"] != 0.001 || chats[0]["decision"] == nil || chats[0]["id"] != h.Get("X-Router-Request-Id") {
		t.Fatalf("chat events: %v", chats)
	}
}

func TestMessagesPassThroughAndAliases(t *testing.T) {
	e := newAnthropicServer(t, "", nil)
	res := postH(t, e.gw.URL+"/v1/messages", `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"x"}]}`, nil)
	io.ReadAll(res.Body)
	if res.StatusCode != 200 || res.Header.Get("X-Router-Model") != "" || e.seen()[0].body["model"] != "claude-sonnet-4-5" {
		t.Fatalf("pass-through: %v %v", res.Header, e.seen()[0].body)
	}

	e = newAnthropicServer(t, "", func(c *config.Config, _ *decision.Selector) { c.Anthropic.AutoModels = []string{"claude-*"} })
	res = postH(t, e.gw.URL+"/v1/messages", `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello!"}]}`, nil)
	io.ReadAll(res.Body)
	if res.Header.Get("X-Router-Model") != "qwen/qwen3.7-flash" || e.seen()[0].body["model"] != "qwen/qwen3.7-flash" {
		t.Fatalf("alias not routed: %v %v", res.Header, e.seen()[0].body)
	}
	res = postH(t, e.gw.URL+"/v1/messages", `{"model":"openai/gpt-5.6-luna","messages":[{"role":"user","content":"hello!"}]}`, nil)
	io.ReadAll(res.Body)
	if res.Header.Get("X-Router-Model") != "" || e.seen()[1].body["model"] != "openai/gpt-5.6-luna" {
		t.Fatalf("non-matching model routed: %v", res.Header)
	}
}

func TestMessagesRetryOnRateLimit(t *testing.T) {
	e := newAnthropicServer(t, "qwen/qwen3.7-flash", nil)
	res := postH(t, e.gw.URL+"/v1/messages", `{"model":"auto","messages":[{"role":"user","content":"hello!"}]}`, nil)
	io.ReadAll(res.Body)
	calls := e.seen()
	if res.StatusCode != 200 || res.Header.Get("X-Router-Failed") != "qwen/qwen3.7-flash" || len(calls) != 2 ||
		calls[1].path != "/messages" || res.Header.Get("X-Router-Model") != calls[1].body["model"] {
		t.Fatalf("status %d headers %v calls %d", res.StatusCode, res.Header, len(calls))
	}
}

func TestMessagesStreamingMetered(t *testing.T) {
	e := newAnthropicServer(t, "", nil)
	res := postH(t, e.gw.URL+"/v1/messages", `{"model":"auto","stream":true,"messages":[{"role":"user","content":"hello!"}]}`, nil)
	b, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(b), "event: message_stop") || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %v %s", res.Header, b)
	}
	if got := e.rt.Tracker.Spent("qwen/qwen3.7-flash"); got != 0.003 {
		t.Fatalf("spend: %v", got)
	}
	chats := waitForLog(t, e.log, 1)
	d := chats[0]["data"].(map[string]any)
	if d["stream"] != true || d["api"] != "anthropic" || d["prompt_tokens"] != 15.0 || d["completion_tokens"] != 20.0 ||
		d["reasoning_tokens"] != 4.0 || d["cost_usd"] != 0.003 {
		t.Fatalf("stream usage: %v", d)
	}
}

func TestClientAuthNotForwarded(t *testing.T) {
	e := newAnthropicServer(t, "", nil)
	hdr := map[string]string{"x-api-key": "client-secret", "Authorization": "Bearer client-secret",
		"anthropic-version": "2023-06-01", "anthropic-beta": "tools-2024-04-04", "Cookie": "s=1"}
	for _, ep := range []string{"/v1/messages", "/v1/chat/completions"} {
		res := postH(t, e.gw.URL+ep, `{"model":"auto","messages":[{"role":"user","content":"hello!"}]}`, hdr)
		io.ReadAll(res.Body)
	}
	for _, c := range e.seen() {
		if c.header.Get("Authorization") != "Bearer gateway-key" || c.header.Get("X-Api-Key") != "" || c.header.Get("Cookie") != "" {
			t.Fatalf("%s: client auth leaked upstream: %v", c.path, c.header)
		}
		if c.header.Get("Anthropic-Version") != "2023-06-01" || c.header.Get("Anthropic-Beta") != "tools-2024-04-04" {
			t.Fatalf("%s: anthropic headers not forwarded: %v", c.path, c.header)
		}
	}
}

func TestMessagesPrivateLocalOnlyRefused(t *testing.T) {
	local := config.Model{ID: "local/tiny", BaseURL: "http://127.0.0.1:1/v1", Local: true, Context: 32768, DefaultSkill: 0.5, OutputMultiplier: 1}
	e := newAnthropicServer(t, "", func(c *config.Config, s *decision.Selector) {
		c.Models = append(c.Models, local)
		c.Decision.Private, s.PrivatePolicy = "local_only", "local_only"
	})
	secret := `{"model":"auto","messages":[{"role":"user","content":"password: hunter2"}]}`
	res := postH(t, e.gw.URL+"/v1/messages", secret, nil)
	var body struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != http.StatusUnprocessableEntity || body.Type != "error" || body.Error.Type != "invalid_request_error" || len(e.seen()) != 0 {
		t.Fatalf("status %d body %+v calls %d", res.StatusCode, body, len(e.seen()))
	}
	// The chat API can use the local model for the same request.
	if d := e.rt.Route(t.Context(), router.Request{FirstUser: "password: x", LastUser: "password: x", UserTurns: 1}, "x"); d.Refused || d.Model != "local/tiny" {
		t.Fatalf("chat route: %+v", d)
	}
}

func TestCountTokensAndModels(t *testing.T) {
	e := newAnthropicServer(t, "", nil)
	res := postH(t, e.gw.URL+"/v1/messages/count_tokens", `{"model":"auto","messages":[{"role":"user","content":"`+strings.Repeat("a", 400)+`"}]}`, nil)
	var ct map[string]int
	json.NewDecoder(res.Body).Decode(&ct)
	if res.StatusCode != 200 || ct["input_tokens"] != 101 || len(e.seen()) != 0 {
		t.Fatalf("count_tokens: %d %v", res.StatusCode, ct)
	}

	req, _ := http.NewRequest(http.MethodGet, e.gw.URL+"/v1/models", nil)
	req.Header.Set("anthropic-version", "2023-06-01")
	res, _ = http.DefaultClient.Do(req)
	var am struct {
		Data []struct{ Type, ID string }
	}
	json.NewDecoder(res.Body).Decode(&am)
	if len(am.Data) < 2 || am.Data[0].ID != "auto" || am.Data[0].Type != "model" {
		t.Fatalf("anthropic models: %+v", am)
	}
	res, _ = http.Get(e.gw.URL + "/v1/models")
	var om struct{ Object string }
	json.NewDecoder(res.Body).Decode(&om)
	if om.Object != "list" {
		t.Fatalf("openai models shape changed: %+v", om)
	}
}

// TestMessagesCheckSkipsTruncated checks that a /v1/messages answer cut off by the client's
// max_tokens (stop_reason "max_tokens") is never sent to the answer check, so it can't be
// misjudged as incomplete and escalated for no reason.
func TestMessagesCheckSkipsTruncated(t *testing.T) {
	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Routing.Check.Enabled = true
	log := filepath.Join(t.TempDir(), "decisions.jsonl")
	checks := &atomic.Int32{}
	jev := fakeDecisions(t, 0.1, checks) // would fail the check and escalate, if run
	t.Cleanup(jev.Close)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"type": "message", "role": "assistant", "model": "qwen/qwen3.7-flash",
			"content": []any{map[string]any{"type": "text", "text": "cut off mid-s"}}, "stop_reason": "max_tokens",
			"usage": map[string]any{"input_tokens": 12, "output_tokens": 200, "cost": 0.001}})
	}))
	t.Cleanup(up.Close)
	sel := &decision.Selector{Mode: "jev", Providers: map[string]decision.Provider{
		"jev": decision.NewHTTPProvider("jev", jev.URL, "typesafe/jev-1.13", "k", false, 4000, cfg.Decision.Providers["jev"].Timeout),
	}}
	rt := router.New(cfg, sel)
	client := upstream.New(up.URL, "gateway-key")
	l, err := store.Open(log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	gw := httptest.NewServer((&Server{Cfg: cfg, Router: rt, Upstream: func(string) *upstream.Client { return client }, Log: l, LogPath: log}).Handler())
	t.Cleanup(gw.Close)

	res := postH(t, gw.URL+"/v1/messages", `{"model":"auto","max_tokens":200,"messages":[{"role":"user","content":"hello!"}]}`, nil)
	io.ReadAll(res.Body)
	if res.Header.Get("X-Router-Checked") != "" || res.Header.Get("X-Router-Escalated") != "" || checks.Load() != 0 {
		t.Fatalf("truncated answer must skip the check: checked=%q escalated=%q checks=%d",
			res.Header.Get("X-Router-Checked"), res.Header.Get("X-Router-Escalated"), checks.Load())
	}
	if got := events(t, log, "check"); len(got) != 0 {
		t.Fatalf("check events: %v", got)
	}
}

func TestMessagesCheckEscalates(t *testing.T) {
	e := newAnthropicServer(t, "", func(c *config.Config, s *decision.Selector) {
		c.Routing.Check.Enabled = true
		jev := fakeDecisions(t, 0.2, nil)
		t.Cleanup(jev.Close)
		s.Providers["jev"] = decision.NewHTTPProvider("jev", jev.URL, "m", "k", false, 6000, c.Decision.Providers["jev"].Timeout)
	})
	res := postH(t, e.gw.URL+"/v1/messages", `{"model":"auto","messages":[{"role":"user","content":"hello!"}]}`, nil)
	b, _ := io.ReadAll(res.Body)
	const from, to = "qwen/qwen3.7-flash", "anthropic/claude-sonnet-5"
	if res.Header.Get("X-Router-Escalated") != from+"->"+to || !strings.Contains(string(b), "answer from "+to) {
		t.Fatalf("headers %v body %s", res.Header, b)
	}
	calls := e.seen()
	if len(calls) != 2 || calls[1].path != "/messages" {
		t.Fatalf("calls %v", calls)
	}
	if chats := events(t, e.log, "chat"); len(chats) != 2 || chats[1]["api"] != "anthropic" || chats[1]["escalated_from"] != from {
		t.Fatalf("chat events: %v", chats)
	}
}

func TestAnthropicText(t *testing.T) {
	if s, ok := anthropicText([]byte(`{"content":[{"type":"thinking","thinking":"x"},{"type":"text","text":"a"},{"type":"text","text":"b"}],"stop_reason":"end_turn"}`)); !ok || s != "ab" {
		t.Fatalf("text: %q %v", s, ok)
	}
	if _, ok := anthropicText([]byte(`{"content":[{"type":"text","text":"let me look"},{"type":"tool_use","id":"t"}],"stop_reason":"tool_use"}`)); ok {
		t.Fatal("tool_use turn must not be checked")
	}
	if _, ok := anthropicText([]byte(`{"content":[{"type":"text","text":"cut off mid-s"}],"stop_reason":"max_tokens"}`)); ok {
		t.Fatal("answer truncated by max_tokens must not be checked")
	}
}

func TestReadMessagesStickyKeyAndToolResultPrivacy(t *testing.T) {
	sys := `"system":[{"type":"text","text":"You are Claude Code."}]`
	first := `{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"fix the bug"}]}`
	turn1 := `{` + sys + `,"messages":[` + first + `]}`
	turn3 := `{` + sys + `,"messages":[` + first + `,
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"path":".env"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"AWS_KEY=AKIAABCDEFGHIJKLMNOP"},{"type":"text","text":"<system-reminder>todo</system-reminder>","cache_control":{"type":"ephemeral"}}]}]}`
	_, r1, _ := readMessages(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(turn1)))
	_, r3, _ := readMessages(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(turn3)))
	if r1.StickyKey() != r3.StickyKey() || r3.UserTurns != 2 {
		t.Fatalf("sticky key changed as the conversation grew: %+v / %+v", r1, r3)
	}
	if r1.LooksPrivate() || !r3.LooksPrivate() || strings.Contains(r3.LastUser+r3.FirstUser, "AKIA") {
		t.Fatalf("tool-result secret: r1 %v r3 %v", r1.LooksPrivate(), r3.LooksPrivate())
	}
}

func TestAnthropicStreamUsageMerge(t *testing.T) {
	// message_delta may repeat input_tokens without the cache counts: message_start's total must stand.
	u := usage{}.merge(anthropicUsage([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":1}}}`)))
	u = u.merge(anthropicUsage([]byte(`{"type":"message_delta","usage":{"input_tokens":10,"output_tokens":20,"cost":0.002}}`)))
	if u != (usage{CostUSD: 0.002, PromptTokens: 100, CompletionTokens: 20}) {
		t.Fatalf("merged usage: %+v", u)
	}
}

func TestMessagesUpstreamErrorAnthropicShape(t *testing.T) {
	e := newAnthropicServer(t, "openai/gpt-5.6-luna", nil) // pass-through: one attempt, 429 with no body
	for _, stream := range []string{"false", "true"} {
		res := postH(t, e.gw.URL+"/v1/messages", `{"model":"openai/gpt-5.6-luna","stream":`+stream+`,"messages":[{"role":"user","content":"x"}]}`, nil)
		var body struct {
			Type  string `json:"type"`
			Error struct{ Type, Message string }
		}
		json.NewDecoder(res.Body).Decode(&body)
		if res.StatusCode != 429 || body.Type != "error" || body.Error.Type != "rate_limit_error" || body.Error.Message == "" ||
			res.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("stream=%s: %d %v %+v", stream, res.StatusCode, res.Header, body)
		}
	}
	if b := anthropicErrorBody(400, []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"m"}}`)); !strings.Contains(string(b), `"message":"m"`) {
		t.Fatalf("anthropic-shaped body rewritten: %s", b)
	}
	if b := anthropicErrorBody(502, []byte(`{"error":{"message":"provider down","code":502}}`)); !strings.Contains(string(b), `"api_error"`) || !strings.Contains(string(b), "provider down") {
		t.Fatalf("openrouter body: %s", b)
	}
}
