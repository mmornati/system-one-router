// Package router turns a chat request into a model choice:
// local pre-checks → decision model (Jev/Laya) → deterministic scoring over models.yaml.
package router

import (
	"context"
	"math"
	"time"

	"github.com/mmornati/system-one-router/internal/config"
	"github.com/mmornati/system-one-router/internal/decision"
)

var complexityLevels = []string{
	"Trivial: one-liner or quick fact, no reasoning needed",
	"Simple: small self-contained task, a competent junior could do it",
	"Substantial: multi-step reasoning or debugging across several parts",
	"Hard: architectural, large-scope, or expert-level reasoning",
}

var riskLevels = []string{
	"Harmless: mistakes have no real consequence",
	"Moderate: a wrong answer could break code or waste time",
	"High: security, production outage, data loss, or legal exposure",
}

type Decision struct {
	// ID is the request id assigned by the gateway, carried through to every event logged for this request.
	ID              string   `json:"id,omitempty"`
	Model           string   `json:"model"`
	Reason          string   `json:"reason"`
	Sticky          bool     `json:"sticky,omitempty"`
	Provider        string   `json:"decision_provider,omitempty"`
	DecisionCostUSD float64  `json:"decision_cost_usd,omitempty"`
	DecisionMs      int64    `json:"decision_ms,omitempty"`
	Signals         *Signals `json:"signals,omitempty"`
	// Answers are the raw decision-model answers (probabilities and per-question confidence).
	Answers map[string]decision.Answer `json:"answers,omitempty"`
	// State is the state map sent to the decision model, kept for Laya fine-tuning. Omitted for private requests.
	State      map[string]string `json:"state,omitempty"`
	Needs      Needs             `json:"needs"`
	Required   float64           `json:"required_skill,omitempty"`
	Candidates []Candidate       `json:"candidates,omitempty"`
	Error      string            `json:"error,omitempty"`
	// Refused is set when the request must not be forwarded anywhere (private + local_only, no local model).
	Refused bool `json:"refused,omitempty"`
}

type Router struct {
	cfg     *config.Config
	sel     *decision.Selector
	Tracker *Tracker
	// OnShadow receives the primary decision and the shadow provider's result (for agreement logging).
	OnShadow func(primary *Decision, shadow *Signals, shadowProvider string, err error)
}

func New(cfg *config.Config, sel *decision.Selector) *Router {
	return &Router{cfg: cfg, sel: sel, Tracker: NewTracker(cfg.Routing.StickyTTL)}
}

func (r *Router) questions() map[string]decision.Question {
	return map[string]decision.Question{
		"primary_topic": decision.Choice("What is the main kind of work this request asks for?", r.cfg.Topics),
		"complexity":    decision.Score("How much reasoning capability does this request need?", complexityLevels),
		"risk":          decision.Score("How costly would a wrong or low-quality answer be?", riskLevels),
		"private_data": decision.Noul("Does the request contain secrets or personal/confidential data?",
			"Contains credentials, keys, personal identifiers, salaries, customer PII, or confidential data.",
			"No sensitive data is present."),
	}
}

func (r *Router) signals(res *decision.Result) *Signals {
	a := res.Answers
	pt := a["primary_topic"]
	topics := map[string]float64{}
	for t, p := range pt.Probabilities {
		if _, ok := r.cfg.Topics[t]; ok && p > 0 {
			topics[t] = p
		}
	}
	if len(topics) == 0 && pt.Choice != "" {
		topics[pt.Choice] = 1
	}
	conf := pt.Confidence
	if conf == 0 {
		conf = pt.Probabilities[pt.Choice]
	}
	return &Signals{
		Topics: topics, Primary: pt.Choice, Confidence: conf,
		Complexity: int(math.Round(a["complexity"].Score)),
		Risk:       clamp(int(math.Round(a["risk"].Score+r.cfg.Decision.RiskOffset)), 0, 2),
		Private:    a["private_data"].Noul >= 0.5,
	}
}

