package main

import (
	"bytes"
	"fmt"
	"html/template"
	"math"
	"os"
	"strings"
	"time"
)

// reportInput is everything the fleet report shows.
type reportInput struct {
	RunID      string
	Scenario   string
	Generated  time.Time
	Version    string
	Aggregate  *aggregate
	Results    []workerResult
	Thresholds []thresholdResult
	Series     *fleetSeries // nil when no request had a timestamp
}

// Chart colors are CSS variables so the report follows the viewer's light or dark
// theme. Workers cycle through the palette.
var workerColors = []string{"var(--c1)", "var(--c2)", "var(--c3)", "var(--c4)", "var(--c5)", "var(--c6)"}

type line struct {
	Name   string
	Color  string // a CSS color for the chart's stroke
	Swatch string // a CSS class that paints the same color in the legend
	Ys     []float64
	Dash   bool
}

// lineChart draws lines over a shared x axis as inline SVG. Everything it writes is
// numbers and fixed text, so the result is safe as template.HTML.
func lineChart(xs []float64, lines []line, yUnit string) template.HTML {
	const w, h, ml, mr, mt, mb = 760, 240, 54, 12, 10, 28
	if len(xs) == 0 {
		return ""
	}
	ymax := 0.0
	for _, l := range lines {
		for _, y := range l.Ys {
			ymax = math.Max(ymax, y)
		}
	}
	yticks, ytop := niceTicks(ymax)
	xmax := xs[len(xs)-1]
	if xmax <= 0 {
		xmax = 1
	}
	px := func(x float64) float64 { return ml + x/xmax*(w-ml-mr) }
	py := func(y float64) float64 { return mt + (1-y/ytop)*(h-mt-mb) }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg viewBox="0 0 %d %d" role="img" aria-label="chart" xmlns="http://www.w3.org/2000/svg">`, w, h)
	for _, t := range yticks {
		fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" class="grid"/><text x="%d" y="%.1f" class="tick" text-anchor="end">%s</text>`,
			ml, w-mr, py(t), py(t), ml-6, py(t)+4, fmtTick(t))
	}
	xticks, _ := niceTicks(xmax)
	for _, t := range xticks {
		if t > xmax {
			continue
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="tick" text-anchor="middle">%s</text>`, px(t), h-8, fmtSeconds(t))
	}
	fmt.Fprintf(&b, `<text x="12" y="%d" class="tick" transform="rotate(-90 12 %d)" text-anchor="middle">%s</text>`, h/2, h/2, yUnit)
	for _, l := range lines {
		var pts strings.Builder
		for i, y := range l.Ys {
			if i >= len(xs) {
				break
			}
			fmt.Fprintf(&pts, "%.1f,%.1f ", px(xs[i]), py(y))
		}
		dash := ""
		if l.Dash {
			dash = ` stroke-dasharray="5 4"`
		}
		fmt.Fprintf(&b, `<polyline fill="none" stroke="%s" stroke-width="1.8" stroke-linejoin="round"%s points="%s"/>`, l.Color, dash, strings.TrimSpace(pts.String()))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// niceTicks returns round tick values from 0 up to a round top that covers max.
func niceTicks(max float64) (ticks []float64, top float64) {
	if !(max > 0) {
		return []float64{0, 1}, 1
	}
	raw := max / 4
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	step := mag
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		step = m * mag
		if raw <= step {
			break
		}
	}
	n := int(math.Ceil(max/step - 1e-9))
	for i := 0; i <= n; i++ {
		ticks = append(ticks, float64(i)*step)
	}
	return ticks, ticks[len(ticks)-1]
}

func fmtTick(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.3g", v)
}

func fmtSeconds(s float64) string {
	if s >= 120 {
		return fmt.Sprintf("%.0fm", s/60)
	}
	return fmt.Sprintf("%.0fs", s)
}

type tile struct{ Label, Value string }

type workerRow struct {
	Name, Status, Requests, Success, P95, Offset, Error, Link string
	OK                                                        bool
}

type checkRow struct {
	Expr, Got string
	Pass      bool
}

type reportView struct {
	reportInput
	Tiles          []tile
	Rows           []workerRow
	Checks         []checkRow
	ChecksFailed   bool
	Throughput     template.HTML
	Latency        template.HTML
	PerWorker      template.HTML
	WorkerLegend   []line
	HasSeries      bool
	Window         string
	StepNote       string
	GeneratedLabel string
}

var reportTmpl = template.Must(template.New("report").Parse(reportHTML))

// renderFleetReport builds the report as one self-contained HTML document.
func renderFleetReport(in reportInput) ([]byte, error) {
	v := reportView{reportInput: in, GeneratedLabel: in.Generated.Format("2006-01-02 15:04:05 MST")}
	a := in.Aggregate
	d := func(ns int64) string { return time.Duration(ns).Round(10 * time.Microsecond).String() }
	if a != nil {
		v.Tiles = []tile{
			{"Requests", fmt.Sprintf("%d", a.Requests)},
			{"Success", fmt.Sprintf("%.2f%%", a.SuccessRate*100)},
			{"Throughput", fmt.Sprintf("%.1f req/s", a.RequestsPerSec)},
			{"Duration", time.Duration(a.DurationNanos).Round(time.Millisecond).String()},
			{"p50", d(a.Latencies.P50)},
			{"p95", d(a.Latencies.P95)},
			{"p99", d(a.Latencies.P99)},
			{"Max", d(a.Latencies.Max)},
		}
	}
	for _, r := range in.Results {
		row := workerRow{Name: r.Name, Status: r.Status, Requests: "-", Success: "-", P95: "-", Offset: "-", Error: r.Err, OK: r.Status == "done"}
		if r.Report != nil {
			row.Requests = fmt.Sprintf("%d", r.Report.Requests)
			row.Success = fmt.Sprintf("%.1f%%", r.Report.SuccessRate*100)
			row.P95 = time.Duration(r.Report.Latencies.P95Nanos).Round(10 * time.Microsecond).String()
			row.Link = r.Name + "/report.html"
		}
		if in.Series != nil {
			for _, w := range in.Series.Workers {
				if w.Name == r.Name {
					row.Offset = "+" + w.Offset.Round(time.Millisecond).String()
				}
			}
		}
		v.Rows = append(v.Rows, row)
	}
	for _, c := range in.Thresholds {
		v.Checks = append(v.Checks, checkRow{
			Expr: c.Threshold.Metric + c.Threshold.Op + c.Threshold.Raw,
			Got:  c.Threshold.show(c.Actual), Pass: c.Pass})
	}
	v.ChecksFailed = thresholdsFailed(in.Thresholds)

	if s := in.Series; s != nil && len(s.Points) > 0 {
		v.HasSeries = true
		xs := make([]float64, len(s.Points))
		rps, errs, p50, p95, p99 := make([]float64, len(xs)), make([]float64, len(xs)), make([]float64, len(xs)), make([]float64, len(xs)), make([]float64, len(xs))
		for i, p := range s.Points {
			xs[i], rps[i], errs[i], p50[i], p95[i], p99[i] = p.T, p.RPS, p.ErrorsPerS, p.P50, p.P95, p.P99
		}
		v.Throughput = lineChart(xs, []line{{Name: "requests/s", Color: "var(--c1)", Ys: rps}, {Name: "errors/s", Color: "var(--bad)", Ys: errs, Dash: true}}, "req/s")
		v.Latency = lineChart(xs, []line{{Name: "p50", Color: "var(--c2)", Ys: p50}, {Name: "p95", Color: "var(--c1)", Ys: p95}, {Name: "p99", Color: "var(--c3)", Ys: p99}}, "ms")
		for i, w := range s.Workers {
			v.WorkerLegend = append(v.WorkerLegend, line{Name: w.Name, Color: workerColors[i%len(workerColors)], Swatch: fmt.Sprintf("sw%d", i%len(workerColors)+1), Ys: w.RPS})
		}
		v.PerWorker = lineChart(xs, v.WorkerLegend, "req/s per worker")
		v.Window = fmt.Sprintf("from %s", s.Start.Local().Format("15:04:05"))
		if s.Step > 1 {
			v.StepNote = fmt.Sprintf("Each point covers %d seconds.", s.Step)
		}
	}
	var buf bytes.Buffer
	if err := reportTmpl.Execute(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeFleetReport(path string, in reportInput) error {
	b, err := renderFleetReport(in)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

const reportHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Fleet report {{.RunID}}</title>
<style>
:root{--bg:#fff;--fg:#1c2024;--muted:#60646c;--line:#e0e1e6;--card:#f6f7f9;--ok:#1a7f4b;--bad:#c62a2f;
--c1:#2563eb;--c2:#0d9488;--c3:#d97706;--c4:#7c3aed;--c5:#db2777;--c6:#65a30d}
@media (prefers-color-scheme:dark){:root{--bg:#111316;--fg:#e8eaed;--muted:#9aa0a8;--line:#2a2e35;--card:#1a1d22;--ok:#4ade80;--bad:#f87171;
--c1:#60a5fa;--c2:#2dd4bf;--c3:#fbbf24;--c4:#a78bfa;--c5:#f472b6;--c6:#a3e635}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
main{max-width:860px;margin:0 auto;padding:24px 16px 48px}
h1{font-size:1.4rem;margin:0 0 4px}h2{font-size:1.05rem;margin:32px 0 10px}
.sub{color:var(--muted);margin:0 0 20px}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(130px,1fr));gap:10px}
.tile{background:var(--card);border:1px solid var(--line);border-radius:8px;padding:10px 12px}
.tile b{display:block;font-size:1.15rem;font-variant-numeric:tabular-nums}.tile span{color:var(--muted);font-size:.8rem}
table{width:100%;border-collapse:collapse;font-variant-numeric:tabular-nums}
th,td{text-align:left;padding:6px 8px;border-bottom:1px solid var(--line);font-size:.9rem}
th{color:var(--muted);font-weight:600}td.n,th.n{text-align:right}
.pass{color:var(--ok);font-weight:600}.fail{color:var(--bad);font-weight:600}
.chart{background:var(--card);border:1px solid var(--line);border-radius:8px;padding:8px}
svg{width:100%;height:auto;display:block}
.grid{stroke:var(--line);stroke-width:1}.tick{fill:var(--muted);font-size:11px}
.legend{display:flex;flex-wrap:wrap;gap:4px 14px;margin:6px 4px 0;font-size:.85rem;color:var(--muted)}
.legend i{display:inline-block;width:12px;height:3px;border-radius:2px;margin-right:6px;vertical-align:middle}
.sw1{background:var(--c1)}.sw2{background:var(--c2)}.sw3{background:var(--c3)}.sw4{background:var(--c4)}.sw5{background:var(--c5)}.sw6{background:var(--c6)}
.note{color:var(--muted);font-size:.85rem}.table-wrap{overflow-x:auto}
a{color:var(--c1)}
</style>
</head>
<body>
<main>
<h1>Fleet report</h1>
<p class="sub">Run <code>{{.RunID}}</code>{{if .Scenario}} · {{.Scenario}}{{end}} · {{len .Results}} worker(s) · {{.GeneratedLabel}}</p>

{{if .Tiles}}<div class="tiles">{{range .Tiles}}<div class="tile"><b>{{.Value}}</b><span>{{.Label}}</span></div>{{end}}</div>
{{else}}<p class="note">No worker produced a report, so there is no fleet total.</p>{{end}}

{{if .Checks}}<h2>Thresholds <span class="{{if .ChecksFailed}}fail{{else}}pass{{end}}">{{if .ChecksFailed}}FAILED{{else}}PASSED{{end}}</span></h2>
<table><thead><tr><th>Threshold</th><th>Fleet total</th><th></th></tr></thead><tbody>
{{range .Checks}}<tr><td><code>{{.Expr}}</code></td><td>{{.Got}}</td><td class="{{if .Pass}}pass{{else}}fail{{end}}">{{if .Pass}}PASS{{else}}FAIL{{end}}</td></tr>{{end}}
</tbody></table>{{end}}

{{if .HasSeries}}
<h2>Throughput</h2>
<div class="chart">{{.Throughput}}</div>
<div class="legend"><span><i style="background:var(--c1)"></i>requests/s</span><span><i style="background:var(--bad)"></i>errors/s</span></div>

<h2>Latency</h2>
<div class="chart">{{.Latency}}</div>
<div class="legend"><span><i style="background:var(--c2)"></i>p50</span><span><i style="background:var(--c1)"></i>p95</span><span><i style="background:var(--c3)"></i>p99</span></div>

<h2>Per worker</h2>
<div class="chart">{{.PerWorker}}</div>
<div class="legend">{{range .WorkerLegend}}<span><i class="{{.Swatch}}"></i>{{.Name}}</span>{{end}}</div>
<p class="note">Time is seconds {{.Window}}, the fleet's first request. {{.StepNote}} These curves are computed over every request of every worker, by its timestamp. Percentiles in the charts come from 5%-wide buckets, so they are within about 5% of exact; the tiles above are exact. Workers' clocks are as aligned as their NTP sync.</p>
{{end}}

<h2>Workers</h2>
<div class="table-wrap"><table><thead><tr><th>Worker</th><th>Status</th><th class="n">Requests</th><th class="n">Success</th><th class="n">p95</th><th class="n">Started</th><th>Report</th></tr></thead><tbody>
{{range .Rows}}<tr><td>{{.Name}}</td><td class="{{if .OK}}pass{{else}}fail{{end}}">{{.Status}}</td><td class="n">{{.Requests}}</td><td class="n">{{.Success}}</td><td class="n">{{.P95}}</td><td class="n">{{.Offset}}</td><td>{{if .Link}}<a href="{{.Link}}">report.html</a>{{else}}-{{end}}{{if .Error}} <span class="note">{{.Error}}</span>{{end}}</td></tr>{{end}}
</tbody></table></div>

{{if .Aggregate}}{{if .Aggregate.Notes}}<h2>Notes</h2>{{range .Aggregate.Notes}}<p class="note">{{.}}</p>{{end}}{{end}}{{end}}
<p class="note" style="margin-top:32px">Generated by resonatectl {{.Version}}.</p>
</main>
</body>
</html>
`
