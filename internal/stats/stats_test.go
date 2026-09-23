package stats

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mmornati/system-one-router/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Models: []config.Model{
			{ID: "cheap/model", Price: config.Price{In: 0.03, Out: 0.13}},
			{ID: "mid/model", Price: config.Price{In: 0.2, Out: 1.2}},
			{ID: "pricey/model", Price: config.Price{In: 4, Out: 20}},
		},
	}
}

// fixture is a synthetic decisions.jsonl covering each aggregate: a routed chat request, a
// passthrough chat request, a chat request with retries and an error, a route_dry, a refused
// request, a shadow agreement and a shadow error, feedback joined to a chat id, a check with an
// escalation, and one malformed line that must be tolerated.
const fixture = `
{"ts":"2026-09-20T10:00:00Z","kind":"chat","data":{"id":"r1","decision":{"id":"r1","model":"cheap/model","reason":"cheapest model above quality floor","decision_provider":"jev","decision_cost_usd":0.0001,"decision_ms":250,"signals":{"primary":"code-gen","complexity":1,"risk":0}},"model":"cheap/model","status":200,"cost_usd":0.0005,"prompt_tokens":1000,"completion_tokens":500,"latency_ms":400}}
{"ts":"2026-09-20T10:05:00Z","kind":"chat","data":{"id":"r2","decision":null,"model":"mid/model","status":200,"cost_usd":0.001,"prompt_tokens":800,"completion_tokens":400,"latency_ms":600}}
{"ts":"2026-09-20T11:00:00Z","kind":"chat","data":{"id":"r3","decision":{"id":"r3","model":"mid/model","reason":"escalated: no model clears the floor","decision_provider":"jev","decision_cost_usd":0.0002,"decision_ms":300,"signals":{"primary":"security","complexity":3,"risk":2}},"model":"mid/model","failed":["cheap/model"],"status":500,"cost_usd":0,"prompt_tokens":0,"completion_tokens":0,"latency_ms":100}}
this is not json at all, tolerate it
{"ts":"2026-09-21T09:00:00Z","kind":"route_dry","data":{"id":"d1","model":"cheap/model","reason":"cheapest model above quality floor","decision_provider":"jev","decision_cost_usd":0.00005,"decision_ms":200,"signals":{"primary":"docs","complexity":0,"risk":0}}}
{"ts":"2026-09-21T09:05:00Z","kind":"refused","data":{"id":"f1","reason":"private request and no local model configured","refused":true,"signals":{"primary":"chat","complexity":0,"risk":0}}}
{"ts":"2026-09-21T12:00:00Z","kind":"shadow","data":{"id":"r1","provider":"laya","agree_topic":true,"agree_complexity":false}}
{"ts":"2026-09-21T12:05:00Z","kind":"shadow","data":{"id":"r3","provider":"laya","error":"timeout"}}
{"ts":"2026-09-22T08:00:00Z","kind":"feedback","data":{"id":"r1","rating":"good"}}
{"ts":"2026-09-22T08:05:00Z","kind":"feedback","data":{"id":"r2","rating":"bad","comment":"too slow"}}
{"ts":"2026-09-22T09:00:00Z","kind":"check","data":{"id":"r1","model":"cheap/model","p_ok":0.4,"passed":false,"escalated_to":"mid/model","check_cost_usd":0.0003,"check_ms":150}}
{"ts":"2026-09-22T09:05:00Z","kind":"check","data":{"id":"r2","model":"mid/model","p_ok":0.9,"passed":true,"check_cost_usd":0.0002,"check_ms":120}}
`

func compute(t *testing.T, since time.Time) *Stats {
	t.Helper()
	st, err := Compute(strings.NewReader(fixture), testConfig(), since)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return st
}

