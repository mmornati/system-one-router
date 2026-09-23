package main

import (
	"fmt"
	"html/template"
	"os"
	"sort"
	"strings"

	"github.com/mmornati/system-one-router/internal/config"
)

type Report struct {
	Date   string         `json:"date"`
	Top    string         `json:"top_model"`
	Config []ModelInfo    `json:"models"`
	Cases  []Case         `json:"cases"`
	Runs   []*ProviderRun `json:"runs"`
}

type ModelInfo struct {
	ID      string  `json:"id"`
	In, Out float64 // USD / M tokens
	Default float64
}

func cfgSummary(cfg *config.Config) []ModelInfo {
	var out []ModelInfo
	for _, m := range cfg.Models {
		out = append(out, ModelInfo{ID: m.ID, In: m.Price.In, Out: m.Price.Out, Default: m.DefaultSkill})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Out < out[j].Out })
	return out
}

func short(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}

func writeHTML(path string, r Report) error {
	tier := map[string]int{}
	for i, m := range r.Config {
		tier[m.ID] = i
	}
	funcs := template.FuncMap{
		"pct": func(v float64) string {
			if v < 0 {
				return "n/a"
			}
			return fmt.Sprintf("%.0f%%", 100*v)
		},
		"pct1":  func(v float64) string { return fmt.Sprintf("%.1f%%", 100*v) },
		"f2":    func(v float64) string { return fmt.Sprintf("%.2f", v) },
		"f3":    func(v float64) string { return fmt.Sprintf("%.3f", v) },
		"usd":   func(v float64) string { return fmt.Sprintf("$%.3f", v) },
		"usd4":  func(v float64) string { return fmt.Sprintf("$%.4f", v) },
		"short": short,
		"tier":  func(m string) int { return tier[m] },
		"row":   func(run *ProviderRun, i int) Row { return run.Rows[i] },
		"trunc": func(s string, n int) string {
			s = strings.Join(strings.Fields(s), " ")
			if len([]rune(s)) <= n {
				return s
			}
			return string([]rune(s)[:n]) + "…"
		},
		"confClass": func(c float64) string {
			switch {
			case c >= 0.8:
				return "hi"
			case c >= 0.5:
				return "mid"
			}
			return "lo"
		},
		"disagree": func(runs []*ProviderRun, i int) bool {
			for _, run := range runs {
				if run.Rows[i].Model != run.Rows[i].GoldModel || run.Rows[i].Model != runs[0].Rows[i].Model {
					return true
				}
			}
			return false
		},
		"usage": func(s Summary) []struct {
			Model string
			N     int
			Pct   float64
		} {
			var out []struct {
				Model string
				N     int
				Pct   float64
			}
			for _, m := range r.Config {
				if n := s.ModelUsage[m.ID]; n > 0 {
					out = append(out, struct {
						Model string
						N     int
						Pct   float64
					}{m.ID, n, float64(n) / float64(s.N)})
				}
			}
			return out
		},
		"groups": func() []string { return []string{"core", "multilingual", "long", "tricky", "private"} },
		"gacc":   func(s Summary, g string) float64 { return s.TopicAccByGroup[g] },
		"savings": func(s Summary) string {
			if s.TopCost == 0 {
				return "n/a"
			}
			return fmt.Sprintf("%.0f%%", 100*(1-s.RoutedCost/s.TopCost))
		},
	}
	t, err := template.New("r").Funcs(funcs).Parse(reportTmpl)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return t.Execute(f, r)
}

