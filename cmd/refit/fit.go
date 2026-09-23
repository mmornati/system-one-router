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
	MinN      float64 // effective sample size (sum of outcome weights) needed before a skill changes
	MinDelta  float64 // |fitted - seed| needed before a skill changes
	Prior     float64 // weight of the seed skill, in observations
	Scale     float64 // logistic scale τ
	Target    float64 // success rate at difficulty == skill
	WCheck    float64 // weight of an answer check (label: p_ok)
	WFeedback float64 // weight of a user rating (label: good=1, bad=0); strongest signal
}

var defaultParams = Params{MinN: 20, MinDelta: 0.02, Prior: 10, Scale: 0.1, Target: 0.8, WCheck: 1, WFeedback: 2}

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

// obs is one weighted outcome of one answer: y in [0,1] at difficulty d.
type obs struct{ d, y, w float64 }

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
// Selection bias. The router only sends a model the prompts whose floor it clears, so a cheap model's raw
// success rate comes from easy prompts and says little about harder ones. So each outcome is scored against
// the difficulty it was routed at, d = min_skill[complexity] + risk_bonus[risk] (the floor, on the same
// 0..1 scale as skills), with a one-parameter logistic (Rasch / Elo-style) model:
//
//	P(success | skill s, difficulty d) = σ((s - d)/Scale + logit(Target))
//
// i.e. a model is expected to succeed Target (80%) of the time on prompts exactly at its skill. Passing
// easy prompts is then weak evidence (it was expected anyway) while failing them is strong evidence, and
// passing hard prompts moves the skill up a lot. The seed skill from config.yaml is the prior: Prior pseudo-
// observations of success rate Target at difficulty = seed, so with no data the fit is the seed exactly.
// The MAP skill solves one monotone equation, found by bisection on [0,1].
//
// default_skill is fitted the same way from the outcomes on topics the model has no explicit skill for
// (those are the topics it governs).
func Fit(cfg *config.Config, lg *Log, p Params) *Result {
	res := &Result{Reliability: map[string]*Reliability{}}
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

	type key struct{ model, topic string }
	data := map[key][]obs{}
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
		var labels []obs
		diff := difficulty(cfg, d.Signals)
		if y, ok := lg.Checks[c.ID+"\x00"+c.Model]; ok {
			labels = append(labels, obs{diff, clamp01(y), p.WCheck})
			res.Checks++
		}
		if y, ok := lg.Feedback[c.ID]; ok && final[c.ID] == c.Model {
			labels = append(labels, obs{diff, y, p.WFeedback})
			res.Feedback++
		}
		if len(labels) == 0 {
			continue
		}
		res.Outcomes++
		m := cfg.Model(c.Model)
		for t, pt := range d.Signals.Topics {
			if _, known := cfg.Topics[t]; !known || pt <= 0 {
				continue
			}
			for _, o := range labels {
				o.w *= pt
				data[key{m.ID, t}] = append(data[key{m.ID, t}], o)
				if _, explicit := m.Skills[t]; !explicit {
					data[key{m.ID, ""}] = append(data[key{m.ID, ""}], o)
				}
			}
		}
	}

	for _, m := range cfg.Models {
		topics := make([]string, 0, len(cfg.Topics)+1)
		for t := range cfg.Topics {
			topics = append(topics, t)
		}
		sort.Strings(topics)
		for _, t := range append(topics, "") {
			pts := data[key{m.ID, t}]
			if len(pts) == 0 {
				continue
			}
			seed := m.DefaultSkill
			if t != "" {
				seed = m.Skill(t)
			}
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
			res.Fits = append(res.Fits, f)
		}
	}
	return res
}

// fitSkill returns the MAP skill: the root of the score equation
//
//	Prior·(Target − P(seed)) + Σ w·(y − P(d)) = 0,   P(x) = σ((s − x)/Scale + logit(Target))
//
// which is strictly decreasing in s, so bisection on [0,1] finds it (clamped at the ends).
func fitSkill(pts []obs, seed float64, p Params) float64 {
	bias := math.Log(p.Target / (1 - p.Target))
	prob := func(s, d float64) float64 { return 1 / (1 + math.Exp(-((s-d)/p.Scale + bias))) }
	g := func(s float64) float64 {
		v := p.Prior * (p.Target - prob(s, seed))
		for _, o := range pts {
			v += o.w * (o.y - prob(s, o.d))
		}
		return v
	}
	lo, hi := 0.0, 1.0
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