func TestTotals(t *testing.T) {
	st := compute(t, time.Time{})
	if st.Totals.Requests != 3 {
		t.Errorf("requests = %d, want 3", st.Totals.Requests)
	}
	if st.Totals.Routed != 2 {
		t.Errorf("routed = %d, want 2", st.Totals.Routed)
	}
	if st.Totals.Passthrough != 1 {
		t.Errorf("passthrough = %d, want 1", st.Totals.Passthrough)
	}
	if st.Totals.Refused != 1 {
		t.Errorf("refused = %d, want 1", st.Totals.Refused)
	}
	if st.Totals.DryRoutes != 1 {
		t.Errorf("dry_routes = %d, want 1", st.Totals.DryRoutes)
	}
	if st.Totals.Errors != 1 {
		t.Errorf("errors = %d, want 1", st.Totals.Errors)
	}
	if got, want := st.Totals.TotalCostUSD, 0.0015; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("total_cost_usd = %v, want %v", got, want)
	}
}

func TestPerModel(t *testing.T) {
	st := compute(t, time.Time{})
	cheap, ok := st.Models["cheap/model"]
	if !ok {
		t.Fatal("missing cheap/model")
	}
	if cheap.Requests != 1 || cheap.CostUSD != 0.0005 || cheap.PromptTokens != 1000 {
		t.Errorf("cheap/model = %+v", cheap)
	}
	if cheap.P50LatencyMs != 400 || cheap.P95LatencyMs != 400 {
		t.Errorf("cheap/model latency = %+v", cheap)
	}
	mid, ok := st.Models["mid/model"]
	if !ok {
		t.Fatal("missing mid/model")
	}
	if mid.Requests != 2 || mid.Errors != 1 {
		t.Errorf("mid/model = %+v", mid)
	}
}

func TestReasons(t *testing.T) {
	st := compute(t, time.Time{})
	if st.Reasons["cheapest"] != 2 { // r1 chat decision + d1 route_dry
		t.Errorf("cheapest = %d, want 2 (%v)", st.Reasons["cheapest"], st.Reasons)
	}
	if st.Reasons["no model cleared the floor"] != 1 {
		t.Errorf("no model cleared the floor = %d, want 1", st.Reasons["no model cleared the floor"])
	}
	if st.Reasons["no capable model"] != 1 {
		t.Errorf("no capable model = %d, want 1 (%v)", st.Reasons["no capable model"], st.Reasons)
	}
}

func TestTopicsAndHistograms(t *testing.T) {
	st := compute(t, time.Time{})
	if st.Topics["code-gen"] != 1 || st.Topics["security"] != 1 || st.Topics["docs"] != 1 || st.Topics["chat"] != 1 {
		t.Errorf("topics = %v", st.Topics)
	}
	if st.Complexity[1] != 1 || st.Complexity[3] != 1 || st.Complexity[0] != 2 {
		t.Errorf("complexity = %v", st.Complexity)
	}
	if st.Risk[2] != 1 || st.Risk[0] != 3 {
		t.Errorf("risk = %v", st.Risk)
	}
}

func TestDecisionLatencyAndCost(t *testing.T) {
	st := compute(t, time.Time{})
	p, ok := st.DecisionLatency["jev"]
	if !ok {
		t.Fatal("missing jev decision latency")
	}
	if p.P50Ms == 0 {
		t.Errorf("p50 = %d, want nonzero", p.P50Ms)
	}
	want := 0.0001 + 0.0002 + 0.00005
	if got := st.Totals.DecisionCostUSD; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("decision_cost_usd = %v, want %v", got, want)
	}
}

func TestDaily(t *testing.T) {
	st := compute(t, time.Time{})
	// Only chat events feed the daily chart; all three in the fixture fall on 2026-09-20.
	if len(st.Daily) != 1 {
		t.Fatalf("daily entries = %d, want 1: %+v", len(st.Daily), st.Daily)
	}
	if st.Daily[0].Date != "2026-09-20" {
		t.Errorf("first day = %s, want 2026-09-20", st.Daily[0].Date)
	}
	day0 := st.Daily[0].Models
	if day0["cheap/model"].Requests != 1 || day0["mid/model"].Requests != 2 {
		t.Errorf("day0 models = %+v", day0)
	}
}

func TestRetries(t *testing.T) {
	st := compute(t, time.Time{})
	if st.Retries.Requests != 1 {
		t.Errorf("retries requests = %d, want 1", st.Retries.Requests)
	}
	if st.Retries.PerModel["cheap/model"] != 1 {
		t.Errorf("retries per model = %v", st.Retries.PerModel)
	}
}

