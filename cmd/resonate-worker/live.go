package main

import (
	"sync"
	"time"

	"github.com/jecklgamis/resonate-fleet/internal/hist"
)

// liveWindow is how many seconds of recent traffic the live rate and latency
// percentiles cover. Short enough to show a problem as it starts, long enough to be
// steady.
const liveWindow = 10

// Live tallies a running task's requests as they are tailed from results.jsonl: a
// rolling window for "what is it doing now", and whole-run totals for what it did.
// It is safe for concurrent use.
type Live struct {
	mu       sync.Mutex
	start    time.Time
	end      time.Time // zero while running
	requests int64
	errors   int64
	maxMs    float64
	whole    hist.Counts // every latency of the run
	secs     [liveWindow]liveSecond
}

// liveSecond is one second's slice of the window; it is reused when the ring wraps.
type liveSecond struct {
	sec      int64 // unix second this slot holds, 0 if empty
	requests int64
	errors   int64
	maxMs    float64
	counts   hist.Counts
}

// NewLive starts a tally at now.
func NewLive(now time.Time) *Live {
	return &Live{start: now, whole: hist.Counts{}}
}

// Record counts one finished request seen at now.
func (l *Live) Record(r resultLine, now time.Time) {
	failed := r.Error != "" || r.StatusCode >= 400
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests++
	l.whole.Add(r.LatencyMs)
	if failed {
		l.errors++
	}
	if r.LatencyMs > l.maxMs {
		l.maxMs = r.LatencyMs
	}
	sec := now.Unix()
	s := &l.secs[sec%liveWindow]
	if s.sec != sec {
		*s = liveSecond{sec: sec, counts: hist.Counts{}}
	}
	s.requests++
	s.counts.Add(r.LatencyMs)
	if failed {
		s.errors++
	}
	if r.LatencyMs > s.maxMs {
		s.maxMs = r.LatencyMs
	}
}

// Finish freezes the tally: from now on the stats are the whole run's.
func (l *Live) Finish(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.end = now
}

// LiveLatency is latency in milliseconds.
type LiveLatency struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

// LiveStats is GET /task/{id}/live. While the task runs, rps and latency describe
// the last few seconds (WindowS); once it has finished they describe the whole run.
// Buckets is the latency histogram those percentiles were read from (see package
// hist), so a caller can add several workers' histograms and read fleet percentiles.
type LiveStats struct {
	Status    Status      `json:"status"`
	ElapsedMs int64       `json:"elapsed_ms"`
	Requests  int64       `json:"requests"` // the whole run so far
	Errors    int64       `json:"errors"`   // likewise
	RPS       float64     `json:"rps"`
	WindowS   int         `json:"window_s,omitempty"` // seconds rps and latency cover; 0 for the whole run
	Latency   LiveLatency `json:"latency_ms"`
	Buckets   hist.Counts `json:"buckets,omitempty"`
}

// Snapshot is the stats at now. A nil Live (a task that has not started) is all zeros.
func (l *Live) Snapshot(now time.Time) LiveStats {
	if l == nil {
		return LiveStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := LiveStats{Requests: l.requests, Errors: l.errors}
	if !l.end.IsZero() {
		st.ElapsedMs = l.end.Sub(l.start).Milliseconds()
		if secs := l.end.Sub(l.start).Seconds(); secs > 0 {
			st.RPS = float64(l.requests) / secs
		}
		st.Buckets = l.whole.Clone()
		st.Latency = latencyOf(st.Buckets, l.maxMs)
		return st
	}
	st.ElapsedMs = now.Sub(l.start).Milliseconds()
	st.WindowS = liveWindow
	window := hist.Counts{}
	var n int64
	var maxMs float64
	for _, s := range l.secs {
		// Slots older than the window are stale leftovers of the ring.
		if s.sec == 0 || now.Unix()-s.sec >= liveWindow || s.sec > now.Unix() {
			continue
		}
		n += s.requests
		window.Merge(s.counts)
		if s.maxMs > maxMs {
			maxMs = s.maxMs
		}
	}
	// Until a full window has passed, divide by the time there has been.
	span := now.Sub(l.start).Seconds()
	if span > liveWindow {
		span = liveWindow
	}
	if span < 1 {
		span = 1
	}
	st.RPS = float64(n) / span
	st.Buckets = window
	st.Latency = latencyOf(window, maxMs)
	return st
}

func latencyOf(c hist.Counts, maxMs float64) LiveLatency {
	return LiveLatency{P50: c.Percentile(50), P95: c.Percentile(95), P99: c.Percentile(99), Max: maxMs}
}
