// Command refit re-fits the per-topic model skills in config.yaml from the outcomes logged in
// data/decisions.jsonl (answer checks and user feedback), and reports upstream reliability separately.
// It only reads the log and the config; with -write it writes an updated copy of the config.
//
//	go run ./cmd/refit                       # report only
//	go run ./cmd/refit -write config.new.yaml
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/mmornati/system-one-router/internal/config"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "config file")
	logPath := flag.String("log", "", "event log (default: log_path from the config)")
	days := flag.Int("days", 0, "only use events from the last N days (0 = all)")
	p := defaultParams
	flag.Float64Var(&p.MinN, "min-n", p.MinN, "minimum effective sample size (weighted outcomes) before a skill is changed")
	flag.Float64Var(&p.MinDelta, "min-delta", p.MinDelta, "minimum |fitted - seed| before a skill is changed")
	flag.Float64Var(&p.Prior, "prior", p.Prior, "weight of the seed skill, in observations")
	flag.Float64Var(&p.Scale, "scale", p.Scale, "logistic scale: skill difference that multiplies the odds of a good label by e (0.1: floors are ~0.15 apart)")
	flag.Float64Var(&p.Target, "target", p.Target, "success rate at skill == difficulty; prior curvature, and the absolute level with -calibrate=false")
	flag.BoolVar(&p.Calibrate, "calibrate", p.Calibrate, "compare labels with other models' at the same source and difficulty (false: read them as absolute success rates, which biases skills down)")
	flag.Float64Var(&p.WCheck, "w-check", p.WCheck, "weight of an answer check (soft label p_ok)")
	flag.Float64Var(&p.WFeedback, "w-feedback", p.WFeedback, "weight of a user feedback rating")
	out := flag.String("write", "", "write a copy of the config with fitted skills to this path (never the config unless named)")
	flag.Parse()
	if err := run(os.Stdout, *cfgPath, *logPath, *days, p, *out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(w io.Writer, cfgPath, logPath string, days int, p Params, out string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if logPath == "" {
		logPath = cfg.LogPath
	}
	f, err := os.Open(logPath)
	if err != nil {
		return err
	}
	defer f.Close()
	var since time.Time
	if days > 0 {
		since = time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	}
	lg, err := ReadLog(f, since)
	if err != nil {
		return err
	}
	res := Fit(cfg, lg, p)
	report(w, res, p)

	changes := res.Changes()
	if out == "" || len(changes) == 0 {
		return nil
	}
	src, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	b, err := WriteSkills(src, changes)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nwrote %d change(s) to %s\n", len(changes), out)
	return nil
}

func report(w io.Writer, r *Result, p Params) {
	fmt.Fprintf(w, "%d chat events: %d routed with signals, %d with an outcome (%d checks, %d feedback)\n\n",
		r.Chats, r.Routed, r.Outcomes, r.Checks, r.Feedback)
	for _, src := range srcNames {
		if c, ok := r.Calibration[src]; ok {
			fmt.Fprintf(w, "%-8s labels: n=%.0f, mean %.2f vs %.2f predicted by the seeds; difficulty levels: %d, %.0f%% of labels with no peer model\n",
				src, c.N, c.MeanLabel, c.MeanSeed, c.Buckets, 100*c.Solo)
		}
	}
	if len(r.Calibration) > 0 {
		fmt.Fprintln(w)
	}

	if len(r.Fits) == 0 {
		fmt.Fprintln(w, "no outcomes to fit: enable answer checks (check-and-escalate) and/or rate answers with POST /feedback")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "MODEL\tTOPIC\tN_EFF\tSUCCESS\tSEED\tFITTED\tDELTA\t")
		for _, f := range r.Fits {
			mark := ""
			if f.Change {
				mark = "*"
			}
			fmt.Fprintf(tw, "%s\t%s\t%.1f\t%.0f%%\t%.2f\t%.2f\t%+.2f\t%s\n", f.Model, topicLabel(f.Topic), f.N, 100*f.Success, f.Seed, f.Fitted, f.Fitted-f.Seed, mark)
		}
		tw.Flush()
	}
	if len(r.Changes()) == 0 {
		fmt.Fprintf(w, "\nnot enough data: no (model, topic) pair has n_eff >= %g and |delta| >= %g; skills unchanged\n", p.MinN, p.MinDelta)
	} else {
		fmt.Fprintf(w, "\n* = proposed change (n_eff >= %g, |delta| >= %g); use -write <path> to apply\n", p.MinN, p.MinDelta)
	}

	if len(r.Reliability) == 0 {
		return
	}
	fmt.Fprintln(w, "\nUpstream reliability (429/5xx: availability, not counted against skill):")
	ids := make([]string, 0, len(r.Reliability))
	for id := range r.Reliability {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tATTEMPTS\tFAILED\tFAIL_RATE")
	for _, id := range ids {
		rel := r.Reliability[id]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.1f%%\n", id, rel.Attempts, rel.Failed, 100*float64(rel.Failed)/float64(rel.Attempts))
	}
	tw.Flush()
}

func topicLabel(t string) string {
	if t == "" {
		return "(default)"
	}
	return t
}