func TestShadow(t *testing.T) {
	st := compute(t, time.Time{})
	laya, ok := st.Shadow["laya"]
	if !ok {
		t.Fatal("missing laya shadow stats")
	}
	if laya.Count != 2 || laya.Errors != 1 {
		t.Errorf("laya = %+v", laya)
	}
	if laya.TopicAgreePct != 100 {
		t.Errorf("topic agree = %v, want 100 (only 1 comparable entry agreed)", laya.TopicAgreePct)
	}
	if laya.ComplexityAgreePct != 0 {
		t.Errorf("complexity agree = %v, want 0", laya.ComplexityAgreePct)
	}
}

func TestChecks(t *testing.T) {
	st := compute(t, time.Time{})
	if st.Checks.Count != 2 || st.Checks.Passed != 1 {
		t.Errorf("checks = %+v", st.Checks)
	}
	if st.Checks.PassRatePct != 50 {
		t.Errorf("pass rate = %v, want 50", st.Checks.PassRatePct)
	}
	if st.Checks.Escalations != 1 {
		t.Errorf("escalations = %d, want 1", st.Checks.Escalations)
	}
	if st.Checks.EscalationPairs["cheap/model -> mid/model"] != 1 {
		t.Errorf("escalation pairs = %v", st.Checks.EscalationPairs)
	}
}

func TestFeedbackJoin(t *testing.T) {
	st := compute(t, time.Time{})
	if st.Feedback.Good != 1 || st.Feedback.Bad != 1 {
		t.Errorf("feedback = %+v", st.Feedback)
	}
	if st.Feedback.PerModel["cheap/model"].Good != 1 {
		t.Errorf("per model good = %+v", st.Feedback.PerModel["cheap/model"])
	}
	if st.Feedback.PerModel["mid/model"].Bad != 1 {
		t.Errorf("per model bad = %+v", st.Feedback.PerModel["mid/model"])
	}
}

func TestSavings(t *testing.T) {
	st := compute(t, time.Time{})
	if st.Savings.PriciestModel != "pricey/model" {
		t.Errorf("priciest = %s, want pricey/model", st.Savings.PriciestModel)
	}
	// r1: 1000 in, 500 out; r2: 800 in, 400 out. r3 has zero tokens, excluded.
	wantCounterfactual := (1000*4+500*20)/1e6 + (800*4+400*20)/1e6
	if got := st.Savings.CounterfactualCostUSD; got < wantCounterfactual-1e-9 || got > wantCounterfactual+1e-9 {
		t.Errorf("counterfactual = %v, want %v", got, wantCounterfactual)
	}
	wantActual := 0.0005 + 0.001
	if got := st.Savings.ActualCostUSD; got < wantActual-1e-9 || got > wantActual+1e-9 {
		t.Errorf("actual = %v, want %v", got, wantActual)
	}
	if st.Savings.SavingsUSD <= 0 || st.Savings.SavingsPct <= 0 {
		t.Errorf("expected positive savings, got %+v", st.Savings)
	}
}

func TestSinceFilter(t *testing.T) {
	since := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	st := compute(t, since)
	if st.Totals.Requests != 0 {
		t.Errorf("requests after filter = %d, want 0 (all chat events are on 09-20)", st.Totals.Requests)
	}
	if st.Totals.DryRoutes != 1 || st.Totals.Refused != 1 {
		t.Errorf("dry/refused after filter = %d/%d, want 1/1", st.Totals.DryRoutes, st.Totals.Refused)
	}
}

func TestEmptyLog(t *testing.T) {
	st, err := Compute(strings.NewReader(""), testConfig(), time.Time{})
	if err != nil {
		t.Fatalf("Compute empty: %v", err)
	}
	if st.Totals.Requests != 0 || len(st.Daily) != 0 {
		t.Errorf("expected empty stats, got %+v", st.Totals)
	}
}

