package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"math"
	"sort"
	"time"

	"github.com/mmornati/system-one-router/internal/config"
	"github.com/mmornati/system-one-router/internal/router"
)

// Params are the fitting knobs (all exposed as flags).
type Params struct {
	MinN     float64 // effective sample size (sum of outcome weights) needed before a skill changes
	MinDelta float64 // |fitted - seed| needed before a skill changes
	Prior    float64 // weight of the seed skill, in observations
	// Scale τ: a skill difference of τ multiplies the odds of a good label by e. 0.1 because the min_skill
	// floors are ~0.15 apart, so being one complexity tier better is worth e^1.5 ≈ 4.5× the odds.
	Scale float64
	// Target: success rate at difficulty == skill, the meaning of "clears the floor" (0.8: most answers at
	// the floor should be acceptable). It shapes the prior's curvature, and is the absolute level used with
	// -calibrate=false.
	Target    float64
	WCheck    float64 // weight of an answer check (label: p_ok)
	WFeedback float64 // weight of a user rating (label: good=1, bad=0); strongest signal
	// Calibrate compares each label with the other models' labels at the same source and difficulty instead
	// of with the absolute logistic, so noisy or strict labels do not drag every skill down (see Fit).
	Calibrate bool
}

var defaultParams = Params{MinN: 20, MinDelta: 0.02, Prior: 10, Scale: 0.1, Target: 0.8, WCheck: 1, WFeedback: 2, Calibrate: true}

// Label sources, calibrated separately (a judge's p_ok and a user's thumbs have different scales).
const (
	srcCheck = iota
	srcFeedback
	nSrc
)

var srcNames = [nSrc]string{"check", "feedback"}

// Log is what refit needs from decisions.jsonl.
type Log struct {
	Chats    []chatEvent
	Checks   map[string]float64 // id + "\x00" + model -> p_ok
	Feedback map[string]float64 // id -> 1 good / 0 bad (last rating wins)
}

type chatEvent struct {
	ID            string           `json:"id"`
	Decision      *router.Decision `json:"decision"`
	Model         string           `json:"model"`
	Failed        []string         `json:"failed"`
	Status        int              `json:"status"`
	EscalatedFrom string           `json:"escalated_from"`
}

