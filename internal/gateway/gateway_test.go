package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmornati/system-one-router/internal/config"
	"github.com/mmornati/system-one-router/internal/decision"
	"github.com/mmornati/system-one-router/internal/router"
	"github.com/mmornati/system-one-router/internal/store"
	"github.com/mmornati/system-one-router/internal/upstream"
)

// fakeJev answers the Decisions API like Jev does.
func fakeJev(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State map[string]string `json:"state"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		topic, cx := "chat", 0.0
		if strings.Contains(req.State["request"], "microservices") {
			topic, cx = "architecture", 3
		}
		json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{
				"primary_topic": map[string]any{"type": "choice", "choice": topic, "confidence": 0.97, "probabilities": map[string]float64{topic: 0.97}},
				"complexity":    map[string]any{"type": "score", "score": cx},
				"risk":          map[string]any{"type": "score", "score": 1.2},
				"private_data":  map[string]any{"type": "noul", "noul": 0.01},
			},
			"usage": map[string]any{"input_tokens": 400, "cost": 0.0000168},
		})
	}))
}

// fakeUpstream echoes the model it was asked for, streaming or not.
func fakeUpstream(t *testing.T, seen *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		model := body["model"].(string)
		*seen = append(*seen, model)
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
			io.WriteString(w, `data: {"choices":[],"usage":{"cost":0.0042,"prompt_tokens":7,"completion_tokens":21,"completion_tokens_details":{"reasoning_tokens":3}}}`+"\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": model, "choices": []any{}, "usage": map[string]any{
			"cost": 0.001, "prompt_tokens": 12, "completion_tokens": 34,
			"completion_tokens_details": map[string]any{"reasoning_tokens": 5},
		}})
	}))
}

func newServer(t *testing.T) (*httptest.Server, *[]string, *router.Router) {
	gw, seen, rt, _ := newServerWithLog(t)
	return gw, seen, rt
}

// newServerWithLog is newServer plus the event log path, for tests that inspect what got written.
func newServerWithLog(t *testing.T) (*httptest.Server, *[]string, *router.Router, string) {
	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	jev := fakeJev(t)
	t.Cleanup(jev.Close)
	seen := &[]string{}
	up := fakeUpstream(t, seen)
	t.Cleanup(up.Close)
	sel := &decision.Selector{Mode: "jev", Providers: map[string]decision.Provider{
		"jev": decision.NewHTTPProvider("jev", jev.URL, "typesafe/jev-1.13", "k", false, 4000, cfg.Decision.Providers["jev"].Timeout),
	}}
	rt := router.New(cfg, sel)
	client := upstream.New(up.URL, "k")
	logPath := filepath.Join(t.TempDir(), "decisions.jsonl")
	log, err := store.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	gw := httptest.NewServer((&Server{Cfg: cfg, Router: rt, Upstream: func(string) *upstream.Client { return client }, Log: log}).Handler())
	t.Cleanup(gw.Close)
	return gw, seen, rt, logPath
}

// readLog returns the logged events at path, decoded generically.
func readLog(t *testing.T, path string) []map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	return events
}

// waitForLog polls the log file until it holds at least n events, for a few hundred ms: the
// server finishes writing its log line slightly after the response body reaches the client.
func waitForLog(t *testing.T, path string, n int) []map[string]any {
	deadline := time.Now().Add(2 * time.Second)
	for {
		events := readLog(t, path)
		if len(events) >= n || time.Now().After(deadline) {
			return events
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func post(t *testing.T, url, body string) *http.Response {
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestAutoRoutesByComplexity(t *testing.T) {
	gw, seen, _ := newServer(t)
	res := post(t, gw.URL+"/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"hello!"}]}`)
	if res.StatusCode != 200 || res.Header.Get("X-Router-Model") != "qwen/qwen3.7-flash" {
		t.Fatalf("status %d model %q", res.StatusCode, res.Header.Get("X-Router-Model"))
	}
	res = post(t, gw.URL+"/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"split our monolith into microservices?"}]}`)
	if got := res.Header.Get("X-Router-Model"); got != "anthropic/claude-opus-5.5" {
		t.Fatalf("hard request routed to %q", got)
	}
	if len(*seen) != 2 || (*seen)[1] != "anthropic/claude-opus-5.5" {
		t.Fatalf("upstream saw %v", *seen)
	}
}

func TestPassThroughExplicitModel(t *testing.T) {
	gw, seen, _ := newServer(t)
	res := post(t, gw.URL+"/v1/chat/completions", `{"model":"openai/gpt-5.6-luna","messages":[{"role":"user","content":"x"}]}`)
	if res.StatusCode != 200 || res.Header.Get("X-Router-Model") != "" || (*seen)[0] != "openai/gpt-5.6-luna" {
		t.Fatalf("pass-through broken: %v %v", res.Header, *seen)
	}
}

func TestStreamingIsForwardedAndMetered(t *testing.T) {
	gw, _, rt := newServer(t)
	res := post(t, gw.URL+"/v1/chat/completions", `{"model":"auto","stream":true,"messages":[{"role":"user","content":"hello!"}]}`)
	b, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(b), `"content":"hi"`) || !strings.Contains(string(b), "[DONE]") {
		t.Fatalf("stream body: %s", b)
	}
	if got := rt.Tracker.Spent("qwen/qwen3.7-flash"); got != 0.0042 {
		t.Fatalf("spend not metered: %v", got)
	}
}

