// Command bench runs labelled prompts through one or more decision providers (Jev, local Laya
// checkpoints) and reports the decision each one takes and the model the router would pick.
// No prompt is ever executed on a chat model.
//
//	go run ./cmd/bench -providers jev,laya,laya-multilingual,laya-auto
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mmornati/system-one-router/internal/config"
	"github.com/mmornati/system-one-router/internal/decision"
	"github.com/mmornati/system-one-router/internal/router"
	"github.com/mmornati/system-one-router/internal/upstream"
)

type Case struct {
	ID         string   `json:"id"`
	Group      string   `json:"group"`
	Lang       string   `json:"lang"`
	Prompt     string   `json:"prompt"`
	Primary    string   `json:"primary"`
	Topics     []string `json:"topics"`
	Complexity int      `json:"complexity"`
	Risk       int      `json:"risk"`
	Private    bool     `json:"private"`
}

// Row is one provider's decision on one case.
type Row struct {
	Case       string             `json:"case"`
	Topic      string             `json:"topic"`
	TopicConf  float64            `json:"topic_conf"`
	TopicProbs map[string]float64 `json:"topic_probs,omitempty"`
	CxScore    float64            `json:"complexity_score"`
	CxConf     float64            `json:"complexity_conf"`
	Complexity int                `json:"complexity"`
	RiskScore  float64            `json:"risk_score"`
	Risk       int                `json:"risk"`
	PrivateP   float64            `json:"private_p"`
	Model      string             `json:"model"`
	GoldModel  string             `json:"gold_model"`
	EstCost    float64            `json:"est_cost_usd"`
	GoldCost   float64            `json:"gold_cost_usd"`
	TopCost    float64            `json:"top_cost_usd"`
	Required   float64            `json:"required_skill"`
	Reason     string             `json:"reason"`
	Ms         int64              `json:"ms"`
	DecCost    float64            `json:"decision_cost_usd"`
	Err        string             `json:"error,omitempty"`
}

type ProviderRun struct {
	Name    string  `json:"name"`
	Label   string  `json:"label"`
	Rows    []Row   `json:"rows"`
	Summary Summary `json:"summary"`
}

func main() {
	cfgPath := flag.String("config", "config.yaml", "config file")
	casesPath := flag.String("cases", "bench/cases.json", "labelled cases")
	providers := flag.String("providers", "jev,laya,laya-multilingual,laya-auto", "comma-separated providers")
	layaURL := flag.String("laya-url", "http://127.0.0.1:8788/decisions", "Laya sidecar endpoint")
	top := flag.String("top", "anthropic/claude-opus-5.5", "reference 'always use the best model' baseline")
	outDir := flag.String("out", "bench/results", "output directory")
	flag.Parse()
	if err := run(*cfgPath, *casesPath, *providers, *layaURL, *top, *outDir); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(cfgPath, casesPath, providerList, layaURL, top, outDir string) error {
	config.LoadDotEnv(".env")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if _, err := upstream.New(cfg.Upstream.BaseURL, "").RefreshPrices(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "price refresh failed, using config prices:", err)
	}
	cancel()

	var cases []Case
	b, err := os.ReadFile(casesPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		return err
	}

	var runs []*ProviderRun
	for _, name := range strings.Split(providerList, ",") {
		p, label, conc, err := buildProvider(cfg, strings.TrimSpace(name), layaURL)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "→ %s (%d cases)\n", label, len(cases))
		runs = append(runs, runProvider(cfg, p, label, conc, cases, top))
	}

	for _, r := range runs {
		r.Summary = summarize(cfg, cases, r.Rows)
	}
	printSummary(runs)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	stamp := time.Now().Format("20060102-150405")
	report := Report{Date: time.Now().Format(time.RFC1123), Cases: cases, Runs: runs, Config: cfgSummary(cfg), Top: top}
	jb, _ := json.MarshalIndent(report, "", " ")
	jsonPath := filepath.Join(outDir, "bench-"+stamp+".json")
	if err := os.WriteFile(jsonPath, jb, 0o644); err != nil {
		return err
	}
	htmlPath := filepath.Join(outDir, "bench-"+stamp+".html")
	if err := writeHTML(htmlPath, report); err != nil {
		return err
	}
	latest := filepath.Join(outDir, "latest.html")
	os.Remove(latest)
	os.Symlink(filepath.Base(htmlPath), latest)
	fmt.Printf("\nSaved %s\n      %s\n", jsonPath, htmlPath)
	return nil
}

