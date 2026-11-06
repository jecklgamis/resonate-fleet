package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/jecklgamis/resonate-fleet/internal/hist"
)

// liveTimeout caps how long one worker may take to answer a live poll, so a stuck
// worker can't stall the view of the others.
const liveTimeout = 3 * time.Second

// workerLive mirrors a worker's GET /task/{id}/live.
type workerLive struct {
	Status    string  `json:"status"`
	ElapsedMs int64   `json:"elapsed_ms"`
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	RPS       float64 `json:"rps"`
	Latency   struct {
		P95 float64 `json:"p95"`
		Max float64 `json:"max"`
	} `json:"latency_ms"`
	Buckets hist.Counts `json:"buckets"`
}

// LiveWorker is one worker's row of a live view.
type LiveWorker struct {
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`
	RPS      float64 `json:"rps"`
	P95Ms    float64 `json:"p95_ms"`
	// Note says why a worker has no numbers: it has no task yet, is unreachable, or
	// predates live stats.
	Note string `json:"note,omitempty"`
}

// LiveLatency is latency in milliseconds across the fleet.
type LiveLatency struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

// LiveView is GET /runs/{id}/live: what a run is doing right now, over every
// worker. Requests and errors are the whole run so far. The rate and latency
// percentiles cover the last few seconds while the run is going (the totals once it
// is over). The percentiles come from the workers' summed histograms, so they are
// the fleet's own, not an average of the workers'.
type LiveView struct {
	ID        string       `json:"id"`
	Status    string       `json:"status"`
	ElapsedMs int64        `json:"elapsed_ms"`
	Workers   []LiveWorker `json:"workers"`
	Reporting int          `json:"reporting"` // workers whose numbers are in the totals
	Requests  int64        `json:"requests"`
	Errors    int64        `json:"errors"`
	ErrorRate float64      `json:"error_rate"`
	RPS       float64      `json:"rps"`
	Latency   LiveLatency  `json:"latency_ms"`
}

// Live polls every worker of the run at once and combines what they report.
func (rs *Runs) Live(ctx context.Context, run *Run) LiveView {
	snap := run.snapshot()
	view := LiveView{ID: snap.ID, Status: snap.Status, Workers: make([]LiveWorker, len(snap.Workers))}
	got := make([]*workerLive, len(snap.Workers))

	var wg sync.WaitGroup
	for i, w := range snap.Workers {
		view.Workers[i] = LiveWorker{Name: w.Name, Status: w.Status}
		if w.TaskID == "" {
			view.Workers[i].Note = "no task yet"
			if w.Error != "" {
				view.Workers[i].Note = w.Error
			}
			continue
		}
		wg.Add(1)
		go func(i int, w *WorkerRun) {
			defer wg.Done()
			wl, err := rs.workerLive(ctx, w)
			if err != nil {
				view.Workers[i].Note = err.Error()
				return
			}
			got[i] = wl
		}(i, w)
	}
	wg.Wait()

	merged := hist.Counts{}
	for i, wl := range got {
		if wl == nil {
			continue
		}
		row := &view.Workers[i]
		row.Requests, row.Errors, row.RPS, row.P95Ms = wl.Requests, wl.Errors, wl.RPS, wl.Latency.P95
		view.Reporting++
		view.Requests += wl.Requests
		view.Errors += wl.Errors
		view.RPS += wl.RPS
		merged.Merge(wl.Buckets)
		if wl.Latency.Max > view.Latency.Max {
			view.Latency.Max = wl.Latency.Max
		}
		if wl.ElapsedMs > view.ElapsedMs {
			view.ElapsedMs = wl.ElapsedMs
		}
	}
	view.Latency.P50, view.Latency.P95, view.Latency.P99 = merged.Percentile(50), merged.Percentile(95), merged.Percentile(99)
	if view.Requests > 0 {
		view.ErrorRate = float64(view.Errors) / float64(view.Requests)
	}
	sort.SliceStable(view.Workers, func(i, j int) bool { return view.Workers[i].Name < view.Workers[j].Name })
	return view
}

func (rs *Runs) workerLive(ctx context.Context, w *WorkerRun) (*workerLive, error) {
	ctx, cancel := context.WithTimeout(ctx, liveTimeout)
	defer cancel()
	resp, err := rs.do(ctx, w, http.MethodGet, "/task/"+w.TaskID+"/live", "", nil)
	if err != nil {
		return nil, fmt.Errorf("unreachable")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("no live stats (an older worker)")
	case resp.StatusCode != http.StatusOK:
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("live stats: %s", resp.Status)
	}
	var wl workerLive
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&wl); err != nil {
		return nil, fmt.Errorf("live stats: unreadable")
	}
	return &wl, nil
}
