// Package stats aggregates the JSONL event log (internal/store) into dashboard-ready numbers:
// cost and latency per model, routing reasons, topic/complexity/risk mix, shadow agreement,
// check escalations, feedback, and savings vs always using the priciest configured model.
package stats

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mmornati/system-one-router/internal/config"
	"github.com/mmornati/system-one-router/internal/router"
)

type Stats struct {
	GeneratedAt     time.Time              `json:"generated_at"`
	Since           *time.Time             `json:"since,omitempty"`
	Totals          Totals                 `json:"totals"`
	Models          map[string]ModelStats  `json:"models"`
	Daily           []DailyEntry           `json:"daily"`
	Reasons         map[string]int         `json:"reasons"`
	Topics          map[string]int         `json:"topics"`
	Complexity      [4]int                 `json:"complexity"`
	Risk            [3]int                 `json:"risk"`
	DecisionLatency map[string]Percentiles `json:"decision_latency"` // by decision provider
	Retries         Retries                `json:"retries"`
	Shadow          map[string]ShadowStats `json:"shadow"` // by shadow provider
	Checks          Checks                 `json:"checks"`
	Feedback        Feedback               `json:"feedback"`
	Savings         Savings                `json:"savings"`
}

type Totals struct {
	Requests        int     `json:"requests"` // chat events
	Routed          int     `json:"routed"`
	Passthrough     int     `json:"passthrough"`
	Refused         int     `json:"refused"`
	Errors          int     `json:"errors"` // status >= 400
	DryRoutes       int     `json:"dry_routes"`
	TotalCostUSD    float64 `json:"total_cost_usd"`
	DecisionCostUSD float64 `json:"decision_cost_usd"`
}

type ModelStats struct {
	Requests         int     `json:"requests"`
	CostUSD          float64 `json:"cost_usd"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	P50LatencyMs     int64   `json:"p50_latency_ms"`
	P95LatencyMs     int64   `json:"p95_latency_ms"`
	Errors           int     `json:"errors"`
}

type DailyEntry struct {
	Date   string              `json:"date"` // UTC yyyy-mm-dd
	Models map[string]DayModel `json:"models"`
}

type DayModel struct {
	Requests int     `json:"requests"`
	CostUSD  float64 `json:"cost_usd"`
}

type Percentiles struct {
	P50Ms int64 `json:"p50_ms"`
	P95Ms int64 `json:"p95_ms"`
}

type Retries struct {
	Requests int            `json:"requests"` // non-empty failed[]
	PerModel map[string]int `json:"per_model"`
}

type ShadowStats struct {
	Count              int     `json:"count"`
	Errors             int     `json:"errors"`
	TopicAgreePct      float64 `json:"topic_agree_pct"`
	ComplexityAgreePct float64 `json:"complexity_agree_pct"`
}

type Checks struct {
	Count           int            `json:"count"`
	Passed          int            `json:"passed"`
	PassRatePct     float64        `json:"pass_rate_pct"`
	Escalations     int            `json:"escalations"`
	EscalationPairs map[string]int `json:"escalation_pairs"` // "from -> to": count
	CostUSD         float64        `json:"cost_usd"`
}

type Feedback struct {
	Good     int                      `json:"good"`
	Bad      int                      `json:"bad"`
	PerModel map[string]FeedbackCount `json:"per_model"`
}

type FeedbackCount struct {
	Good int `json:"good"`
	Bad  int `json:"bad"`
}

type Savings struct {
	ActualCostUSD         float64 `json:"actual_cost_usd"`
	CounterfactualCostUSD float64 `json:"counterfactual_cost_usd"`
	SavingsUSD            float64 `json:"savings_usd"`
	SavingsPct            float64 `json:"savings_pct"`
	PriciestModel         string  `json:"priciest_model"`
}

type event struct {
	TS   string          `json:"ts"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

type chatData struct {
	ID               string           `json:"id"`
	Decision         *router.Decision `json:"decision"`
	Model            string           `json:"model"`
	Failed           []string         `json:"failed"`
	Status           int              `json:"status"`
	CostUSD          float64          `json:"cost_usd"`
	PromptTokens     int64            `json:"prompt_tokens"`
	CompletionTokens int64            `json:"completion_tokens"`
	LatencyMs        int64            `json:"latency_ms"`
}

type shadowData struct {
	ID              string `json:"id"`
	Provider        string `json:"provider"`
	AgreeTopic      bool   `json:"agree_topic"`
	AgreeComplexity bool   `json:"agree_complexity"`
	Error           string `json:"error"`
}

type feedbackData struct {
	ID      string `json:"id"`
	Rating  string `json:"rating"`
	Comment string `json:"comment"`
}

type checkData struct {
	ID           string  `json:"id"`
	Model        string  `json:"model"`
	POK          float64 `json:"p_ok"`
	Passed       bool    `json:"passed"`
	EscalatedTo  string  `json:"escalated_to"`
	CheckCostUSD float64 `json:"check_cost_usd"`
	CheckMs      int64   `json:"check_ms"`
}

// modelAgg accumulates per-model numbers before percentiles are derived.
type modelAgg struct {
	requests, errors               int
	costUSD                        float64
	promptTokens, completionTokens int64
	latencies                      []int64
}

// Compute streams the JSONL event log and aggregates it into Stats. Malformed or overlong lines
// are skipped rather than failing the whole pass. since filters out events older than it; the
// zero value keeps everything.
func Compute(r io.Reader, cfg *config.Config, since time.Time) (*Stats, error) {
	br := bufio.NewReaderSize(r, 64<<10)

	st := &Stats{
		Models:          map[string]ModelStats{},
		Reasons:         map[string]int{},
		Topics:          map[string]int{},
		DecisionLatency: map[string]Percentiles{},
		Retries:         Retries{PerModel: map[string]int{}},
		Shadow:          map[string]ShadowStats{},
		Checks:          Checks{EscalationPairs: map[string]int{}},
		Feedback:        Feedback{PerModel: map[string]FeedbackCount{}},
	}

	models := map[string]*modelAgg{}
	daily := map[string]map[string]*DayModel{}
	decisionLatencies := map[string][]int64{}
	chatModelByID := map[string]string{}
	shadowAgg := map[string]*struct{ count, errs, topicAgree, cxAgree int }{}

	var priciest *config.Model
	for i := range cfg.Models {
		if priciest == nil || cfg.Models[i].Price.Out > priciest.Price.Out {
			priciest = &cfg.Models[i]
		}
	}
	if priciest != nil {
		st.Savings.PriciestModel = priciest.ID
	}

	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			processLine(line, since, st, models, daily, decisionLatencies, chatModelByID, shadowAgg, priciest)
		}
		if err != nil {
			break
		}
	}

	finalize(st, models, daily, decisionLatencies, shadowAgg)
	st.GeneratedAt = time.Now().UTC()
	if !since.IsZero() {
		s := since
		st.Since = &s
	}
	return st, nil
}