func buildProvider(cfg *config.Config, name, layaURL string) (decision.Provider, string, int, error) {
	switch name {
	case "jev":
		pc, ok := cfg.Decision.Providers["jev"]
		if !ok {
			return nil, "", 0, fmt.Errorf("jev not configured")
		}
		key := os.Getenv(pc.APIKeyEnv)
		if key == "" {
			return nil, "", 0, fmt.Errorf("%s is empty", pc.APIKeyEnv)
		}
		return decision.NewHTTPProvider("jev", pc.URL, pc.Model, key, false, pc.MaxStateChars, 10*time.Second), "Jev 1.13 (OpenRouter)", 4, nil
	case "laya":
		return decision.NewHTTPProvider(name, layaURL, "laya", "", true, 1500, 30*time.Second), "Laya English (local, 512 tok)", 1, nil
	case "laya-multilingual":
		return decision.NewHTTPProvider(name, layaURL, "laya-multilingual", "", true, 3000, 30*time.Second), "Laya multilingual (local, 1024 tok)", 1, nil
	case "laya-auto":
		return decision.NewHTTPProvider(name, layaURL, "laya-auto", "", true, 1500, 30*time.Second), "Laya auto (language-routed)", 1, nil
	}
	return nil, "", 0, fmt.Errorf("unknown provider %q", name)
}

func runProvider(cfg *config.Config, p decision.Provider, label string, conc int, cases []Case, top string) *ProviderRun {
	sel := &decision.Selector{Mode: p.Name(), PrivatePolicy: "ignore", Providers: map[string]decision.Provider{p.Name(): p}}
	rt := router.New(cfg, sel)
	// warm-up (first call on MPS / cold connection is not representative)
	rt.Route(context.Background(), router.Request{FirstUser: "warm up", LastUser: "warm up", UserTurns: 1, Chars: 7}, "")

	rows := make([]Row, len(cases))
	var wg sync.WaitGroup
	sem := make(chan struct{}, conc)
	for i, c := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req := router.Request{FirstUser: c.Prompt, LastUser: c.Prompt, UserTurns: 1, Chars: len(c.Prompt)}
			d := rt.Route(context.Background(), req, "")
			rows[i] = toRow(cfg, c, d, top)
		}()
	}
	wg.Wait()
	return &ProviderRun{Name: p.Name(), Label: label, Rows: rows}
}

func toRow(cfg *config.Config, c Case, d *router.Decision, top string) Row {
	r := Row{Case: c.ID, Model: d.Model, Reason: d.Reason, Ms: d.DecisionMs, DecCost: d.DecisionCostUSD, Err: d.Error, Required: d.Required}
	if d.Signals != nil {
		r.Topic, r.TopicConf, r.Complexity, r.Risk = d.Signals.Primary, d.Signals.Confidence, d.Signals.Complexity, d.Signals.Risk
	}
	if a, ok := d.Answers["primary_topic"]; ok {
		r.TopicProbs = a.Probabilities
	}
	if a, ok := d.Answers["complexity"]; ok {
		r.CxScore, r.CxConf = a.Score, a.Confidence
	}
	r.RiskScore = d.Answers["risk"].Score
	r.PrivateP = d.Answers["private_data"].Noul
	for _, cand := range d.Candidates {
		if cand.ID == d.Model {
			r.EstCost = cand.EstCost
		}
		if cand.ID == top {
			r.TopCost = cand.EstCost
		}
	}
	gold := router.Signals{Topics: map[string]float64{c.Primary: 1}, Primary: c.Primary, Confidence: 1, Complexity: c.Complexity, Risk: c.Risk}
	if g, _, _ := router.Score(cfg, gold, d.Needs, router.Env{}); g != nil {
		r.GoldModel, r.GoldCost = g.ID, g.EstCost
	}
	return r
}

