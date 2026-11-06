package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

type req struct {
	at   time.Duration
	ms   float64
	code int
}

func writeResults(t *testing.T, dir, worker string, reqs []req, extra ...string) {
	t.Helper()
	d := filepath.Join(dir, worker)
	os.MkdirAll(d, 0o755)
	var b strings.Builder
	for _, r := range reqs {
		fmt.Fprintf(&b, `{"timestamp":%q,"latency_ms":%g,"status_code":%d}`+"\n", t0.Add(r.at).Format(time.RFC3339Nano), r.ms, r.code)
	}
	for _, e := range extra {
		b.WriteString(e + "\n")
	}
	os.WriteFile(filepath.Join(d, "results.jsonl"), []byte(b.String()), 0o644)
}

func named(names ...string) []workerResult {
	var rs []workerResult
	for _, n := range names {
		rs = append(rs, workerResult{Name: n, Status: "done"})
	}
	return rs
}

func TestSeriesBucketsEveryWorkerBySecond(t *testing.T) {
	dir := t.TempDir()
	// a: 3 requests in second 0, 2 in second 1. b starts 1.5s later: 4 in second 1 (one
	// error), 1 in second 2.
	writeResults(t, dir, "a", []req{{0, 10, 200}, {100 * time.Millisecond, 10, 200}, {200 * time.Millisecond, 10, 200}, {time.Second, 10, 200}, {1500 * time.Millisecond, 10, 200}},
		"not json", `{"latency_ms":5}`) // junk and a line with no timestamp are skipped
	writeResults(t, dir, "b", []req{{1500 * time.Millisecond, 1000, 500}, {1600 * time.Millisecond, 1000, 200}, {1700 * time.Millisecond, 1000, 200}, {1800 * time.Millisecond, 1000, 200}, {2 * time.Second, 1000, 200}})

	s := buildSeries(dir, named("a", "b"))
	if s == nil || s.Step != 1 || len(s.Points) != 3 {
		t.Fatalf("series = %+v", s)
	}
	if !s.Start.Equal(t0) {
		t.Fatalf("start = %v, want the first request %v", s.Start, t0)
	}
	// Fleet requests per second: 3, 2+4, 1.
	for i, want := range []float64{3, 6, 1} {
		if s.Points[i].RPS != want {
			t.Errorf("point %d rps = %v, want %v", i, s.Points[i].RPS, want)
		}
	}
	if s.Points[1].ErrorsPerS != 1 || s.Points[0].ErrorsPerS != 0 {
		t.Errorf("errors/s = %v, %v", s.Points[0].ErrorsPerS, s.Points[1].ErrorsPerS)
	}
	// Second 0 is all 10ms; second 1 mixes 2x10ms with 4x1000ms, so p50 is slow.
	if p := s.Points[0].P95; p < 10 || p > 10.6 {
		t.Errorf("second 0 p95 = %v, want ~10", p)
	}
	if p := s.Points[1].P50; p < 1000 || p > 1060 {
		t.Errorf("second 1 p50 = %v, want ~1000", p)
	}
	if len(s.Workers) != 2 || s.Workers[0].Name != "a" || s.Workers[1].Offset != 1500*time.Millisecond || s.Workers[0].Offset != 0 {
		t.Fatalf("workers = %+v", s.Workers)
	}
	if got := s.Workers[1].RPS; got[0] != 0 || got[1] != 4 || got[2] != 1 {
		t.Errorf("worker b rps = %v", got)
	}
}

func TestSeriesLongRunsAreSteppedAndKeepEveryRequest(t *testing.T) {
	dir := t.TempDir()
	var rs []req
	for sec := 0; sec < 1500; sec++ { // 1500s: more than maxPoints, so steps of 3
		rs = append(rs, req{time.Duration(sec) * time.Second, 5, 200}, req{time.Duration(sec)*time.Second + time.Millisecond, 5, 200})
	}
	writeResults(t, dir, "a", rs)
	s := buildSeries(dir, named("a"))
	if s.Step != 3 || len(s.Points) != 500 {
		t.Fatalf("step=%d points=%d, want 3 and 500", s.Step, len(s.Points))
	}
	total := 0.0
	for _, p := range s.Points {
		total += p.RPS * float64(s.Step)
	}
	if total != 3000 {
		t.Fatalf("requests across points = %v, want 3000", total)
	}
}

func TestSeriesWithNothingToShow(t *testing.T) {
	dir := t.TempDir()
	if buildSeries(dir, named("missing")) != nil {
		t.Fatal("no file should give no series")
	}
	writeResults(t, dir, "empty", nil, `{"latency_ms":5}`)
	if buildSeries(dir, named("empty")) != nil {
		t.Fatal("no timestamps should give no series")
	}
}

