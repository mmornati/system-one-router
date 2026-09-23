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

func (b *logBuilder) fit(t *testing.T) *Result {
	t.Helper()
	lg, err := ReadLog(bytes.NewReader(b.buf.Bytes()), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return Fit(testConfig(t), lg, defaultParams)
}

func row(r *Result, model, topic string) *FitRow {
	for i, f := range r.Fits {
		if f.Model == model && f.Topic == topic {
			return &r.Fits[i]
		}
	}
	return nil
}

func TestFitChecksMoveSkill(t *testing.T) {
	var b logBuilder
	docs := map[string]float64{"docs": 1}
	for range 40 { // deepseek passes every simple docs check...
		id := b.chat(deepseek, docs, 1, 0, nil)
		b.event("check", map[string]any{"id": id, "model": deepseek, "p_ok": 0.97, "passed": true})
	}
	for range 40 { // ...and fails every simple debugging check.
		id := b.chat(deepseek, map[string]float64{"debugging": 1}, 1, 0, nil)
		b.event("check", map[string]any{"id": id, "model": deepseek, "p_ok": 0.1, "passed": false, "escalated_to": sonnet})
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
	// Selection bias: passing easy prompts is weak evidence, failing them strong.
	if up.Fitted-up.Seed >= down.Seed-down.Fitted {
		t.Fatalf("asymmetry expected: up %+v down %+v", up, down)
	}
	if r.Checks != 80 || r.Outcomes != 80 {
		t.Fatalf("counts: %+v", r)
	}
}

func TestFitHardPromptsCountMore(t *testing.T) {
	fitted := func(cx int) float64 {
		var b logBuilder
		for range 30 {
			id := b.chat(deepseek, map[string]float64{"docs": 1}, cx, 0, nil)
			b.event("feedback", map[string]any{"id": id, "rating": "good"})
		}
		return row(b.fit(t), deepseek, "docs").Fitted
	}
	if easy, hard := fitted(0), fitted(3); hard <= easy {
		t.Fatalf("success on hard prompts should raise skill more: easy %.2f hard %.2f", easy, hard)
	}
}

func TestFitBelowMinN(t *testing.T) {
	var b logBuilder
	for range 5 { // 5 checks + 5 feedback×2 = 15 < 20
		id := b.chat(qwen, map[string]float64{"chat": 1}, 0, 0, nil)
		b.event("check", map[string]any{"id": id, "model": qwen, "p_ok": 0.0})
		b.event("feedback", map[string]any{"id": id, "rating": "bad"})
	}
	r := b.fit(t)
	f := row(r, qwen, "chat")
	if f == nil || f.N != 15 || f.Change || len(r.Changes()) != 0 || f.Fitted >= f.Seed {
		t.Fatalf("below min-n: %+v", f)
	}
}

func TestFitTopicWeightsAndDefault(t *testing.T) {
	var b logBuilder
	for range 50 { // qwen has no explicit code-gen skill: it feeds default_skill too
		id := b.chat(qwen, map[string]float64{"chat": 0.6, "code-gen": 0.4}, 0, 0, nil)
		b.event("check", map[string]any{"id": id, "model": qwen, "p_ok": 0.0})
	}
	r := b.fit(t)
	if f := row(r, qwen, "chat"); f == nil || f.N != 30 {
		t.Fatalf("chat weight: %+v", f)
	}
	cg, def := row(r, qwen, "code-gen"), row(r, qwen, "")
	if cg == nil || def == nil || cg.N != 20 || def.N != 20 || !def.Change || def.Fitted >= def.Seed {
		t.Fatalf("default: %+v %+v", cg, def)
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
	r := b.fit(t)
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
