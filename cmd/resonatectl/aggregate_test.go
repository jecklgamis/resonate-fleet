package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeWorker(t *testing.T, dir, name string, report string, latenciesMs []float64) {
	t.Helper()
	d := filepath.Join(dir, name)
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "report.json"), []byte(report), 0o644)
	var b strings.Builder
	for _, l := range latenciesMs {
		fmt.Fprintf(&b, `{"latency_ms":%g,"status_code":200}`+"\n", l)
	}
	os.WriteFile(filepath.Join(d, "results.jsonl"), []byte(b.String()), 0o644)
}

func TestAggregateComputesExactPercentilesAcrossWorkers(t *testing.T) {
	dir := t.TempDir()
	// Worker a is fast (10ms), worker b slow (1s). Averaging per-worker percentiles
	// would put p50 at ~505ms; over the union of all 200 requests it is 10ms.
	var fast, slow []float64
	for i := 0; i < 100; i++ {
		fast = append(fast, 10)
		slow = append(slow, 1000)
	}
	writeWorker(t, dir, "a", `{"requests":100,"success":100,"duration":10000000000,"status_codes":{"200":100}}`, fast)
	writeWorker(t, dir, "b", `{"requests":100,"success":90,"duration":8000000000,"status_codes":{"200":90,"500":10}}`, slow)

	agg := buildAggregate(dir, []workerResult{{Name: "a", Status: "done"}, {Name: "b", Status: "done"}})
	if agg == nil {
		t.Fatal("no aggregate")
	}
	if agg.Workers != 2 || agg.Requests != 200 || agg.Success != 190 {
		t.Fatalf("totals = %+v", agg)
	}
	if agg.SuccessRate != 0.95 {
		t.Fatalf("success rate = %v, want 0.95", agg.SuccessRate)
	}
	if agg.DurationNanos != 10e9 || agg.RequestsPerSec != 20 {
		t.Fatalf("duration=%d rps=%v, want 10s and 20", agg.DurationNanos, agg.RequestsPerSec)
	}
	if agg.StatusCodes["200"] != 190 || agg.StatusCodes["500"] != 10 {
		t.Fatalf("status codes = %v", agg.StatusCodes)
	}
	if got := time.Duration(agg.Latencies.P50); got != 10*time.Millisecond {
		t.Fatalf("p50 = %v, want 10ms (100th of 200 sorted)", got)
	}
	if got := time.Duration(agg.Latencies.P95); got != time.Second {
		t.Fatalf("p95 = %v, want 1s", got)
	}
	if agg.Samples != 200 {
		t.Fatalf("samples = %d", agg.Samples)
	}
}

func TestAggregateSkipsWorkersWithoutAReport(t *testing.T) {
	dir := t.TempDir()
	writeWorker(t, dir, "a", `{"requests":10,"success":10,"duration":1000000000}`, []float64{5, 5, 5, 5, 5, 5, 5, 5, 5, 5})
	agg := buildAggregate(dir, []workerResult{
		{Name: "a", Status: "done"},
		{Name: "b", Status: "failed"}, // never wrote a report
		{Name: "c", Status: "error"},  // unreachable: expected, no note
	})
	if agg.Workers != 1 || agg.Requests != 10 {
		t.Fatalf("agg = %+v", agg)
	}
	if len(agg.Notes) != 1 || !strings.Contains(agg.Notes[0], "b:") {
		t.Fatalf("notes = %v, want one about b", agg.Notes)
	}
	if buildAggregate(t.TempDir(), []workerResult{{Name: "x", Status: "error"}}) != nil {
		t.Fatal("aggregate with no reports should be nil")
	}
}

func TestParseThreshold(t *testing.T) {
	for _, tc := range []struct {
		in     string
		metric string
		op     string
		val    float64
	}{
		{"p95<300ms", "p95", "<", float64(300 * time.Millisecond)},
		{"p99 <= 1.5s", "p99", "<=", float64(1500 * time.Millisecond)},
		{"success_rate>=99%", "success_rate", ">=", 0.99},
		{"error_rate<0.01", "error_rate", "<", 0.01},
		{"rps>100", "rps", ">", 100},
	} {
		th, err := parseThreshold(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if th.Metric != tc.metric || th.Op != tc.op || th.Value != tc.val {
			t.Errorf("%q = %+v, want %s %s %v", tc.in, th, tc.metric, tc.op, tc.val)
		}
	}
	for _, bad := range []string{"", "p95", "<300ms", "p95<fast", "bogus<1", "rps<5%", "p95=300ms", "success_rate>abc"} {
		if _, err := parseThreshold(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestEvalThresholds(t *testing.T) {
	agg := &aggregate{Requests: 1000, Success: 985, SuccessRate: 0.985, RequestsPerSec: 100,
		Latencies: latencySummary{P95: int64(200 * time.Millisecond), P99: int64(900 * time.Millisecond)}}
	ts, err := parseThresholds([]string{"p95<300ms", "p99<500ms", "success_rate>=99%", "error_rate<=2%", "rps>=100"})
	if err != nil {
		t.Fatal(err)
	}
	got := evalThresholds(ts, agg)
	want := []bool{true, false, false, true, true}
	for i, r := range got {
		if r.Pass != want[i] {
			t.Errorf("%s: pass=%v (actual %v), want %v", r.Threshold.Expr, r.Pass, r.Actual, want[i])
		}
	}
	if !thresholdsFailed(got) {
		t.Fatal("thresholdsFailed should be true")
	}

	var out bytes.Buffer
	printThresholds(&out, got)
	for _, s := range []string{"PASS  p95<300ms", "FAIL  p99<500ms", "(got 900ms)", "FAIL  success_rate>=99%", "(got 98.50%)"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output missing %q:\n%s", s, out.String())
		}
	}

	// No aggregate at all: every threshold fails rather than silently passing.
	if !thresholdsFailed(evalThresholds(ts, nil)) {
		t.Fatal("thresholds must fail when there is no data")
	}
	if thresholdsFailed(evalThresholds(nil, agg)) {
		t.Fatal("no thresholds can't fail")
	}
}
