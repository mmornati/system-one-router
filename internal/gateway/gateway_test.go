package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mmornati/system-one-router/internal/config"
	"github.com/mmornati/system-one-router/internal/decision"
	"github.com/mmornati/system-one-router/internal/router"
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
			io.WriteString(w, `data: {"choices":[],"usage":{"cost":0.0042}}`+"\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": model, "choices": []any{}, "usage": map[string]any{"cost": 0.001}})
	}))
}

func newServer(t *testing.T) (*httptest.Server, *[]string, *router.Router) {
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
	gw := httptest.NewServer((&Server{Cfg: cfg, Router: rt, Upstream: func(string) *upstream.Client { return client }}).Handler())
	t.Cleanup(gw.Close)
	return gw, seen, rt
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