func sampleInput(t *testing.T) reportInput {
	dir := t.TempDir()
	writeResults(t, dir, "w-a", []req{{0, 10, 200}, {time.Second, 12, 200}})
	writeResults(t, dir, "w-b", []req{{100 * time.Millisecond, 20, 500}, {time.Second, 22, 200}})
	results := []workerResult{
		{Name: "w-a", Status: "done", Report: &reportSummary{Requests: 2, SuccessRate: 1}},
		{Name: "w-b", Status: "failed", Err: "assertion failed", Report: &reportSummary{Requests: 2, SuccessRate: 0.5}},
	}
	agg := &aggregate{Workers: 2, Requests: 4, Success: 3, SuccessRate: 0.75, DurationNanos: 2e9, RequestsPerSec: 2,
		Latencies: latencySummary{P50: 1e7, P95: 2e7, P99: 2.2e7, Max: 2.2e7}, Notes: []string{"w-c: no usable report.json, left out"}}
	ths, _ := parseThresholds([]string{"p95<300ms", "success_rate>=99%"})
	return reportInput{RunID: "abc123", Scenario: "checkout.yaml", Generated: t0, Version: "v9", Aggregate: agg,
		Results: results, Thresholds: evalThresholds(ths, agg), Series: buildSeries(dir, results)}
}

func TestFleetReportContents(t *testing.T) {
	b, err := renderFleetReport(sampleInput(t))
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		"Fleet report", "abc123", "checkout.yaml", "2 worker(s)",
		"75.00%", "2.0 req/s", "p99", // tiles
		"Thresholds", "FAILED", "p95&lt;300ms", "PASS", "success_rate&gt;=99%", "FAIL", // checks (escaped)
		"Throughput", "Latency", "Per worker",
		`<a href="w-a/report.html">report.html</a>`, "assertion failed", "&#43;100ms",
		"no usable report.json, left out", "resonatectl v9",
		"prefers-color-scheme:dark",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if n := strings.Count(html, "<polyline"); n != 2+3+2 { // throughput 2, latency 3, two workers
		t.Errorf("polylines = %d, want 7", n)
	}
	if strings.Contains(html, "ZgotmplZ") {
		t.Error("html/template rejected a value somewhere")
	}
	// Self-contained: nothing is fetched from anywhere but the SVG namespace.
	if bad := regexp.MustCompile(`(src|href)="https?:|<script|@import|url\(http`).FindString(html); bad != "" {
		t.Errorf("report reaches outside itself: %q", bad)
	}
}

func TestFleetReportEscapesWhatItIsGiven(t *testing.T) {
	in := sampleInput(t)
	in.Scenario = `<script>alert(1)</script>`
	in.Results[0].Name = `w-a"><img src=x onerror=alert(2)>`
	in.Results[1].Err = `</td><script>alert(3)</script>`
	b, err := renderFleetReport(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "<script") || strings.Contains(string(b), "<img") {
		t.Fatalf("unescaped markup in the report")
	}
}

func TestFleetReportWithNoDataStillRenders(t *testing.T) {
	b, err := renderFleetReport(reportInput{RunID: "r", Generated: t0, Results: []workerResult{{Name: "w", Status: "error", Err: "unreachable"}}})
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	if !strings.Contains(html, "No worker produced a report") || !strings.Contains(html, "unreachable") || strings.Contains(html, "<svg") {
		t.Fatalf("unexpected degenerate report:\n%s", html)
	}
}

func TestNiceTicks(t *testing.T) {
	for _, tc := range []struct {
		max  float64
		top  float64
		want int // ticks
	}{{0, 1, 2}, {97, 100, 5}, {3.2, 4, 5}, {1234, 1500, 4}, {0.007, 0.008, 5}} {
		ticks, top := niceTicks(tc.max)
		if top < tc.max || top != ticks[len(ticks)-1] || ticks[0] != 0 {
			t.Errorf("niceTicks(%v) = %v top %v: must start at 0 and cover the max", tc.max, ticks, top)
		}
		if len(ticks) < 2 || len(ticks) > 7 {
			t.Errorf("niceTicks(%v) gave %d ticks", tc.max, len(ticks))
		}
	}
	if _, top := niceTicks(97); top != 100 {
		t.Errorf("niceTicks(97) top = %v, want 100", top)
	}
}

func TestLineChartIsPlainSVG(t *testing.T) {
	svg := string(lineChart([]float64{0, 1, 2}, []line{{Name: "a", Color: "var(--c1)", Ys: []float64{1, 5, 3}}, {Name: "b", Color: "red", Ys: []float64{0, 0, 0}, Dash: true}}, "req/s"))
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") || strings.Count(svg, "<polyline") != 2 || !strings.Contains(svg, "stroke-dasharray") {
		t.Fatalf("svg = %s", svg)
	}
	if lineChart(nil, nil, "x") != "" {
		t.Fatal("no points should draw nothing")
	}
}
