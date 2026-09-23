package router

import (
	"math"
	"sort"

	"github.com/mmornati/system-one-router/internal/config"
)

// Signals is what the decision model told us about a request.
type Signals struct {
	Topics     map[string]float64 `json:"topics"` // probability per topic (sums to ~1)
	Primary    string             `json:"primary"`
	Confidence float64            `json:"confidence"`
	Complexity int                `json:"complexity"` // 0..3
	Risk       int                `json:"risk"`       // 0..2
	Private    bool               `json:"private"`
}

// Needs are hard requirements derived from the raw request, not from the decision model.
type Needs struct {
	InputTokens int  `json:"input_tokens"`
	Tools       bool `json:"tools"`
	Vision      bool `json:"vision"`
	LocalOnly   bool `json:"local_only"`
	// AnthropicAPI: the request is in Anthropic Messages format and is forwarded as-is to /messages.
	AnthropicAPI bool `json:"anthropic_api,omitempty"`
}

type Candidate struct {
	ID       string  `json:"id"`
	Skill    float64 `json:"skill"`
	EstCost  float64 `json:"est_cost_usd"`
	EffCost  float64 `json:"eff_cost"` // cost after load penalty, used for ranking
	Eligible bool    `json:"eligible"`
	Why      string  `json:"why,omitempty"`
}

// Env carries live state the scorer needs: in-flight requests and spend per model.
type Env struct {
	InFlight func(model string) int
	SpentUSD func(model string) float64
}

// Score ranks models: among those that clear the quality floor and hard constraints,
// the cheapest (after load penalty) wins; ties go to the higher skill.
// If none clears the floor, the most skilled capable model wins (escalation).
func Score(cfg *config.Config, sig Signals, needs Needs, env Env) (best *Candidate, required float64, all []Candidate) {
	r := cfg.Routing
	cx := clamp(sig.Complexity, 0, len(r.MinSkill)-1)
	if sig.Confidence < cfg.Decision.ConfidenceThreshold && cx < len(r.MinSkill)-1 {
		cx++ // unsure about the request: be conservative
	}
	required = r.MinSkill[cx] + r.RiskBonus[clamp(sig.Risk, 0, len(r.RiskBonus)-1)]
	estOut := r.EstOutputTokens[clamp(sig.Complexity, 0, len(r.EstOutputTokens)-1)]

	var capable []Candidate
	for _, m := range cfg.Models {
		c := Candidate{ID: m.ID}
		for t, p := range sig.Topics {
			c.Skill += p * m.Skill(t)
		}
		if len(sig.Topics) == 0 {
			c.Skill = m.DefaultSkill
		}
		c.EstCost = (float64(needs.InputTokens)*m.Price.In + float64(estOut)*m.OutputMultiplier*m.Price.Out) / 1e6
		load := 0
		if env.InFlight != nil {
			load = env.InFlight(m.ID)
		}
		c.EffCost = c.EstCost * (1 + r.LoadPenalty*float64(load))

		switch {
		case m.Context > 0 && needs.InputTokens+estOut > m.Context:
			c.Why = "context too small"
		case needs.Tools && !m.Tools:
			c.Why = "no tool calling"
		case needs.Vision && !m.Vision:
			c.Why = "no vision"
		case needs.AnthropicAPI && !m.ServesAnthropic():
			c.Why = "no Anthropic API"
		case needs.LocalOnly && !m.Local:
			c.Why = "private request, model not local"
		case m.DailyBudgetUSD > 0 && env.SpentUSD != nil && env.SpentUSD(m.ID) >= m.DailyBudgetUSD:
			c.Why = "daily budget exhausted"
		}
		if c.Why == "" {
			capable = append(capable, c)
			if c.Skill >= required {
				c.Eligible = true
			} else {
				c.Why = "below quality floor"
			}
		}
		all = append(all, c)
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Eligible != all[j].Eligible {
			return all[i].Eligible
		}
		if all[i].Eligible {
			if math.Abs(all[i].EffCost-all[j].EffCost) > 1e-9 {
				return all[i].EffCost < all[j].EffCost
			}
			return all[i].Skill > all[j].Skill
		}
		return all[i].Skill > all[j].Skill
	})
	if len(all) > 0 && all[0].Eligible {
		b := all[0]
		return &b, required, all
	}
	if len(capable) > 0 {
		sort.SliceStable(capable, func(i, j int) bool { return capable[i].Skill > capable[j].Skill })
		b := capable[0]
		b.Why = "escalated: no model clears the floor"
		return &b, required, all
	}
	return nil, required, all
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