// ReadLog parses chat, check and feedback events newer than since (zero: all). Malformed lines are skipped.
func ReadLog(r io.Reader, since time.Time) (*Log, error) {
	lg := &Log{Checks: map[string]float64{}, Feedback: map[string]float64{}}
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			lg.add(line, since)
		}
		if err == io.EOF {
			return lg, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (lg *Log) add(line []byte, since time.Time) {
	var ev struct {
		TS   string          `json:"ts"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	if ts, err := time.Parse(time.RFC3339Nano, ev.TS); err == nil && !since.IsZero() && ts.Before(since) {
		return
	}
	switch ev.Kind {
	case "chat":
		var c chatEvent
		if json.Unmarshal(ev.Data, &c) == nil { // events from before request ids still count for reliability
			lg.Chats = append(lg.Chats, c)
		}
	case "check":
		var c struct {
			ID, Model, Error string
			POK              *float64 `json:"p_ok"`
		}
		if json.Unmarshal(ev.Data, &c) == nil && c.Error == "" && c.POK != nil {
			lg.Checks[c.ID+"\x00"+c.Model] = *c.POK
		}
	case "feedback":
		var f struct{ ID, Rating string }
		if json.Unmarshal(ev.Data, &f) != nil {
			return
		}
		switch f.Rating {
		case "good":
			lg.Feedback[f.ID] = 1
		case "bad":
			lg.Feedback[f.ID] = 0
		}
	}
}

// obs is one weighted outcome of one answer: label y in [0,1] from source src at difficulty d. s0 is the
// answering model's seed skill for the answer's topic mix; peer is the other models' mean label at the same
// source and difficulty (0 when uncalibrated).
type obs struct {
	d, y, w, s0, peer float64
	src               int
}

// Calibration summarises the labels of one source.
type Calibration struct {
	N         float64 // total label weight
	MeanLabel float64 // weighted mean label
	MeanSeed  float64 // weighted mean success the absolute (uncalibrated) model predicts from the seeds
	Buckets   int     // difficulty levels seen
	Solo      float64 // share of label weight at difficulties where only one model was seen (no peers: no signal)
}

// FitRow is the fitted skill of one model on one topic ("" = the model's default_skill).
type FitRow struct {
	Model, Topic string
	N            float64 // effective sample size: sum of outcome weights × topic probability
	Success      float64 // weighted raw success rate
	Seed, Fitted float64
	Change       bool
}

type Reliability struct{ Attempts, Failed int }

type Result struct {
	Chats, Routed, Outcomes, Checks, Feedback int
	Fits                                      []FitRow
	Reliability                               map[string]*Reliability
	Calibration                               map[string]Calibration // by label source
}

// Changes returns the rows that clear -min-n and -min-delta.
func (r *Result) Changes() []FitRow {
	var out []FitRow
	for _, f := range r.Fits {
		if f.Change {
			out = append(out, f)
		}
	}
	return out
}

// Fit turns logged outcomes into skill estimates.
//
// Outcomes. Each answer from a routed (non-sticky, non-pass-through) chat event can carry up to two labels:
// its answer check (y = p_ok, a soft label, weight WCheck) and, for the final answer of a request, the
// user's rating (good 1 / bad 0, weight WFeedback). When a check escalated the request, the check labels
// the first model and the feedback the model that answered last. Upstream failures (429/5xx, models in
// `failed`) say nothing about quality: they only go to the reliability table. Other 4xx are skipped.
// An answer counts toward every topic in proportion to the logged topic probability.
//
// Selection bias and label scale. The router only sends a model the prompts whose floor it clears, so a
// cheap model's raw success rate comes from easy prompts. And labels are noisy and not on an absolute
// scale: p_ok rarely gets near 1, ratings skew to "bad". So a label is never read as an absolute success
// rate. It is compared with the labels of the other models at the same source and difficulty,
// d = min_skill[complexity] + risk_bonus[risk] (the floor the answer was routed against):
//
//	E[label | skill s] = σ(logit(peer) + (s − seed)/Scale)
//
// where peer is the other models' mean label there. A model whose labels are as good as its peers' keeps
// its seed. Labels better or worse than the peers' move it, on a logistic slope: beating peers who already
// score 0.97 is little headroom and weak evidence, and falling behind them is strong evidence. A difficulty
// where no other model was seen carries no signal. The absolute level of the skills therefore stays
// anchored to the seeds, which judge labels alone cannot identify.
//
// With -calibrate=false the label is read as an absolute success probability instead (a Rasch/Elo-style
// model: σ((s − d)/Scale + logit(Target))). That drags every skill down whenever labels are below ~0.95.
//
// The seed skill from config.yaml is the prior: Prior pseudo-observations that expect exactly Target
// success at s = seed, so with no data the fit is the seed. The MAP skill solves one monotone equation,
// found by bisection on [0,1].
//
// default_skill is fitted the same way from the outcomes on topics the model has no explicit skill for
// (the topics it governs), leaving out topics that get their own new skill, so no outcome is used twice.
func Fit(cfg *config.Config, lg *Log, p Params) *Result {
	res := &Result{Reliability: map[string]*Reliability{}, Calibration: map[string]Calibration{}}
	rel := func(m string) *Reliability {
		if res.Reliability[m] == nil {
			res.Reliability[m] = &Reliability{}
		}
		return res.Reliability[m]
	}

	// The final answer of each request (the escalated one if any) is what feedback rates.
	final := map[string]string{}
	for _, c := range lg.Chats {
		if c.ID != "" && ok2xx(c.Status) && (final[c.ID] == "" || c.EscalatedFrom != "") {
			final[c.ID] = c.Model
		}
	}

	type answer struct {
		m      *config.Model
		topics map[string]float64
		labels []obs
	}
	var answers []answer
	for _, c := range lg.Chats {
		res.Chats++
		for _, m := range c.Failed {
			rel(m).Attempts++
			rel(m).Failed++
		}
		if c.Model != "" {
			rel(c.Model).Attempts++
			if c.Status >= 500 || c.Status == 429 {
				rel(c.Model).Failed++
			}
		}
		d := c.Decision
		if d == nil || d.Sticky || d.Signals == nil || cfg.Model(c.Model) == nil {
			continue
		}
		res.Routed++
		if !ok2xx(c.Status) || c.ID == "" {
			continue // availability (5xx/429) or a bad request (other 4xx): not a quality signal
		}
		m := cfg.Model(c.Model)
		topics := map[string]float64{}
		var s0, sum float64
		for t, pt := range d.Signals.Topics {
			if _, known := cfg.Topics[t]; known && pt > 0 {
				topics[t] = pt
				s0 += pt * m.Skill(t)
				sum += pt
			}
		}
		if sum == 0 {
			continue
		}
		s0 /= sum
		var labels []obs
		diff := difficulty(cfg, d.Signals)
		if y, ok := lg.Checks[c.ID+"\x00"+c.Model]; ok {
			labels = append(labels, obs{d: diff, y: clamp01(y), w: p.WCheck, s0: s0, src: srcCheck})
			res.Checks++
		}
		if y, ok := lg.Feedback[c.ID]; ok && final[c.ID] == c.Model {
			labels = append(labels, obs{d: diff, y: y, w: p.WFeedback, s0: s0, src: srcFeedback})
			res.Feedback++
		}
		if len(labels) == 0 {
			continue
		}
		res.Outcomes++
		answers = append(answers, answer{m, topics, labels})
	}

	// Peers: per (label source, difficulty), the labels of every model, to compare each model with the others.
	type bucket struct {
		src int
		d   float64
	}
	type sums struct{ w, wy float64 }
	all, byModel := map[bucket]sums{}, map[bucket]map[string]sums{}
	for _, a := range answers {
		for _, o := range a.labels {
			k := bucket{o.src, o.d}
			s := all[k]
			s.w, s.wy = s.w+o.w, s.wy+o.w*o.y
			all[k] = s
			if byModel[k] == nil {
				byModel[k] = map[string]sums{}
			}
			s = byModel[k][a.m.ID]
			s.w, s.wy = s.w+o.w, s.wy+o.w*o.y
			byModel[k][a.m.ID] = s
		}
	}
	cal := map[int]*Calibration{}
	for k, s := range all {
		c := cal[k.src]
		if c == nil {
			c = &Calibration{}
			cal[k.src] = c
		}
		c.Buckets++
		c.N += s.w
		c.MeanLabel += s.wy
		if len(byModel[k]) < 2 {
			c.Solo += s.w
		}
	}
	for _, a := range answers {
		for _, o := range a.labels {
			cal[o.src].MeanSeed += o.w * prob(o.s0, o.d, p)
		}
	}
	for src, c := range cal {
		c.MeanLabel, c.MeanSeed, c.Solo = c.MeanLabel/c.N, c.MeanSeed/c.N, c.Solo/c.N
		res.Calibration[srcNames[src]] = *c
	}

	type key struct{ model, topic string }
	data := map[key][]obs{}
	for _, a := range answers {
		for _, o := range a.labels {
			if p.Calibrate {
				// Leave-one-model-out peer mean at this source and difficulty.
				k := bucket{o.src, o.d}
				mine, tot := byModel[k][a.m.ID], all[k]
				if tot.w-mine.w < 1e-9 {
					continue // no other model seen here: nothing to compare with
				}
				o.peer = math.Max(0.02, math.Min(0.98, (tot.wy-mine.wy)/(tot.w-mine.w)))
			}
			for t, pt := range a.topics {
				o2 := o
				o2.w *= pt
				data[key{a.m.ID, t}] = append(data[key{a.m.ID, t}], o2)
			}
		}
	}

	row := func(m config.Model, t string, seed float64, pts []obs) FitRow {
		f := FitRow{Model: m.ID, Topic: t, Seed: seed, Fitted: fitSkill(pts, seed, p)}
		var sy float64
		for _, o := range pts {
			f.N += o.w
			sy += o.w * o.y
		}
		f.Success = round(sy/f.N, 1e6)
		f.N = round(f.N, 1e6) // drop float noise from fractional topic weights
		f.Fitted = round(f.Fitted, 100)
		f.Change = f.N >= p.MinN && math.Abs(f.Fitted-f.Seed) >= p.MinDelta-1e-9
		return f
	}
	topics := make([]string, 0, len(cfg.Topics))
	for t := range cfg.Topics {
		topics = append(topics, t)
	}
	sort.Strings(topics)
	for _, m := range cfg.Models {
		var implicit []obs // outcomes on topics still governed by default_skill
		for _, t := range topics {
			pts := data[key{m.ID, t}]
			if len(pts) == 0 {
				continue
			}
			f := row(m, t, m.Skill(t), pts)
			res.Fits = append(res.Fits, f)
			if _, explicit := m.Skills[t]; !explicit && !f.Change {
				implicit = append(implicit, pts...)
			}
		}
		if len(implicit) > 0 {
			res.Fits = append(res.Fits, row(m, "", m.DefaultSkill, implicit))
		}
	}
	return res
}

// prob is the absolute (uncalibrated) success probability of skill s at difficulty d:
// σ((s − d)/Scale + logit(Target)).
func prob(s, d float64, p Params) float64 { return sigmoid((s-d)/p.Scale + logit(p.Target)) }

// expected is the label a model with skill s is expected to get on o, when its seed skill is seed. Calibrated
// (o.peer > 0): the peers' mean label when s == seed, moving with s on the logistic's slope. Otherwise the
// absolute prob.
func expected(o obs, s, seed float64, p Params) float64 {
	if o.peer > 0 {
		return sigmoid(logit(o.peer) + (s-seed)/p.Scale)
	}
	return prob(s, o.d, p)
}

// fitSkill returns the MAP skill: the root of the score equation
//
//	Prior·(Target − σ(logit(Target) + (s − seed)/Scale)) + Σ w·(y − expected(s)) = 0
//
// which is strictly decreasing in s, so bisection on [0,1] finds it (clamped at the ends).
func fitSkill(pts []obs, seed float64, p Params) float64 {
	return bisect(0, 1, func(s float64) float64 {
		v := p.Prior * (p.Target - sigmoid(logit(p.Target)+(s-seed)/p.Scale))
		for _, o := range pts {
			v += o.w * (o.y - expected(o, s, seed, p))
		}
		return v
	})
}

// bisect finds the root of a decreasing g on [lo, hi], clamped to the ends.
func bisect(lo, hi float64, g func(float64) float64) float64 {
	if g(lo) <= 0 {
		return lo
	}
	if g(hi) >= 0 {
		return hi
	}
	for range 60 {
		mid := (lo + hi) / 2
		if g(mid) > 0 {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }

func logit(p float64) float64 { return math.Log(p / (1 - p)) }

// difficulty is the quality floor a request was routed against, without the low-confidence bump
// (that reflects the decision model's doubt, not the prompt).
func difficulty(cfg *config.Config, s *router.Signals) float64 {
	r := cfg.Routing
	return r.MinSkill[clampInt(s.Complexity, len(r.MinSkill)-1)] + r.RiskBonus[clampInt(s.Risk, len(r.RiskBonus)-1)]
}

func ok2xx(status int) bool { return status >= 200 && status < 300 }

func clampInt(v, hi int) int { return max(0, min(v, hi)) }

func round(v, scale float64) float64 { return math.Round(v*scale) / scale }

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }
