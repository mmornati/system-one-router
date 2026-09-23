package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mmornati/system-one-router/internal/config"
	"github.com/mmornati/system-one-router/internal/router"
)

const (
	qwen     = "qwen/qwen3.7-flash"
	deepseek = "deepseek/deepseek-v4.1-flash"
	sonnet   = "anthropic/claude-sonnet-5"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load("../../config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// logBuilder writes synthetic decisions.jsonl events.
type logBuilder struct {
	buf bytes.Buffer
	n   int
}

func (b *logBuilder) event(kind string, data any) {
	line, _ := json.Marshal(map[string]any{"ts": "2026-09-20T10:00:00Z", "kind": kind, "data": data})
	b.buf.Write(append(line, '\n'))
}

// chat logs one routed answer and returns its id; extra fields override the defaults.
func (b *logBuilder) chat(model string, topics map[string]float64, cx, risk int, extra map[string]any) string {
	b.n++
	id := fmt.Sprintf("req-%d", b.n)
	d := &router.Decision{ID: id, Model: model, Reason: "cheapest model above quality floor",
		Signals: &router.Signals{Topics: topics, Confidence: 0.95, Complexity: cx, Risk: risk}}
	ev := map[string]any{"id": id, "decision": d, "model": model, "status": 200}
	for k, v := range extra {
		ev[k] = v
	}
	b.event("chat", ev)
	return id
}

func (b *logBuilder) fit(t *testing.T) *Result { t.Helper(); return b.fitWith(t, defaultParams) }

func (b *logBuilder) fitWith(t *testing.T, p Params) *Result {
	t.Helper()
	lg, err := ReadLog(bytes.NewReader(b.buf.Bytes()), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return Fit(testConfig(t), lg, p)
}

func row(r *Result, model, topic string) *FitRow {
	for i, f := range r.Fits {
		if f.Model == model && f.Topic == topic {
			return &r.Fits[i]
		}
	}
	return nil
}

type route struct {
	topic    string
	cx, risk int
}

// Two routes per model that the router would plausibly give it, overlapping between models so each
// difficulty level has peers.
var routes = map[string][]route{
	qwen:                        {{"chat", 0, 0}, {"writing", 0, 0}},
	deepseek:                    {{"docs", 1, 0}, {"chat", 0, 0}},
	"openai/gpt-5.6-luna":       {{"writing", 1, 0}, {"docs", 0, 0}},
	sonnet:                      {{"debugging", 2, 1}, {"code-gen", 1, 0}},
	"anthropic/claude-opus-5.5": {{"architecture", 3, 1}, {"debugging", 2, 1}},
}

// population logs n answers per model and route, with checks scattered around pOK (±0.05,
// deterministic): the reference the other outcomes are compared with.
func (b *logBuilder) population(n int, pOK float64, models ...string) {
	for i := range n {
		for _, m := range models {
			for _, r := range routes[m] {
				id := b.chat(m, map[string]float64{r.topic: 1}, r.cx, r.risk, nil)
				b.event("check", map[string]any{"id": id, "model": m, "p_ok": pOK + 0.05*float64(i%3-1)})
			}
		}
	}
}

var others = []string{qwen, "openai/gpt-5.6-luna", sonnet, "anthropic/claude-opus-5.5"}

func TestFitChecksMoveSkill(t *testing.T) {
	var b logBuilder
	b.population(30, 0.85, others...)
	docs := map[string]float64{"docs": 1}
	for range 40 { // deepseek passes every simple docs check, better than the other models...
		id := b.chat(deepseek, docs, 1, 0, nil)
		b.event("check", map[string]any{"id": id, "model": deepseek, "p_ok": 0.97, "passed": true})
	}
	for range 40 { // ...and fails most simple debugging checks.
		id := b.chat(deepseek, map[string]float64{"debugging": 1}, 1, 0, nil)
		b.event("check", map[string]any{"id": id, "model": deepseek, "p_ok": 0.3, "passed": false, "escalated_to": sonnet})
	}
	r := b.fit(t)

	up := row(r, deepseek, "docs")
	if up == nil || !up.Change || up.Fitted <= up.Seed || up.N != 40 {
		t.Fatalf("docs should go up: %+v", up)
	}
	down := row(r, deepseek, "debugging")
	if down == nil || !down.Change || down.Fitted >= down.Seed-0.1 {
		t.Fatalf("debugging should drop a lot: %+v", down)
	}
	if r.Checks != 320 || r.Outcomes != 320 {
		t.Fatalf("counts: %+v", r)
	}
}

// The systematic-bias guard: labels well below 1 but no worse than anyone else's must not move skills.
func TestFitUniformLabelsDoNotDrift(t *testing.T) {
	for _, pOK := range []float64{0.6, 0.85} {
		var b logBuilder
		b.population(40, pOK, append(others, deepseek)...)
		r := b.fit(t)
		for _, f := range r.Fits {
			if f.Change || abs(f.Fitted-f.Seed) > 0.015 {
				t.Errorf("p_ok %.2f: %s/%s moved %.2f → %.2f", pOK, f.Model, f.Topic, f.Seed, f.Fitted)
			}
		}
		if c := r.Calibration["check"]; c.MeanLabel >= c.MeanSeed {
			t.Errorf("p_ok %.2f: labels should be below the seed predictions: %+v", pOK, c)
		}

		// Without calibration the same data drags every skill down: the bias being corrected.
		lg, _ := ReadLog(bytes.NewReader(b.buf.Bytes()), time.Time{})
		p := defaultParams
		p.Calibrate = false
		for _, f := range Fit(testConfig(t), lg, p).Fits {
			if f.Fitted >= f.Seed {
				t.Errorf("uncalibrated p_ok %.2f: %s/%s did not drop: %+v", pOK, f.Model, f.Topic, f)
			}
		}
	}
}

// Selection bias: a model is only compared with models that saw the same difficulty.
func TestFitComparesAtSameDifficultyOnly(t *testing.T) {
	var b logBuilder
	b.population(30, 0.85, others...)
	for range 40 { // no other model answered complexity-3, risk-0 prompts: no signal at all
		id := b.chat(deepseek, map[string]float64{"docs": 1}, 3, 0, nil)
		b.event("check", map[string]any{"id": id, "model": deepseek, "p_ok": 0.1})
	}
	if f := row(b.fit(t), deepseek, "docs"); f != nil {
		t.Fatalf("no peers, yet fitted: %+v", f)
	}

	// Where peers exist, doing better or worse than them moves the skill, whatever the absolute label level.
	fitted := func(peer, mine float64) FitRow {
		var b logBuilder
		b.population(30, peer, others...)
		for range 40 {
			id := b.chat(deepseek, map[string]float64{"docs": 1}, 0, 0, nil) // peers here: qwen, luna
			b.event("check", map[string]any{"id": id, "model": deepseek, "p_ok": mine})
		}
		return *row(b.fit(t), deepseek, "docs")
	}
	if f := fitted(0.55, 0.85); !f.Change || f.Fitted <= f.Seed {
		t.Fatalf("better than peers should go up: %+v", f)
	}
	if f := fitted(0.95, 0.65); !f.Change || f.Fitted >= f.Seed {
		t.Fatalf("worse than peers should go down: %+v", f)
	}
}

func TestFitBelowMinN(t *testing.T) {
	var b logBuilder
	b.population(30, 0.85, others[1:]...)
	for range 15 { // 15 < 20
		id := b.chat(qwen, map[string]float64{"chat": 1}, 0, 0, nil)
		b.event("check", map[string]any{"id": id, "model": qwen, "p_ok": 0.0})
	}
	f := row(b.fit(t), qwen, "chat")
	if f == nil || f.N != 15 || f.Change || f.Fitted >= f.Seed {
		t.Fatalf("below min-n: %+v", f)
	}
}

func TestFitTopicWeightsAndDefault(t *testing.T) {
	var b logBuilder
	b.population(30, 0.85, others[1:]...)
	for range 50 { // qwen has no explicit code-gen skill: a new one is proposed...
		id := b.chat(qwen, map[string]float64{"chat": 0.6, "code-gen": 0.4}, 0, 0, nil)
		b.event("check", map[string]any{"id": id, "model": qwen, "p_ok": 0.0})
	}
	r := b.fit(t)
	if f := row(r, qwen, "chat"); f == nil || f.N != 30 {
		t.Fatalf("chat weight: %+v", f)
	}
	// ...and those outcomes are not reused for default_skill.
	if cg, def := row(r, qwen, "code-gen"), row(r, qwen, ""); cg == nil || cg.N != 20 || !cg.Change || cg.Fitted >= cg.Seed || def != nil {
		t.Fatalf("code-gen/default: %+v %+v", cg, def)
	}

	// Spread thinly over implicit topics, no topic reaches min-n: default_skill is re-fitted instead.
	b = logBuilder{}
	b.population(30, 0.85, others[1:]...)
	spread := map[string]float64{"code-gen": 0.25, "debugging": 0.25, "security": 0.25, "infra-devops": 0.25}
	for range 50 {
		id := b.chat(qwen, spread, 0, 0, nil)
		b.event("check", map[string]any{"id": id, "model": qwen, "p_ok": 0.0})
	}
	r = b.fit(t)
	if def := row(r, qwen, ""); def == nil || def.N != 50 || !def.Change || def.Fitted >= def.Seed {
		t.Fatalf("default: %+v", def)
	}
	for _, f := range r.Fits {
		if f.Model == qwen && f.Topic != "" && f.Change {
			t.Fatalf("thin topic changed: %+v", f)
		}
	}
}

func TestFitUpstreamFailuresAreNotQuality(t *testing.T) {
	var b logBuilder
	docs := map[string]float64{"docs": 1}
	for range 30 {
		b.chat(qwen, docs, 0, 0, map[string]any{"status": 502})
		b.chat(qwen, docs, 0, 0, map[string]any{"status": 429})
		id := b.chat(deepseek, docs, 0, 0, map[string]any{"failed": []string{qwen}})
		b.event("check", map[string]any{"id": id, "model": deepseek, "p_ok": 0.9})
		id = b.chat(qwen, docs, 0, 0, map[string]any{"status": 400})
		b.event("feedback", map[string]any{"id": id, "rating": "bad"})
	}
	r := b.fit(t)
	if f := row(r, qwen, "docs"); f != nil {
		t.Fatalf("upstream errors counted as quality: %+v", f)
	}
	if rel := r.Reliability[qwen]; rel == nil || rel.Attempts != 120 || rel.Failed != 90 {
		t.Fatalf("reliability: %+v", rel)
	}
	if rel := r.Reliability[deepseek]; rel.Attempts != 30 || rel.Failed != 0 {
		t.Fatalf("reliability deepseek: %+v", rel)
	}
}

func TestFitEscalationAndSkips(t *testing.T) {
	var b logBuilder
	docs := map[string]float64{"docs": 1}
	for range 25 {
		id := b.chat(deepseek, docs, 1, 0, nil)
		b.event("check", map[string]any{"id": id, "model": deepseek, "p_ok": 0.2, "passed": false, "escalated_to": sonnet})
		d := &router.Decision{ID: id, Model: deepseek, Signals: &router.Signals{Topics: docs, Confidence: 0.95, Complexity: 1}}
		b.event("chat", map[string]any{"id": id, "decision": d, "model": sonnet, "status": 200, "escalated_from": deepseek})
		b.event("feedback", map[string]any{"id": id, "rating": "good"})
	}
	for range 25 { // sticky, pass-through and check errors carry no signal
		b.event("chat", map[string]any{"id": "s", "decision": router.Decision{Model: qwen, Sticky: true}, "model": qwen, "status": 200})
		b.event("chat", map[string]any{"id": "p", "decision": nil, "model": qwen, "status": 200})
		b.event("check", map[string]any{"id": "s", "model": qwen, "error": "timeout"})
		b.event("feedback", map[string]any{"id": "p", "rating": "bad"})
	}
	p := defaultParams
	p.Calibrate = false // no peers in this log; this test is about attribution
	r := b.fitWith(t, p)
	if f := row(r, deepseek, "docs"); f == nil || f.N != 25 || f.Success != 0.2 {
		t.Fatalf("check → first model only: %+v", f)
	}
	if f := row(r, sonnet, "docs"); f == nil || f.N != 50 || f.Success != 1 {
		t.Fatalf("feedback → final model: %+v", f)
	}
	if f := row(r, qwen, "chat"); f != nil || r.Routed != 50 {
		t.Fatalf("sticky/pass-through counted: %+v routed=%d", f, r.Routed)
	}
}

func TestFitNoDataIsSeed(t *testing.T) {
	for _, seed := range []float64{0.2, 0.55, 0.9} {
		if got := fitSkill(nil, seed, defaultParams); abs(got-seed) > 1e-6 {
			t.Fatalf("seed %.2f → %.4f", seed, got)
		}
	}
}

func TestWriteSkillsPreservesComments(t *testing.T) {
	src, err := os.ReadFile("../../config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	changes := []FitRow{
		{Model: qwen, Topic: "chat", Fitted: 0.81},
		{Model: qwen, Topic: "", Fitted: 0.40},
		{Model: qwen, Topic: "code-gen", Fitted: 0.31},
		{Model: qwen, Topic: "architecture", Fitted: 0.22},
		{Model: sonnet, Topic: "docs", Fitted: 0.9},
	}
	out, err := WriteSkills(src, changes)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(
		"skills: {chat: 0.75, writing: 0.60, docs: 0.55}", "skills: {chat: 0.81, writing: 0.60, docs: 0.55, architecture: 0.22, code-gen: 0.31}",
		"default_skill: 0.45", "default_skill: 0.40",
		"data-sql: 0.84}", "data-sql: 0.84, docs: 0.90}",
	).Replace(string(src))
	if string(out) != want {
		t.Fatalf("unexpected output:\n%s", out)
	}

	// The written file still loads, with the new values.
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, out, 0o644)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if m := cfg.Model(qwen); m.Skill("chat") != 0.81 || m.DefaultSkill != 0.40 || m.Skill("code-gen") != 0.31 {
		t.Fatalf("reloaded: %+v", m)
	}
}

func TestWriteSkillsBlockAndMissing(t *testing.T) {
	src := `# models
models:
  - id: a
    default_skill: 0.5 # seed
    skills:
      chat: 0.70  # guess

      docs: 0.60
  - id: b
    price: {in: 1, out: 2}
`
	out, err := WriteSkills([]byte(src), []FitRow{
		{Model: "a", Topic: "chat", Fitted: 0.72}, {Model: "a", Topic: "writing", Fitted: 0.66},
		{Model: "b", Topic: "chat", Fitted: 0.5}, {Model: "b", Topic: "", Fitted: 0.44},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `# models
models:
  - id: a
    default_skill: 0.5 # seed
    skills:
      chat: 0.72  # guess

      docs: 0.60
      writing: 0.66
  - id: b
    default_skill: 0.44
    skills: {chat: 0.50}
    price: {in: 1, out: 2}
`
	if string(out) != want {
		t.Fatalf("got:\n%s", out)
	}
	if _, err := WriteSkills([]byte(src), []FitRow{{Model: "nope", Topic: "chat"}}); err == nil {
		t.Fatal("unknown model should fail")
	}
}

func TestRunRealShapedLog(t *testing.T) {
	dir := t.TempDir()
	var b logBuilder
	b.chat(qwen, map[string]float64{"chat": 1}, 0, 0, nil)
	b.event("route_dry", map[string]any{"id": "x"})
	b.buf.WriteString("not json\n")
	logPath := filepath.Join(dir, "decisions.jsonl")
	os.WriteFile(logPath, b.buf.Bytes(), 0o644)
	var out bytes.Buffer
	outCfg := filepath.Join(dir, "out.yaml")
	if err := run(&out, "../../config.yaml", logPath, 0, defaultParams, outCfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not enough data") {
		t.Fatalf("output:\n%s", out.String())
	}
	if _, err := os.Stat(outCfg); !os.IsNotExist(err) {
		t.Fatal("nothing to change: no file should be written")
	}
}

func abs(v float64) float64 { return max(v, -v) }