const reportTmpl = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Routing Decision Benchmark</title>
<style>
:root{--bg:#f7f7f5;--surface:#fff;--ink:#1d1f24;--muted:#6a6f7a;--line:#e3e4e8;--accent:#3b5bdb;
--ok:#2f7d4f;--ok-bg:#e7f4ec;--bad:#b8412c;--bad-bg:#fbeae6;--mid:#9a6a00;--mid-bg:#fbf1d9;
--t0:#dfeee4;--t1:#e3ecf8;--t2:#efe6f7;--t3:#fbe9d9;--t4:#f8dede}
@media (prefers-color-scheme:dark){:root:not([data-theme=light]){--bg:#121317;--surface:#1a1c21;--ink:#e6e7ea;--muted:#9aa0ab;--line:#2c2f36;--accent:#8ea2ff;
--ok:#7fd3a0;--ok-bg:#1b3326;--bad:#f09a86;--bad-bg:#3a211b;--mid:#e9c46a;--mid-bg:#3a3017;
--t0:#1f3328;--t1:#1e2b3d;--t2:#2d2340;--t3:#3d2c1c;--t4:#3f2323}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Inter,sans-serif}
main{max-width:1400px;margin:0 auto;padding:28px 16px 60px}
h1{font-size:24px;margin:0 0 4px;letter-spacing:-.01em}h2{font-size:16px;margin:32px 0 10px}
.sub{color:var(--muted);margin:0 0 20px}
.card{background:var(--surface);border:1px solid var(--line);border-radius:10px;padding:14px 16px;overflow-x:auto}
table{border-collapse:collapse;width:100%}th,td{padding:6px 8px;text-align:left;border-bottom:1px solid var(--line);vertical-align:top}
th{font-size:12px;color:var(--muted);font-weight:600;white-space:nowrap}
.num{text-align:right;font-variant-numeric:tabular-nums;white-space:nowrap}
.metrics td:first-child{color:var(--muted);white-space:nowrap}.metrics td{font-variant-numeric:tabular-nums}
.best{font-weight:700;color:var(--ok)}
.bar{display:flex;height:22px;border-radius:5px;overflow:hidden;margin:4px 0 10px;border:1px solid var(--line)}
.bar span{display:flex;align-items:center;justify-content:center;font-size:11px;white-space:nowrap;overflow:hidden;color:var(--ink)}
.t0{background:var(--t0)}.t1{background:var(--t1)}.t2{background:var(--t2)}.t3{background:var(--t3)}.t4{background:var(--t4)}
.chip{display:inline-block;font-size:11px;padding:1px 7px;border-radius:10px;background:var(--bg);border:1px solid var(--line);color:var(--muted);margin-right:3px}
.ok{color:var(--ok)}.miss{background:var(--bad-bg);color:var(--bad);border-radius:4px;padding:0 4px}
.conf{font-size:11px;padding:0 5px;border-radius:8px;margin-left:4px}.conf.hi{background:var(--ok-bg);color:var(--ok)}.conf.mid{background:var(--mid-bg);color:var(--mid)}.conf.lo{background:var(--bad-bg);color:var(--bad)}
.model{font-size:12px;padding:1px 6px;border-radius:5px;white-space:nowrap}
.prompt{max-width:320px;color:var(--muted);font-size:12px}
.id{font-weight:600;white-space:nowrap}
.gold{background:color-mix(in srgb,var(--accent) 6%,transparent)}
.cell{white-space:nowrap;font-size:12px}.cell .l2{color:var(--muted)}
.filters{display:flex;gap:6px;flex-wrap:wrap;margin:0 0 10px}
.filters button{font:inherit;font-size:12px;padding:4px 10px;border-radius:14px;border:1px solid var(--line);background:var(--surface);color:var(--ink);cursor:pointer}
.filters button.on{background:var(--accent);border-color:var(--accent);color:#fff}
.legend{font-size:12px;color:var(--muted);margin-top:8px}
.err{color:var(--bad);font-size:11px}
</style></head><body><main>
<h1>Routing Decision Benchmark</h1>
<p class="sub">{{len .Cases}} labelled prompts · {{len .Runs}} decision providers · {{.Date}}. Decisions only: no prompt was sent to a chat model. The "gold" route is what the router picks when fed the human labels.</p>

<h2>Summary</h2>
<div class="card"><table class="metrics">
<tr><th></th>{{range .Runs}}<th>{{.Label}}</th>{{end}}</tr>
<tr><td>Topic accuracy</td>{{range .Runs}}<td>{{pct .Summary.TopicAcc}} <span class="chip">acceptable {{pct .Summary.TopicAcceptable}}</span></td>{{end}}</tr>
{{$runs := .Runs}}{{range groups}}{{$g := .}}<tr><td>&nbsp;&nbsp;· {{$g}}</td>{{range $runs}}<td>{{pct (gacc .Summary $g)}}</td>{{end}}</tr>{{end}}
<tr><td>Complexity exact / within ±1</td>{{range .Runs}}<td>{{pct .Summary.CxExact}} / {{pct .Summary.CxNear}}</td>{{end}}</tr>
<tr><td>Risk exact</td>{{range .Runs}}<td>{{pct .Summary.RiskExact}}</td>{{end}}</tr>
<tr><td>Private data detected (model only)</td>{{range .Runs}}<td>{{pct .Summary.PrivateAcc}}</td>{{end}}</tr>
<tr><td>Confident answers (≥ 0.8)</td>{{range .Runs}}<td>{{pct .Summary.HighConfShare}}</td>{{end}}</tr>
<tr><td>Accuracy when confident / unsure</td>{{range .Runs}}<td>{{pct .Summary.HighConfAcc}} / {{pct .Summary.LowConfAcc}}</td>{{end}}</tr>
<tr><td>Calibration error (ECE, lower is better)</td>{{range .Runs}}<td>{{f3 .Summary.ECE}}</td>{{end}}</tr>
<tr><td>Route matches gold route</td>{{range .Runs}}<td>{{pct .Summary.RouteMatch}}</td>{{end}}</tr>
<tr><td>Under- / over-provisioned routes</td>{{range .Runs}}<td>{{.Summary.UnderProvisioned}} / {{.Summary.OverProvisioned}}</td>{{end}}</tr>
<tr><td>Est. model cost: routed / gold</td>{{range .Runs}}<td>{{usd .Summary.RoutedCost}} / {{usd .Summary.GoldCost}} <span class="chip">−{{savings .Summary}} vs always {{short $.Top}}</span></td>{{end}}</tr>
<tr><td>Decision latency p50 / p90</td>{{range .Runs}}<td>{{.Summary.P50}} ms / {{.Summary.P90}} ms</td>{{end}}</tr>
<tr><td>Decision cost per 1,000 requests</td>{{range .Runs}}<td>{{usd4 .Summary.DecisionCostPer1k}}</td>{{end}}</tr>
<tr><td>Agreement with {{(index .Runs 0).Label}}: topic / model</td>{{range $i, $r := .Runs}}<td>{{if $i}}{{pct $r.Summary.AgreeWithFirst}} / {{pct $r.Summary.ModelAgreeWithFirst}}{{else}}—{{end}}</td>{{end}}</tr>
<tr><td>Errors</td>{{range .Runs}}<td>{{.Summary.Errors}}</td>{{end}}</tr>
</table>
<p class="legend">Under-provisioned: routed to a cheaper model than the labels call for (quality risk). Over-provisioned: routed to a pricier one (money wasted). Costs are estimates from prompt length and expected output per complexity level, at live OpenRouter prices.</p></div>

<h2>Where requests would go</h2>
<div class="card">
{{range .Runs}}<div><b>{{.Label}}</b><div class="bar">{{range usage .Summary}}<span class="t{{tier .Model}}" style="width:{{pct1 .Pct}}" title="{{.Model}}: {{.N}}">{{short .Model}} · {{.N}}</span>{{end}}</div></div>{{end}}
<p class="legend">Models, cheapest to most expensive (output $/M): {{range $i, $m := .Config}}<span class="chip t{{$i}}">{{short $m.ID}} ${{f2 $m.Out}}</span>{{end}}</p>
</div>

<h2>Every decision</h2>
<div class="filters" id="filters">
<button class="on" data-f="all">All</button><button data-f="diff">Disagreements only</button>
{{range groups}}<button data-f="{{.}}">{{.}}</button>{{end}}
</div>
<div class="card"><table id="cases">
<tr><th>Case</th><th>Prompt</th><th class="gold">Gold labels → model</th>{{range .Runs}}<th>{{.Label}}</th>{{end}}</tr>
{{range $i, $c := .Cases}}
<tr data-group="{{$c.Group}}" data-diff="{{disagree $runs $i}}">
<td><div class="id">{{$c.ID}}</div><span class="chip">{{$c.Group}}</span>{{if ne $c.Lang "en"}}<span class="chip">{{$c.Lang}}</span>{{end}}{{if $c.Private}}<span class="chip">private</span>{{end}}</td>
<td class="prompt" title="{{trunc $c.Prompt 600}}">{{trunc $c.Prompt 110}}</td>
<td class="cell gold"><div>{{$c.Primary}}</div><div class="l2">cx {{$c.Complexity}} · risk {{$c.Risk}}</div>{{with row (index $runs 0) $i}}<span class="model t{{tier .GoldModel}}">{{short .GoldModel}}</span>{{end}}</td>
{{range $runs}}{{with row . $i}}<td class="cell">
<div><span class="{{if eq .Topic $c.Primary}}ok{{else}}miss{{end}}">{{if .Topic}}{{.Topic}}{{else}}—{{end}}</span><span class="conf {{confClass .TopicConf}}">{{pct .TopicConf}}</span></div>
<div class="l2">cx <span class="{{if ne .Complexity $c.Complexity}}miss{{end}}">{{.Complexity}}</span> ({{f2 .CxScore}}) · risk <span class="{{if ne .Risk $c.Risk}}miss{{end}}">{{.Risk}}</span> · priv {{pct .PrivateP}}</div>
<span class="model t{{tier .Model}}" title="{{.Reason}} · required skill {{f2 .Required}} · est {{usd4 .EstCost}}">{{short .Model}}</span>{{if ne .Model .GoldModel}} <span class="l2">≠ gold</span>{{end}} <span class="l2">{{.Ms}} ms</span>
{{if .Err}}<div class="err">{{trunc .Err 90}}</div>{{end}}
</td>{{end}}{{end}}
</tr>{{end}}
</table></div>
<p class="legend">Confidence chips: green ≥ 80% (router trusts it), amber 50–80%, red &lt; 50%. Below the router's 0.8 threshold the complexity is bumped one level, so unsure decisions go to a stronger model.</p>
</main>
<script>
document.getElementById('filters').addEventListener('click',e=>{const b=e.target.closest('button');if(!b)return;
document.querySelectorAll('#filters button').forEach(x=>x.classList.toggle('on',x===b));const f=b.dataset.f;
document.querySelectorAll('#cases tr[data-group]').forEach(tr=>{tr.style.display=(f==='all'||(f==='diff'&&tr.dataset.diff==='true')||tr.dataset.group===f)?'':'none'})});
</script></body></html>`