func (r *Router) Route(ctx context.Context, req Request, id string) *Decision {
	needs := Needs{InputTokens: req.EstTokens(), Tools: req.Tools, Vision: req.Vision, AnthropicAPI: req.AnthropicAPI}
	env := Env{InFlight: r.Tracker.InFlight, SpentUSD: r.Tracker.Spent}
	key := req.StickyKey()

	privateHint := req.LooksPrivate()
	if req.UserTurns > 1 {
		// A secret that shows up mid-conversation (e.g. in a tool result) must not follow the sticky
		// model off the machine.
		if m, ok := r.Tracker.Sticky(key); ok && r.serves(m, needs) && !(privateHint && r.cfg.Decision.Private == "local_only" && !r.isLocal(m)) {
			return &Decision{ID: id, Model: m, Reason: "sticky: continuing conversation", Sticky: true, Needs: needs}
		}
	}

	p, err := r.sel.Pick(privateHint)
	if err != nil {
		if privateHint && r.cfg.Decision.Private == "local_only" {
			return r.localOnly(id, needs, err)
		}
		return r.fallback(id, needs, err)
	}
	st := req.State(p.MaxStateChars())
	res, err := p.Decide(ctx, st, r.questions())
	if err != nil {
		return r.fallback(id, needs, err)
	}
	sig := r.signals(res)
	sig.Private = sig.Private || privateHint
	needs.LocalOnly = sig.Private && r.cfg.Decision.Private == "local_only"

	best, required, all := Score(r.cfg, *sig, needs, env)
	d := &Decision{ID: id, Provider: p.Name(), DecisionCostUSD: res.CostUSD, DecisionMs: res.Latency.Milliseconds(),
		Signals: sig, Answers: res.Answers, Needs: needs, Required: required, Candidates: all}
	if !sig.Private {
		d.State = st
	}
	if best == nil {
		if needs.LocalOnly {
			d.Reason, d.Refused = "private request and no capable local model", true
			return d
		}
		d2 := r.fallback(id, needs, nil)
		d.Model, d.Reason = d2.Model, "no capable model; "+d2.Reason
		return d
	}
	d.Model = best.ID
	d.Reason = best.Why
	if d.Reason == "" {
		d.Reason = "cheapest model above quality floor"
	}
	r.Tracker.SetSticky(key, d.Model)

	if s := r.sel.Shadow; s != nil && r.OnShadow != nil && s.Name() != p.Name() {
		go func() {
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			sres, err := s.Decide(sctx, req.State(s.MaxStateChars()), r.questions())
			if err != nil {
				r.OnShadow(d, nil, s.Name(), err)
				return
			}
			r.OnShadow(d, r.signals(sres), s.Name(), nil)
		}()
	}
	return d
}

func (r *Router) fallback(id string, needs Needs, err error) *Decision {
	d := &Decision{ID: id, Model: r.cfg.Routing.FallbackModel, Reason: "fallback", Needs: needs}
	if err != nil {
		d.Error = err.Error()
		d.Reason = "fallback: decision failed"
	}
	if d.Model == "" || !r.serves(d.Model, needs) {
		d.Model = r.cfg.Models[0].ID
		for _, m := range r.cfg.Models {
			if r.serves(m.ID, needs) {
				d.Model = m.ID
				break
			}
		}
	}
	return d
}

func (r *Router) isLocal(model string) bool {
	m := r.cfg.Model(model)
	return m != nil && m.Local
}

// serves reports whether model can take a request with these needs' API format (models not in the
// config are remote, so they can).
func (r *Router) serves(model string, needs Needs) bool {
	m := r.cfg.Model(model)
	return m == nil || !needs.AnthropicAPI || m.ServesAnthropic()
}

// Alternatives returns the next models to try after d.Model fails upstream: other eligible candidates
// in rank order, then the fallback model.
func (r *Router) Alternatives(d *Decision) []string {
	var out []string
	seen := map[string]bool{d.Model: true}
	for _, c := range d.Candidates {
		if c.Eligible && !seen[c.ID] {
			out, seen[c.ID] = append(out, c.ID), true
		}
	}
	if fb := r.cfg.Routing.FallbackModel; fb != "" && !seen[fb] && !d.Needs.LocalOnly && r.serves(fb, d.Needs) {
		out = append(out, fb)
	}
	return out
}

// localOnly handles private requests that must stay on this machine when no local decision model is available.
func (r *Router) localOnly(id string, needs Needs, err error) *Decision {
	needs.LocalOnly = true
	for _, m := range r.cfg.Models {
		if m.Local && r.serves(m.ID, needs) {
			return &Decision{ID: id, Model: m.ID, Reason: "private request: first local model (no local decision provider)", Needs: needs, Error: err.Error()}
		}
	}
	return &Decision{ID: id, Reason: "private request and no local model configured", Needs: needs, Error: err.Error(), Refused: true}
}