// An escalated request logs two chat events with the same id: one request, two calls' spend.
func TestEscalatedRequestCountedOnce(t *testing.T) {
	events := `
{"ts":"2026-09-20T10:00:00Z","kind":"chat","data":{"id":"e1","decision":{"id":"e1","model":"cheap/model","reason":"cheapest model above quality floor","decision_provider":"jev","decision_cost_usd":0.0001,"decision_ms":250,"signals":{"primary":"code-gen","complexity":1,"risk":0}},"model":"cheap/model","status":200,"cost_usd":0.001,"prompt_tokens":1000,"completion_tokens":500,"latency_ms":400}}
{"ts":"2026-09-20T10:00:01Z","kind":"chat","data":{"id":"e1","decision":{"id":"e1","model":"cheap/model","reason":"cheapest model above quality floor","decision_provider":"jev","decision_cost_usd":0.0001,"decision_ms":250,"signals":{"primary":"code-gen","complexity":1,"risk":0}},"model":"mid/model","status":200,"cost_usd":0.002,"prompt_tokens":1000,"completion_tokens":600,"latency_ms":900,"escalated_from":"cheap/model"}}
{"ts":"2026-09-20T10:00:01Z","kind":"check","data":{"id":"e1","model":"cheap/model","p_ok":0.2,"passed":false,"escalated_to":"mid/model","check_cost_usd":0.00004,"check_ms":300,"provider":"jev"}}
{"ts":"2026-09-20T10:01:00Z","kind":"chat","data":{"id":"e2","decision":{"id":"e2","model":"cheap/model","reason":"cheapest model above quality floor","signals":{"primary":"chat","complexity":0,"risk":0}},"model":"cheap/model","status":200,"cost_usd":0.001,"prompt_tokens":10,"completion_tokens":5,"latency_ms":100}}
{"ts":"2026-09-20T10:01:01Z","kind":"chat","data":{"id":"e2","decision":{"id":"e2","model":"cheap/model","reason":"cheapest model above quality floor","signals":{"primary":"chat","complexity":0,"risk":0}},"model":"mid/model","status":500,"cost_usd":0,"latency_ms":50,"escalated_from":"cheap/model"}}
{"ts":"2026-09-20T10:01:01Z","kind":"check","data":{"id":"e2","model":"cheap/model","p_ok":0.3,"passed":false,"escalated_to":"","check_cost_usd":0.00004,"check_ms":300,"provider":"jev"}}
{"ts":"2026-09-20T10:02:00Z","kind":"check","data":{"id":"e3","model":"cheap/model","error":"timeout"}}
{"ts":"2026-09-20T10:03:00Z","kind":"feedback","data":{"id":"e1","rating":"good"}}
`
	st, err := Compute(strings.NewReader(events), testConfig(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	tt := st.Totals
	if tt.Requests != 2 || tt.Routed != 2 || tt.Errors != 0 || math.Abs(tt.TotalCostUSD-0.004) > 1e-12 || math.Abs(tt.DecisionCostUSD-0.0001) > 1e-12 {
		t.Errorf("totals = %+v", tt)
	}
	if st.Reasons["cheapest"] != 2 || st.Topics["code-gen"] != 1 || st.Complexity[1] != 1 {
		t.Errorf("decision counted per call: reasons %v topics %v complexity %v", st.Reasons, st.Topics, st.Complexity)
	}
	if m := st.Models["mid/model"]; m.Requests != 2 || m.CostUSD != 0.002 || m.CompletionTokens != 600 || m.Errors != 1 {
		t.Errorf("escalated calls not metered per model: %+v", m)
	}
	if st.Daily[0].Models["mid/model"].CostUSD != 0.002 {
		t.Errorf("daily = %+v", st.Daily)
	}
	if math.Abs(st.Savings.ActualCostUSD-0.004) > 1e-12 {
		t.Errorf("savings actual = %v", st.Savings.ActualCostUSD)
	}
	if c := st.Checks; c.Count != 2 || c.Errors != 1 || c.Passed != 0 || c.Escalations != 1 {
		t.Errorf("checks = %+v", c)
	}
	if st.Feedback.PerModel["mid/model"].Good != 1 {
		t.Errorf("feedback should go to the model that answered: %v", st.Feedback.PerModel)
	}
}
