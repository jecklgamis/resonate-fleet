package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"
)

// aggregate is one result for the whole fleet, built from every worker's own
// output. Latency percentiles come from the individual requests in each worker's
// results.jsonl, so they are exact: percentiles can't be averaged across workers,
// but they can be computed over the union of every request.
type aggregate struct {
	Workers        int            `json:"workers"`          // workers that contributed
	Requests       int            `json:"requests"`         // summed from each worker's report.json
	Success        int            `json:"success"`          // likewise
	SuccessRate    float64        `json:"success_rate"`     // Success / Requests
	DurationNanos  int64          `json:"duration"`         // the longest worker run
	RequestsPerSec float64        `json:"requests_per_sec"` // Requests / duration
	Latencies      latencySummary `json:"latencies"`        // nanoseconds, over every request seen
	StatusCodes    map[string]int `json:"status_codes"`
	Samples        int            `json:"latency_samples"` // requests the latencies were computed from
	Notes          []string       `json:"notes,omitempty"` // why the numbers may be incomplete
}

type latencySummary struct {
	Min  int64 `json:"min"`
	Mean int64 `json:"mean"`
	P50  int64 `json:"p50"`
	P75  int64 `json:"p75"`
	P90  int64 `json:"p90"`
	P95  int64 `json:"p95"`
	P99  int64 `json:"p99"`
	Max  int64 `json:"max"`
}

// fullReport is the part of resonate's report.json that aggregation reads.
type fullReport struct {
	reportSummary
	Success     int            `json:"success"`
	StatusCodes map[string]int `json:"status_codes"`
}

// buildAggregate combines the workers that finished (done or failed - a failed
// assertion still ran a load) using the files fetched into outDir/<worker>/.
// It returns nil when no worker produced a report.
func buildAggregate(outDir string, results []workerResult) *aggregate {
	agg := &aggregate{StatusCodes: map[string]int{}}
	var lat []float64
	for _, r := range results {
		dir := filepath.Join(outDir, r.Name)
		var rep fullReport
		b, err := os.ReadFile(filepath.Join(dir, "report.json"))
		if err != nil || json.Unmarshal(b, &rep) != nil {
			if r.Status != "error" {
				agg.Notes = append(agg.Notes, fmt.Sprintf("%s: no usable report.json, left out", r.Name))
			}
			continue
		}
		agg.Workers++
		agg.Requests += rep.Requests
		agg.Success += rep.Success
		if rep.DurationNanos > agg.DurationNanos {
			agg.DurationNanos = rep.DurationNanos
		}
		for code, n := range rep.StatusCodes {
			agg.StatusCodes[code] += n
		}
		l, err := readLatenciesMs(filepath.Join(dir, "results.jsonl"))
		if err != nil {
			agg.Notes = append(agg.Notes, fmt.Sprintf("%s: no results.jsonl, its requests are in the totals but not the percentiles", r.Name))
		}
		lat = append(lat, l...)
	}
	if agg.Workers == 0 {
		return nil
	}
	if agg.Requests > 0 {
		agg.SuccessRate = float64(agg.Success) / float64(agg.Requests)
	}
	if agg.DurationNanos > 0 {
		agg.RequestsPerSec = float64(agg.Requests) / (time.Duration(agg.DurationNanos).Seconds())
	}
	agg.Samples = len(lat)
	agg.Latencies = summarize(lat)
	return agg
}

// readLatenciesMs reads the latency_ms of every line of a results.jsonl.
func readLatenciesMs(path string) ([]float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []float64
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var l struct {
				LatencyMs *float64 `json:"latency_ms"`
			}
			if json.Unmarshal(line, &l) == nil && l.LatencyMs != nil {
				out = append(out, *l.LatencyMs)
			}
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// summarize turns latencies in milliseconds into nanosecond percentiles
// (nearest-rank).
func summarize(ms []float64) latencySummary {
	if len(ms) == 0 {
		return latencySummary{}
	}
	sort.Float64s(ms)
	var sum float64
	for _, v := range ms {
		sum += v
	}
	ns := func(v float64) int64 { return int64(v * float64(time.Millisecond)) }
	pct := func(p float64) int64 {
		i := int(math.Ceil(p/100*float64(len(ms)))) - 1
		return ns(ms[min(max(i, 0), len(ms)-1)])
	}
	return latencySummary{
		Min: ns(ms[0]), Mean: ns(sum / float64(len(ms))),
		P50: pct(50), P75: pct(75), P90: pct(90), P95: pct(95), P99: pct(99),
		Max: ns(ms[len(ms)-1]),
	}
}

func writeAggregate(path string, a *aggregate) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func printAggregate(w io.Writer, a *aggregate) {
	d := func(ns int64) string { return time.Duration(ns).Round(10 * time.Microsecond).String() }
	fmt.Fprintf(w, "\nFLEET TOTAL  (%d worker(s))\n", a.Workers)
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "requests\t%d\n", a.Requests)
	fmt.Fprintf(tw, "success\t%.2f%%\n", a.SuccessRate*100)
	fmt.Fprintf(tw, "throughput\t%.1f req/s over %s\n", a.RequestsPerSec, time.Duration(a.DurationNanos).Round(time.Millisecond))
	fmt.Fprintf(tw, "latency\tp50 %s  p90 %s  p95 %s  p99 %s  max %s\n",
		d(a.Latencies.P50), d(a.Latencies.P90), d(a.Latencies.P95), d(a.Latencies.P99), d(a.Latencies.Max))
	tw.Flush()
	for _, n := range a.Notes {
		fmt.Fprintln(w, "note:", n)
	}
}