func TestDryRoute(t *testing.T) {
	gw, seen, _ := newServer(t)
	res := post(t, gw.URL+"/route", `{"messages":[{"role":"user","content":"split into microservices"}]}`)
	var d router.Decision
	json.NewDecoder(res.Body).Decode(&d)
	if d.Model != "anthropic/claude-opus-5.5" || d.Signals.Risk != 1 || len(*seen) != 0 {
		t.Fatalf("dry route: %+v seen=%v", d, *seen)
	}
}

func TestRetryOnRateLimit(t *testing.T) {
	// Upstream rate-limits the cheapest model; the gateway must fall through to the next candidate.
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["model"] == "qwen/qwen3.7-flash" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if body["reasoning"] == nil {
			t.Errorf("reasoning effort not injected")
		}
		json.NewEncoder(w).Encode(map[string]any{"model": body["model"]})
	}))
	defer limited.Close()

	cfg, _ := config.Load("../../config.yaml")
	jev := fakeJev(t)
	defer jev.Close()
	sel := &decision.Selector{Mode: "jev", Providers: map[string]decision.Provider{
		"jev": decision.NewHTTPProvider("jev", jev.URL, "m", "k", false, 4000, cfg.Decision.Providers["jev"].Timeout)}}
	client := upstream.New(limited.URL, "k")
	srv := httptest.NewServer((&Server{Cfg: cfg, Router: router.New(cfg, sel), Upstream: func(string) *upstream.Client { return client }}).Handler())
	defer srv.Close()

	res := post(t, srv.URL+"/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"hello!"}]}`)
	if res.StatusCode != 200 || res.Header.Get("X-Router-Failed") != "qwen/qwen3.7-flash" || res.Header.Get("X-Router-Model") == "qwen/qwen3.7-flash" {
		t.Fatalf("status %d model %q failed %q", res.StatusCode, res.Header.Get("X-Router-Model"), res.Header.Get("X-Router-Failed"))
	}
}

func TestRequestIDHeaderAndLog(t *testing.T) {
	gw, _, _, logPath := newServerWithLog(t)
	res := post(t, gw.URL+"/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"hello!"}]}`)
	id := res.Header.Get("X-Router-Request-Id")
	if id == "" {
		t.Fatal("X-Router-Request-Id header missing")
	}
	io.ReadAll(res.Body)

	var chat map[string]any
	for _, ev := range waitForLog(t, logPath, 1) {
		if ev["kind"] == "chat" {
			chat = ev
		}
	}
	if chat == nil {
		t.Fatal("no chat event logged")
	}
	if chat["data"].(map[string]any)["id"] != id {
		t.Fatalf("logged id %v != header id %q", chat["data"].(map[string]any)["id"], id)
	}
}

func TestUsageTokensExtracted(t *testing.T) {
	gw, _, _, logPath := newServerWithLog(t)

	res := post(t, gw.URL+"/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"hello!"}]}`)
	io.ReadAll(res.Body)
	res2 := post(t, gw.URL+"/v1/chat/completions", `{"model":"auto","stream":true,"messages":[{"role":"user","content":"hello!"}]}`)
	io.ReadAll(res2.Body)

	var nonStream, stream map[string]any
	for _, ev := range waitForLog(t, logPath, 2) {
		if ev["kind"] != "chat" {
			continue
		}
		data := ev["data"].(map[string]any)
		if data["stream"] == true {
			stream = data
		} else {
			nonStream = data
		}
	}
	if nonStream == nil || nonStream["prompt_tokens"] != 12.0 || nonStream["completion_tokens"] != 34.0 || nonStream["reasoning_tokens"] != 5.0 {
		t.Fatalf("non-streaming usage: %+v", nonStream)
	}
	if stream == nil || stream["prompt_tokens"] != 7.0 || stream["completion_tokens"] != 21.0 || stream["reasoning_tokens"] != 3.0 {
		t.Fatalf("streaming usage: %+v", stream)
	}
}

func TestFeedback(t *testing.T) {
	gw, _, _, logPath := newServerWithLog(t)

	res := post(t, gw.URL+"/feedback", `{"id":"abc123","rating":"good","comment":"nice"}`)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("valid feedback: status %d", res.StatusCode)
	}
	if res := post(t, gw.URL+"/feedback", `{"id":"","rating":"good"}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing id: status %d", res.StatusCode)
	}
	if res := post(t, gw.URL+"/feedback", `{"id":"abc123","rating":"meh"}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad rating: status %d", res.StatusCode)
	}

	var fb map[string]any
	for _, ev := range waitForLog(t, logPath, 1) {
		if ev["kind"] == "feedback" {
			fb = ev["data"].(map[string]any)
		}
	}
	if fb == nil || fb["id"] != "abc123" || fb["rating"] != "good" || fb["comment"] != "nice" {
		t.Fatalf("feedback not logged: %+v", fb)
	}
}
