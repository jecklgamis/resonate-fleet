package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// liveView mirrors the controller's GET /runs/{id}/live.
type liveView struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Workers   []struct {
		Name     string  `json:"name"`
		Status   string  `json:"status"`
		Requests int64   `json:"requests"`
		Errors   int64   `json:"errors"`
		RPS      float64 `json:"rps"`
		P95Ms    float64 `json:"p95_ms"`
		Note     string  `json:"note"`
	} `json:"workers"`
	Reporting int     `json:"reporting"`
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	ErrorRate float64 `json:"error_rate"`
	RPS       float64 `json:"rps"`
	Latency   struct {
		P50 float64 `json:"p50"`
		P95 float64 `json:"p95"`
		P99 float64 `json:"p99"`
		Max float64 `json:"max"`
	} `json:"latency_ms"`
}

// permanentError is a live-view failure that retrying won't fix: the run doesn't
// exist, or the controller has no live view.
type permanentError struct{ error }

// Live fetches what a run is doing right now.
func (c *ControllerClient) Live(ctx context.Context, id string) (liveView, error) {
	var v liveView
	resp, err := c.do(ctx, http.MethodGet, "/runs/"+id+"/live", "", nil)
	if err != nil {
		return v, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusNotFound && strings.Contains(string(b), "no such run"):
		return v, permanentError{fmt.Errorf("no such run %q", id)}
	case resp.StatusCode == http.StatusNotFound:
		return v, permanentError{fmt.Errorf("this controller has no live view; upgrade it")}
	case resp.StatusCode != http.StatusOK:
		return v, fmt.Errorf("live view: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, fmt.Errorf("parsing live view: %w", err)
	}
	return v, nil
}

// fmtMs renders milliseconds as a short duration like 4.8ms.
func fmtMs(ms float64) string {
	return time.Duration(ms * float64(time.Millisecond)).Round(10 * time.Microsecond).String()
}

// liveLine is the whole fleet on one line.
func liveLine(v liveView) string {
	elapsed := time.Duration(v.ElapsedMs * int64(time.Millisecond)).Round(time.Second)
	if v.Reporting == 0 {
		return fmt.Sprintf("[%s] waiting for the workers to start...", elapsed)
	}
	return fmt.Sprintf("[%s] %d/%d workers  %.1f req/s  %d req  err %.2f%%  p50 %s  p95 %s  p99 %s",
		elapsed, v.Reporting, len(v.Workers), v.RPS, v.Requests, v.ErrorRate*100,
		fmtMs(v.Latency.P50), fmtMs(v.Latency.P95), fmtMs(v.Latency.P99))
}

// liveTable is a header, one row per worker, and a FLEET row, as lines.
func liveTable(v liveView) []string {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "run %s  %s  %s\n", v.ID, v.Status, time.Duration(v.ElapsedMs*int64(time.Millisecond)).Round(time.Second))
	fmt.Fprintln(tw, "WORKER\tSTATUS\tREQUESTS\tERRORS\tREQ/S\tP95\tNOTE")
	for _, w := range v.Workers {
		if w.Note != "" {
			fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t-\t%s\n", w.Name, w.Status, w.Note)
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%.1f\t%s\t\n", w.Name, w.Status, w.Requests, w.Errors, w.RPS, fmtMs(w.P95Ms))
	}
	fmt.Fprintf(tw, "FLEET\t\t%d\t%d\t%.1f\t%s\tp50 %s  p99 %s  max %s\n", v.Requests, v.Errors, v.RPS,
		fmtMs(v.Latency.P95), fmtMs(v.Latency.P50), fmtMs(v.Latency.P99), fmtMs(v.Latency.Max))
	tw.Flush()
	return strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
}

// isTerminal reports whether f is an interactive terminal.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// liveDisplay draws the live view. On a terminal it redraws in place; otherwise it
// appends a plain line or table each tick, less often so a log stays readable.
type liveDisplay struct {
	out   io.Writer
	tty   bool
	every time.Duration
	table bool // a per-worker table, not just the one-line summary

	drawn int // lines the last frame took, to be erased by the next
}

func newLiveDisplay(out *os.File, table bool) *liveDisplay {
	d := &liveDisplay{out: out, tty: isTerminal(out), table: table, every: time.Second}
	if !d.tty {
		d.every = 5 * time.Second
	}
	return d
}

// frame draws one view.
func (d *liveDisplay) frame(v liveView) {
	lines := []string{liveLine(v)}
	if d.table {
		lines = liveTable(v)
	}
	if d.tty {
		d.erase()
		for _, l := range lines {
			fmt.Fprintf(d.out, "\x1b[K%s\n", l)
		}
		d.drawn = len(lines)
		return
	}
	fmt.Fprintln(d.out, strings.Join(lines, "\n"))
}

// erase clears the previous frame (terminal only).
func (d *liveDisplay) erase() {
	if d.tty && d.drawn > 0 {
		fmt.Fprintf(d.out, "\x1b[%dA\x1b[J", d.drawn)
		d.drawn = 0
	}
}

// follow draws the run every tick until ctx is done or the run is over, and
// returns the last view it saw. A poll that fails for a passing reason (the network)
// is skipped and tried again: the view is a convenience, and the run's own result is
// what counts. One that can't succeed (no such run, no live view) ends it.
func (d *liveDisplay) follow(ctx context.Context, c *ControllerClient, id string) (last liveView, err error) {
	tick := time.NewTicker(d.every)
	defer tick.Stop()
	for {
		v, e := c.Live(ctx, id)
		switch {
		case e == nil:
			last, err = v, nil
			d.frame(v)
			if v.Status != "running" {
				return last, nil
			}
		case errors.As(e, new(permanentError)):
			return last, e // retrying won't help
		case last.ID == "":
			err = e // never got a view: let the caller decide whether that matters
		}
		select {
		case <-ctx.Done():
			return last, err
		case <-tick.C:
		}
	}
}