func processLine(line []byte, since time.Time, st *Stats, models map[string]*modelAgg,
	daily map[string]map[string]*DayModel, decisionLatencies map[string][]int64,
	chatModelByID map[string]string, shadowAgg map[string]*struct{ count, errs, topicAgree, cxAgree int },
	priciest *config.Model,
) {
	var ev event
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	ts, tsErr := time.Parse(time.RFC3339Nano, ev.TS)
	if tsErr == nil && !since.IsZero() && ts.Before(since) {
		return
	}

	switch ev.Kind {
	case "chat":
		var d chatData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return
		}
		st.Totals.Requests++
		st.Totals.TotalCostUSD += d.CostUSD
		if d.Status >= 400 {
			st.Totals.Errors++
		}
		if d.Decision != nil {
			st.Totals.Routed++
			recordDecision(d.Decision, st, decisionLatencies)
		} else {
			st.Totals.Passthrough++
		}
		if len(d.Failed) > 0 {
			st.Retries.Requests++
			for _, m := range d.Failed {
				st.Retries.PerModel[m]++
			}
		}
		if d.Model != "" {
			chatModelByID[d.ID] = d.Model
			ma := models[d.Model]
			if ma == nil {
				ma = &modelAgg{}
				models[d.Model] = ma
			}
			ma.requests++
			ma.costUSD += d.CostUSD
			ma.promptTokens += d.PromptTokens
			ma.completionTokens += d.CompletionTokens
			ma.latencies = append(ma.latencies, d.LatencyMs)
			if d.Status >= 400 {
				ma.errors++
			}

			if tsErr == nil {
				date := ts.UTC().Format("2006-01-02")
				dm := daily[date]
				if dm == nil {
					dm = map[string]*DayModel{}
					daily[date] = dm
				}
				e := dm[d.Model]
				if e == nil {
					e = &DayModel{}
					dm[d.Model] = e
				}
				e.Requests++
				e.CostUSD += d.CostUSD
			}

			if priciest != nil && (d.PromptTokens > 0 || d.CompletionTokens > 0) {
				st.Savings.ActualCostUSD += d.CostUSD
				st.Savings.CounterfactualCostUSD += (float64(d.PromptTokens)*priciest.Price.In + float64(d.CompletionTokens)*priciest.Price.Out) / 1e6
			}
		}

	case "route_dry":
		st.Totals.DryRoutes++
		var d router.Decision
		if err := json.Unmarshal(ev.Data, &d); err == nil {
			recordDecision(&d, st, decisionLatencies)
		}

	case "refused":
		st.Totals.Refused++
		var d router.Decision
		if err := json.Unmarshal(ev.Data, &d); err == nil {
			recordDecision(&d, st, decisionLatencies)
		}

	case "shadow":
		var d shadowData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return
		}
		a := shadowAgg[d.Provider]
		if a == nil {
			a = &struct{ count, errs, topicAgree, cxAgree int }{}
			shadowAgg[d.Provider] = a
		}
		a.count++
		if d.Error != "" {
			a.errs++
			return
		}
		if d.AgreeTopic {
			a.topicAgree++
		}
		if d.AgreeComplexity {
			a.cxAgree++
		}

	case "feedback":
		var d feedbackData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return
		}
		fc := st.Feedback.PerModel[chatModelByID[d.ID]]
		switch d.Rating {
		case "good":
			st.Feedback.Good++
			fc.Good++
		case "bad":
			st.Feedback.Bad++
			fc.Bad++
		default:
			return
		}
		st.Feedback.PerModel[chatModelByID[d.ID]] = fc

	case "check":
		var d checkData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return
		}
		st.Checks.Count++
		st.Checks.CostUSD += d.CheckCostUSD
		if d.Passed {
			st.Checks.Passed++
		}
		if d.EscalatedTo != "" {
			st.Checks.Escalations++
			st.Checks.EscalationPairs[d.Model+" -> "+d.EscalatedTo]++
		}
	}
}