// ---------- metrics ----------

type Summary struct {
	N, Errors           int
	TopicAcc            float64
	TopicAccByGroup     map[string]float64
	TopicAcceptable     float64 // predicted topic is one of the gold topics
	CxExact, CxNear     float64
	RiskExact           float64
	PrivateAcc          float64
	HighConfShare       float64 // share of answers with topic confidence >= threshold
	HighConfAcc         float64
	LowConfAcc          float64
	ECE                 float64
	RouteMatch          float64
	UnderProvisioned    int // routed to a cheaper model than gold (quality risk)
	OverProvisioned     int // routed to a pricier model than gold (money wasted)
	RoutedCost          float64
	GoldCost            float64
	TopCost             float64
	P50, P90            int64
	DecisionCostPer1k   float64
	ModelUsage          map[string]int
	AgreeWithFirst      float64 // topic agreement with the first provider
	ModelAgreeWithFirst float64
}

func summarize(cfg *config.Config, cases []Case, rows []Row) Summary {
	s := Summary{N: len(rows), TopicAccByGroup: map[string]float64{}, ModelUsage: map[string]int{}}
	groupN, groupHit := map[string]int{}, map[string]int{}
	var hit, acceptable, cxE, cxN, rkE, priv, hi, hiHit, loHit, route int
	var lat []int64
	var decCost float64
	type cb struct {
		conf float64
		hit  bool
	}
	var calib []cb
	for i, r := range rows {
		c := cases[i]
		if r.Err != "" {
			s.Errors++
		}
		h := r.Topic == c.Primary
		hit += b2i(h)
		groupN[c.Group]++
		groupHit[c.Group] += b2i(h)
		acceptable += b2i(slices.Contains(c.Topics, r.Topic))
		cxE += b2i(r.Complexity == c.Complexity)
		cxN += b2i(abs(r.Complexity-c.Complexity) <= 1)
		rkE += b2i(r.Risk == c.Risk)
		priv += b2i((r.PrivateP >= 0.5) == c.Private)
		calib = append(calib, cb{r.TopicConf, h})
		if r.TopicConf >= cfg.Decision.ConfidenceThreshold {
			hi++
			hiHit += b2i(h)
		} else {
			loHit += b2i(h)
		}
		route += b2i(r.Model == r.GoldModel)
		switch {
		case r.Model != r.GoldModel && r.EstCost < r.GoldCost:
			s.UnderProvisioned++
		case r.Model != r.GoldModel && r.EstCost > r.GoldCost:
			s.OverProvisioned++
		}
		s.RoutedCost += r.EstCost
		s.GoldCost += r.GoldCost
		s.TopCost += r.TopCost
		s.ModelUsage[r.Model]++
		lat = append(lat, r.Ms)
		decCost += r.DecCost
	}
	n := float64(s.N)
	s.TopicAcc, s.TopicAcceptable = float64(hit)/n, float64(acceptable)/n
	s.CxExact, s.CxNear, s.RiskExact, s.PrivateAcc = float64(cxE)/n, float64(cxN)/n, float64(rkE)/n, float64(priv)/n
	s.RouteMatch = float64(route) / n
	s.HighConfShare = float64(hi) / n
	s.HighConfAcc = ratio(hiHit, hi)
	s.LowConfAcc = ratio(loHit, s.N-hi)
	for g, k := range groupN {
		s.TopicAccByGroup[g] = float64(groupHit[g]) / float64(k)
	}
	// expected calibration error, 10 equal-width bins
	bins := make([]struct {
		n, hit int
		conf   float64
	}, 10)
	for _, x := range calib {
		k := min(int(x.conf*10), 9)
		bins[k].n++
		bins[k].conf += x.conf
		bins[k].hit += b2i(x.hit)
	}
	for _, bn := range bins {
		if bn.n > 0 {
			s.ECE += float64(bn.n) / n * math.Abs(float64(bn.hit)/float64(bn.n)-bn.conf/float64(bn.n))
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	s.P50, s.P90 = lat[len(lat)/2], lat[len(lat)*9/10]
	s.DecisionCostPer1k = 1000 * decCost / n
	return s
}

func agreement(a, b []Row) (topic, model float64) {
	var t, m int
	for i := range a {
		t += b2i(a[i].Topic == b[i].Topic)
		m += b2i(a[i].Model == b[i].Model)
	}
	return float64(t) / float64(len(a)), float64(m) / float64(len(a))
}

func printSummary(runs []*ProviderRun) {
	for _, r := range runs[1:] {
		r.Summary.AgreeWithFirst, r.Summary.ModelAgreeWithFirst = agreement(runs[0].Rows, r.Rows)
	}
	pct := func(v float64) string {
		if v < 0 {
			return "  n/a"
		}
		return fmt.Sprintf("%5.1f%%", 100*v)
	}
	fmt.Printf("\n%-34s", "")
	for _, r := range runs {
		fmt.Printf(" %22.22s", r.Name)
	}
	line := func(label string, f func(Summary) string) {
		fmt.Printf("\n%-34s", label)
		for _, r := range runs {
			fmt.Printf(" %22s", f(r.Summary))
		}
	}
	line("errors", func(s Summary) string { return fmt.Sprint(s.Errors) })
	line("topic accuracy", func(s Summary) string { return pct(s.TopicAcc) })
	line("topic acceptable (any gold topic)", func(s Summary) string { return pct(s.TopicAcceptable) })
	for _, g := range []string{"core", "multilingual", "long", "tricky", "private"} {
		line("  topic acc · "+g, func(s Summary) string { return pct(s.TopicAccByGroup[g]) })
	}
	line("complexity exact / ±1", func(s Summary) string { return pct(s.CxExact) + " / " + pct(s.CxNear) })
	line("risk exact", func(s Summary) string { return pct(s.RiskExact) })
	line("private detected (model only)", func(s Summary) string { return pct(s.PrivateAcc) })
	line("confident (≥ threshold) share", func(s Summary) string { return pct(s.HighConfShare) })
	line("accuracy when confident / not", func(s Summary) string { return pct(s.HighConfAcc) + " / " + pct(s.LowConfAcc) })
	line("ECE (lower = better calibrated)", func(s Summary) string { return fmt.Sprintf("%.3f", s.ECE) })
	line("route = gold-label route", func(s Summary) string { return pct(s.RouteMatch) })
	line("under / over-provisioned", func(s Summary) string { return fmt.Sprintf("%d / %d", s.UnderProvisioned, s.OverProvisioned) })
	line("est. model cost routed / gold", func(s Summary) string { return fmt.Sprintf("$%.3f / $%.3f", s.RoutedCost, s.GoldCost) })
	line("decision latency p50 / p90", func(s Summary) string { return fmt.Sprintf("%dms / %dms", s.P50, s.P90) })
	line("decision cost per 1k", func(s Summary) string { return fmt.Sprintf("$%.4f", s.DecisionCostPer1k) })
	fmt.Printf("\n%-34s %22s", "topic / model agreement w/ "+runs[0].Name, "—")
	for _, r := range runs[1:] {
		fmt.Printf(" %22s", pct(r.Summary.AgreeWithFirst)+" / "+pct(r.Summary.ModelAgreeWithFirst))
	}
	fmt.Printf("\n\nalways %s: $%.3f for the same prompts\n", "top model", runs[0].Summary.TopCost)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func ratio(a, b int) float64 {
	if b == 0 {
		return -1 // no samples; JSON cannot encode NaN
	}
	return float64(a) / float64(b)
}
