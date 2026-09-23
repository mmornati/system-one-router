package router

import (
	"context"
	"errors"

	"github.com/mmornati/system-one-router/internal/decision"
)

// escalationMargin is how much more skilled a model must be to be worth re-sending a failed answer to.
const escalationMargin = 0.05

// ErrCheckSkipped means the answer cannot be checked without breaking the privacy policy.
var ErrCheckSkipped = errors.New("check skipped: private request and no local decision provider")

var checkQuestion = decision.Noul("Does the answer fully and correctly address the request?",
	"The answer is complete and correct, does what was asked, and follows every instruction and constraint in the request.",
	"The answer is wrong, incomplete, evasive or truncated, refuses without reason, or ignores instructions or constraints in the request.")

// Check asks the decision model whether answer fully addresses req, returning P(yes). d must be the
// routing decision for req; it decides whether the request is private.
func (r *Router) Check(ctx context.Context, req Request, answer string, d *Decision) (pOK, cost float64, ms int64, provider string, err error) {
	private := req.LooksPrivate() || (d.Signals != nil && d.Signals.Private)
	p, err := r.sel.Pick(private)
	if err != nil {
		if private && r.cfg.Decision.Private == "local_only" {
			return 0, 0, 0, "", ErrCheckSkipped
		}
		return 0, 0, 0, "", err
	}
	res, err := p.Decide(ctx, r.checkState(req, answer, p.MaxStateChars()), map[string]decision.Question{"answer_ok": checkQuestion})
	if err != nil {
		return 0, 0, 0, p.Name(), err
	}
	return res.Answers["answer_ok"].Noul, res.CostUSD, res.Latency.Milliseconds(), p.Name(), nil
}

// checkState fits the answer, the request and (if room is left) the system prompt into budget chars.
// The answer gets up to max_answer_chars, but always leaves the request at least a third of the budget.
func (r *Router) checkState(req Request, answer string, budget int) map[string]string {
	limit := min(r.cfg.Routing.Check.MaxAnswerChars, budget-min(len(req.LastUser), budget/3))
	ans := trimMiddle(answer, limit)
	rest := budget - min(len(ans), limit)
	share := rest
	if req.System != "" {
		share = rest * 3 / 4
	}
	st := map[string]string{"answer": ans, "request": trimMiddle(req.LastUser, share)}
	if rest -= len(st["request"]); req.System != "" && rest >= 100 {
		st["system_prompt"] = trimMiddle(req.System, rest)
	}
	return st
}

// EscalationTarget picks the model to re-send a request to when current's answer failed the check:
// the cheapest capable candidate clearly more skilled than current, else the most skilled one if it
// beats current at all. Models below the quality floor are fine; hard constraints (context, tools,
// vision, privacy, budget) are not. Returns "" when there is nothing stronger to try.
func (r *Router) EscalationTarget(d *Decision, current string, exclude []string) string {
	skip := map[string]bool{current: true}
	for _, m := range exclude {
		skip[m] = true
	}
	cur, found := 0.0, false
	for _, c := range d.Candidates {
		if c.ID == current {
			cur, found = c.Skill, true
		}
	}
	if !found {
		return ""
	}
	var cheap, top *Candidate
	for i := range d.Candidates {
		c := &d.Candidates[i]
		if skip[c.ID] || !(c.Eligible || c.Why == "below quality floor") || c.Skill <= cur {
			continue
		}
		if m := r.cfg.Model(c.ID); m == nil || (d.Needs.LocalOnly && !m.Local) ||
			(m.DailyBudgetUSD > 0 && r.Tracker.Spent(c.ID) >= m.DailyBudgetUSD) {
			continue
		}
		if c.Skill > cur+escalationMargin && (cheap == nil || c.EstCost < cheap.EstCost || (c.EstCost == cheap.EstCost && c.Skill > cheap.Skill)) {
			cheap = c
		}
		if top == nil || c.Skill > top.Skill {
			top = c
		}
	}
	if cheap != nil {
		return cheap.ID
	}
	if top != nil {
		return top.ID
	}
	return ""
}
