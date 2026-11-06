package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jecklgamis/resonate-fleet/internal/hist"
)

// maxPoints caps how many points a chart gets; a longer run is shown in steps of
// several seconds so the report stays small.
const maxPoints = 600

// fleetSeries is the whole fleet's traffic over time, built from every worker's
// results.jsonl by request timestamp, so it is the fleet's own series, not a sum of
// the workers' summaries.
type fleetSeries struct {
	Start   time.Time // the first request of any worker
	Step    int       // seconds each point covers
	Points  []seriesPoint
	Workers []workerSeries
}

// seriesPoint is one step of the run. Latencies are milliseconds, read from a
// histogram with 5%-wide buckets (see package hist).
type seriesPoint struct {
	T          float64 // seconds from Start to the point's beginning
	RPS        float64
	ErrorsPerS float64
	P50, P95   float64
	P99        float64
}

// workerSeries is one worker's request rate over the same points.
type workerSeries struct {
	Name   string
	Offset time.Duration // how long after the fleet's first request this worker's began
	RPS    []float64
}

type tsLine struct {
	Timestamp  time.Time `json:"timestamp"`
	LatencyMs  float64   `json:"latency_ms"`
	StatusCode int       `json:"status_code"`
	Error      string    `json:"error"`
}

// secBucket is one worker-second of traffic.
type secBucket struct {
	req, errs int
	h         hist.Counts
}

// buildSeries reads each finished worker's results.jsonl in outDir/<worker>/. It
// returns nil when no request has a timestamp. Lines that aren't JSON, or have no
// timestamp, are skipped.
func buildSeries(outDir string, results []workerResult) *fleetSeries {
	type perWorker struct {
		name  string
		first time.Time
		secs  map[int64]*secBucket
	}
	var workers []*perWorker
	var start time.Time
	for _, r := range results {
		pw := &perWorker{name: r.Name, secs: map[int64]*secBucket{}}
		f, err := os.Open(filepath.Join(outDir, r.Name, "results.jsonl"))
		if err != nil {
			continue
		}
		br := bufio.NewReaderSize(f, 1<<20)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				var l tsLine
				if json.Unmarshal(line, &l) == nil && !l.Timestamp.IsZero() {
					sec := l.Timestamp.Unix()
					b := pw.secs[sec]
					if b == nil {
						b = &secBucket{h: hist.Counts{}}
						pw.secs[sec] = b
					}
					b.req++
					b.h.Add(l.LatencyMs)
					if l.Error != "" || l.StatusCode >= 400 {
						b.errs++
					}
					if pw.first.IsZero() || l.Timestamp.Before(pw.first) {
						pw.first = l.Timestamp
					}
				}
			}
			if err != nil {
				break
			}
		}
		f.Close()
		if len(pw.secs) > 0 {
			workers = append(workers, pw)
			if start.IsZero() || pw.first.Before(start) {
				start = pw.first
			}
		}
	}
	if len(workers) == 0 {
		return nil
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].name < workers[j].name })

	first, last := start.Unix(), start.Unix()
	for _, w := range workers {
		for s := range w.secs {
			if s > last {
				last = s
			}
		}
	}
	span := int(last-first) + 1
	step := (span + maxPoints - 1) / maxPoints
	if step < 1 {
		step = 1
	}
	n := (span + step - 1) / step

	fs := &fleetSeries{Start: start, Step: step, Points: make([]seriesPoint, n)}
	fleetH := make([]hist.Counts, n)
	fleetReq := make([]int, n)
	fleetErr := make([]int, n)
	for _, w := range workers {
		ws := workerSeries{Name: w.name, Offset: w.first.Sub(start), RPS: make([]float64, n)}
		counts := make([]int, n)
		for sec, b := range w.secs {
			i := int(sec-first) / step
			counts[i] += b.req
			fleetReq[i] += b.req
			fleetErr[i] += b.errs
			if fleetH[i] == nil {
				fleetH[i] = hist.Counts{}
			}
			fleetH[i].Merge(b.h)
		}
		for i, c := range counts {
			ws.RPS[i] = float64(c) / float64(step)
		}
		fs.Workers = append(fs.Workers, ws)
	}
	for i := range fs.Points {
		p := &fs.Points[i]
		p.T = float64(i * step)
		p.RPS = float64(fleetReq[i]) / float64(step)
		p.ErrorsPerS = float64(fleetErr[i]) / float64(step)
		if fleetH[i] != nil {
			p.P50, p.P95, p.P99 = fleetH[i].Percentile(50), fleetH[i].Percentile(95), fleetH[i].Percentile(99)
		}
	}
	return fs
}
