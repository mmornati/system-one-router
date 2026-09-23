package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mmornati/system-one-router/internal/config"
	"github.com/mmornati/system-one-router/internal/decision"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestScoreTiers(t *testing.T) {
	cfg := testConfig(t)
	cases := []struct {
		name     string
		topic    string
		cx, risk int
		conf     float64
		want     string
	}{
		{"trivial chat", "chat", 0, 0, 0.99, "qwen/qwen3.7-flash"},
		{"simple code", "code-gen", 1, 0, 0.99, "deepseek/deepseek-v4.1-flash"},
		{"substantial debugging", "debugging", 2, 1, 0.99, "anthropic/claude-sonnet-5"},
		{"hard architecture", "architecture", 3, 1, 0.99, "anthropic/claude-opus-5.5"},
		{"low confidence bumps tier", "code-gen", 1, 0, 0.4, "openai/gpt-5.6-luna"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sig := Signals{Topics: map[string]float64{c.topic: 1}, Primary: c.topic, Confidence: c.conf, Complexity: c.cx, Risk: c.risk}
			best, _, _ := Score(cfg, sig, Needs{InputTokens: 500}, Env{})
			if best == nil || best.ID != c.want {
				t.Fatalf("got %+v, want %s", best, c.want)
			}
		})
	}
}

func TestScoreConstraints(t *testing.T) {
	cfg := testConfig(t)
	sig := Signals{Topics: map[string]float64{"architecture": 1}, Confidence: 1, Complexity: 3, Risk: 2}

	// Opus budget exhausted: escalate to the best remaining model instead of failing.
	env := Env{SpentUSD: func(m string) float64 {
		if m == "anthropic/claude-opus-5.5" {
			return 100
		}
		return 0
	}}
	best, _, _ := Score(cfg, sig, Needs{InputTokens: 500}, env)
	if best.ID != "anthropic/claude-sonnet-5" || !strings.HasPrefix(best.Why, "escalated") {
		t.Fatalf("budget: got %+v", best)
	}

	// Local-only with no local model: nothing capable.
	if best, _, _ := Score(cfg, sig, Needs{LocalOnly: true}, Env{}); best != nil {
		t.Fatalf("local only: expected nil, got %+v", best)
	}

	// Context too large for everything.
	if best, _, _ := Score(cfg, sig, Needs{InputTokens: 5_000_000}, Env{}); best != nil {
		t.Fatalf("context: expected nil, got %+v", best)
	}
}

func TestLoadPenaltyShiftsChoice(t *testing.T) {
	cfg := testConfig(t)
	cfg.Routing.LoadPenalty = 10
	sig := Signals{Topics: map[string]float64{"chat": 1}, Confidence: 1}
	busy := Env{InFlight: func(m string) int {
		if m == "qwen/qwen3.7-flash" {
			return 5
		}
		return 0
	}}
	best, _, _ := Score(cfg, sig, Needs{InputTokens: 500}, busy)
	if best.ID == "qwen/qwen3.7-flash" {
		t.Fatalf("expected load to move traffic away from qwen, got %s", best.ID)
	}
}

func TestRequestStateAndPrivacy(t *testing.T) {
	r := Request{System: strings.Repeat("s", 5000), FirstUser: "hi", LastUser: strings.Repeat("x", 5000), UserTurns: 2}
	st := r.State(1500)
	total := 0
	for _, v := range st {
		total += len(v)
	}
	if total > 1600 {
		t.Fatalf("state too large: %d", total)
	}
	if !(Request{LastUser: "key AKIAIOSFODNN7EXAMPLE leaked"}).LooksPrivate() {
		t.Fatal("aws key not detected")
	}
	if (Request{LastUser: "write a regex for emails"}).LooksPrivate() {
		t.Fatal("false positive")
	}
}

type fakeProvider struct {
	name  string
	local bool
	res   *decision.Result
	err   error
	calls int
}

func (f *fakeProvider) Name() string       { return f.name }
func (f *fakeProvider) Local() bool        { return f.local }
func (f *fakeProvider) MaxStateChars() int { return 4000 }
func (f *fakeProvider) Decide(context.Context, map[string]string, map[string]decision.Question) (*decision.Result, error) {
	f.calls++
	return f.res, f.err
}

func jevAnswer(topic string, conf, cx, risk float64) *decision.Result {
	return &decision.Result{Answers: map[string]decision.Answer{
		"primary_topic": {Choice: topic, Confidence: conf, Probabilities: map[string]float64{topic: conf}},
		"complexity":    {Score: cx},
		"risk":          {Score: risk},
		"private_data":  {Noul: 0.02},
	}}
}

func TestRouteStickyAndFallback(t *testing.T) {
	cfg := testConfig(t)
	jev := &fakeProvider{name: "jev", res: jevAnswer("code-gen", 0.95, 1, 0.3)}
	rt := New(cfg, &decision.Selector{Mode: "jev", Providers: map[string]decision.Provider{"jev": jev}})

	first := rt.Route(context.Background(), Request{FirstUser: "write a csv parser", LastUser: "write a csv parser", UserTurns: 1})
	if first.Model != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("first: %+v", first)
	}
	next := rt.Route(context.Background(), Request{FirstUser: "write a csv parser", LastUser: "now add tests", UserTurns: 2})
	if !next.Sticky || next.Model != first.Model || jev.calls != 1 {
		t.Fatalf("sticky: %+v calls=%d", next, jev.calls)
	}

	jev.err = errors.New("boom")
	fb := rt.Route(context.Background(), Request{FirstUser: "other", LastUser: "other", UserTurns: 1})
	if fb.Model != cfg.Routing.FallbackModel || fb.Error == "" {
		t.Fatalf("fallback: %+v", fb)
	}
}

func TestSelector(t *testing.T) {
	jev, laya := &fakeProvider{name: "jev"}, &fakeProvider{name: "laya", local: true}
	ps := map[string]decision.Provider{"jev": jev, "laya": laya}
	s := &decision.Selector{Mode: "auto", PrivatePolicy: "prefer_local", Providers: ps}
	if p, _ := s.Pick(false); p.Name() != "jev" {
		t.Fatal("auto public should use jev")
	}
	if p, _ := s.Pick(true); p.Name() != "laya" {
		t.Fatal("auto private should use laya")
	}
	strict := &decision.Selector{Mode: "jev", PrivatePolicy: "local_only", Providers: map[string]decision.Provider{"jev": jev}}
	if _, err := strict.Pick(true); err == nil {
		t.Fatal("local_only without local provider must fail")
	}
}
