package main

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"
)

// reportSummary is the subset of resonate's report.json this tool reads for
// the per-worker table. The fleet total is built separately, in aggregate.go.
type reportSummary struct {
	Requests       int     `json:"requests"`
	SuccessRate    float64 `json:"success_rate"`
	RequestsPerSec float64 `json:"requests_per_sec"`
	DurationNanos  int64   `json:"duration"`
	Latencies      struct {
		P95Nanos int64 `json:"p95"`
	} `json:"latencies"`
}

// workerResult is one worker's outcome, gathered independently of every other
// worker - nothing here is combined across the results slice.
type workerResult struct {
	Name     string
	TaskID   string
	Status   string // "done", "failed", or "error" (the controller couldn't submit to or poll the worker)
	ExitCode int
	Err      string // resonate's own error (status "failed"), or the controller's (status "error")
	Warning  string // a note that doesn't fail the worker, e.g. it started late
	Report   *reportSummary
}

// printTable writes one row per worker to w, sorted by name so repeated runs
// (and tests) are deterministic regardless of which worker finished first.
func printTable(w io.Writer, results []workerResult) {
	sorted := append([]workerResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKER\tSTATUS\tREQUESTS\tSUCCESS\tP95\tDURATION\tERROR")
	for _, r := range sorted {
		requests, success, p95, dur := "-", "-", "-", "-"
		if r.Report != nil {
			requests = fmt.Sprintf("%d", r.Report.Requests)
			success = fmt.Sprintf("%.1f%%", r.Report.SuccessRate*100)
			p95 = time.Duration(r.Report.Latencies.P95Nanos).String()
			dur = time.Duration(r.Report.DurationNanos).String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Status, requests, success, p95, dur, r.Err)
	}
	tw.Flush()
}

// anyFailed reports whether the run should exit non-zero: any worker
// unreachable, or any worker's task not "done".
func anyFailed(results []workerResult) bool {
	for _, r := range results {
		if r.Status != "done" {
			return true
		}
	}
	return false
}
