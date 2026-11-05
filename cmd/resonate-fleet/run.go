package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"text/tabwriter"
	"time"
)

// reportSummary is the subset of resonate's report.json this tool reads for
// the per-worker table — never the whole report, and never combined across
// workers (see the fleet README: workers report independently, by design).
type reportSummary struct {
	Requests       int     `json:"requests"`
	SuccessRate    float64 `json:"success_rate"`
	RequestsPerSec float64 `json:"requests_per_sec"`
	DurationNanos  int64   `json:"duration"`
	Latencies      struct {
		P95Nanos int64 `json:"p95"`
	} `json:"latencies"`
}

// workerResult is one worker's outcome, gathered independently of every
// other worker — nothing here is combined across the Results slice.
type workerResult struct {
	Worker   Worker
	TaskID   string
	Status   string // "done", "failed", or "error" (fleet-side: couldn't submit/poll/fetch)
	ExitCode int
	Err      string // resonate's own error (status=="failed"), or this tool's own error (status=="error")
	Report   *reportSummary
}

// runFleet submits scenarioPath to every worker in cfg concurrently, polls
// each independently, and downloads each worker's report files into
// outDir/<worker-name>/. One worker's failure (reachability, resonate
// failing, a timed-out poll) never stops or affects any other worker's run —
// each goroutine owns exactly one worker end to end.
func runFleet(ctx context.Context, cfg *FleetConfig, scenarioPath, outDir string, pollInterval time.Duration) []workerResult {
	results := make([]workerResult, len(cfg.Workers))
	var wg sync.WaitGroup
	for i, w := range cfg.Workers {
		wg.Add(1)
		go func(i int, w Worker) {
			defer wg.Done()
			results[i] = runOne(ctx, w, scenarioPath, filepath.Join(outDir, w.Name), pollInterval)
		}(i, w)
	}
	wg.Wait()
	return results
}

func runOne(ctx context.Context, w Worker, scenarioPath, workerDir string, pollInterval time.Duration) workerResult {
	res := workerResult{Worker: w, Status: "error"}
	c := newWorkerClient(w)

	taskID, err := c.Submit(ctx, scenarioPath)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	res.TaskID = taskID

	snap, err := c.Poll(ctx, taskID, pollInterval)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	res.Status = snap.Status
	res.ExitCode = snap.ExitCode
	res.Err = snap.Error

	// A done or failed task still has its report files (resonate writes them
	// whether or not its own assertions passed) — fetch them regardless of
	// which of the two it is.
	for _, name := range []string{"report.html", "report.json", "results.jsonl", "log"} {
		dest := filepath.Join(workerDir, name)
		if err := c.FetchFile(ctx, taskID, name, dest); err != nil {
			// A missing file (e.g. results.jsonl for a task that failed before
			// resonate ran) isn't fatal to the fleet run — note it and move on.
			fmt.Fprintf(os.Stderr, "warning: %s: %s: %v\n", w.Name, name, err)
			continue
		}
		if name == "report.json" {
			if b, err := os.ReadFile(dest); err == nil {
				var rs reportSummary
				if json.Unmarshal(b, &rs) == nil {
					res.Report = &rs
				}
			}
		}
	}
	return res
}

// printTable writes one row per worker, in the order given, to w — sorted by
// name so repeated runs (and tests) are deterministic regardless of which
// goroutine finished first.
func printTable(w io.Writer, results []workerResult) {
	sorted := append([]workerResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Worker.Name < sorted[j].Worker.Name })

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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Worker.Name, r.Status, requests, success, p95, dur, r.Err)
	}
	tw.Flush()
}

// anyFailed reports whether the fleet run should exit non-zero: any worker
// unreachable, or any worker's task not "done".
func anyFailed(results []workerResult) bool {
	for _, r := range results {
		if r.Status != "done" {
			return true
		}
	}
	return false
}