// recordDecision folds a routing Decision (from chat, route_dry, or refused) into the reason,
// topic, complexity, risk and decision-latency/cost aggregates.
func recordDecision(d *router.Decision, st *Stats, decisionLatencies map[string][]int64) {
	st.Totals.DecisionCostUSD += d.DecisionCostUSD
	st.Reasons[reasonCategory(d.Reason)]++
	if d.Provider != "" {
		decisionLatencies[d.Provider] = append(decisionLatencies[d.Provider], d.DecisionMs)
	}
	if d.Signals != nil {
		if d.Signals.Primary != "" {
			st.Topics[d.Signals.Primary]++
		}
		if cx := d.Signals.Complexity; cx >= 0 && cx < len(st.Complexity) {
			st.Complexity[cx]++
		}
		if rk := d.Signals.Risk; rk >= 0 && rk < len(st.Risk) {
			st.Risk[rk]++
		}
	}
}

func reasonCategory(reason string) string {
	switch {
	case reason == "":
		return "other"
	case strings.HasPrefix(reason, "sticky"):
		return "sticky"
	case strings.HasPrefix(reason, "escalated"):
		return "escalated"
	case strings.HasPrefix(reason, "fallback"):
		return "fallback"
	case strings.Contains(reason, "no capable model") || strings.Contains(reason, "no local model"):
		return "no capable model"
	case reason == "cheapest model above quality floor":
		return "cheapest"
	default:
		return "other"
	}
}

func finalize(st *Stats, models map[string]*modelAgg, daily map[string]map[string]*DayModel,
	decisionLatencies map[string][]int64, shadowAgg map[string]*struct{ count, errs, topicAgree, cxAgree int },
) {
	for id, ma := range models {
		p50, p95 := percentiles(ma.latencies)
		avg := 0.0
		if ma.requests > 0 {
			sum := int64(0)
			for _, l := range ma.latencies {
				sum += l
			}
			avg = float64(sum) / float64(ma.requests)
		}
		st.Models[id] = ModelStats{
			Requests: ma.requests, CostUSD: ma.costUSD, PromptTokens: ma.promptTokens,
			CompletionTokens: ma.completionTokens, AvgLatencyMs: avg,
			P50LatencyMs: p50, P95LatencyMs: p95, Errors: ma.errors,
		}
	}

	var dates []string
	for date := range daily {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	for _, date := range dates {
		e := DailyEntry{Date: date, Models: map[string]DayModel{}}
		for m, dm := range daily[date] {
			e.Models[m] = *dm
		}
		st.Daily = append(st.Daily, e)
	}

	for provider, latencies := range decisionLatencies {
		p50, p95 := percentiles(latencies)
		st.DecisionLatency[provider] = Percentiles{P50Ms: p50, P95Ms: p95}
	}

	for provider, a := range shadowAgg {
		compared := a.count - a.errs
		topicPct, cxPct := 0.0, 0.0
		if compared > 0 {
			topicPct = 100 * float64(a.topicAgree) / float64(compared)
			cxPct = 100 * float64(a.cxAgree) / float64(compared)
		}
		st.Shadow[provider] = ShadowStats{
			Count: a.count, Errors: a.errs, TopicAgreePct: topicPct, ComplexityAgreePct: cxPct,
		}
	}

	if st.Checks.Count > 0 {
		st.Checks.PassRatePct = 100 * float64(st.Checks.Passed) / float64(st.Checks.Count)
	}

	if st.Savings.CounterfactualCostUSD > 0 {
		st.Savings.SavingsUSD = st.Savings.CounterfactualCostUSD - st.Savings.ActualCostUSD
		st.Savings.SavingsPct = 100 * st.Savings.SavingsUSD / st.Savings.CounterfactualCostUSD
	}
}

// percentiles returns p50 and p95 of a (not necessarily sorted) latency slice, in ms.
func percentiles(v []int64) (p50, p95 int64) {
	if len(v) == 0 {
		return 0, 0
	}
	sorted := append([]int64(nil), v...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return pct(sorted, 0.5), pct(sorted, 0.95)
}

func pct(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
