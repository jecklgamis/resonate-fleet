package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func res(ms float64, code int) resultLine { return resultLine{LatencyMs: ms, StatusCode: code} }

func TestLiveWindowRateErrorsAndPercentiles(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	l := NewLive(t0)
	// 5 seconds of 10 req/s: nine fast OK requests and one slow 500 each second.
	for s := 0; s < 5; s++ {
		now := t0.Add(time.Duration(s) * time.Second)
		for i := 0; i < 9; i++ {
			l.Record(res(10, 200), now)
		}
		l.Record(res(500, 500), now)
	}
	st := l.Snapshot(t0.Add(5 * time.Second))
	if st.Requests != 50 || st.Errors != 5 {
		t.Fatalf("requests=%d errors=%d, want 50 and 5", st.Requests, st.Errors)
	}
	if st.RPS < 9.9 || st.RPS > 10.1 { // 50 requests over the 5s there has been
		t.Fatalf("rps = %v, want ~10", st.RPS)
	}
	if st.WindowS != liveWindow {
		t.Fatalf("window = %d", st.WindowS)
	}
	// p50 is the fast ones (~10ms), p99 the slow ones (~500ms), each within a bucket.
	if st.Latency.P50 < 10 || st.Latency.P50 > 10.6 || st.Latency.P99 < 500 || st.Latency.P99 > 530 || st.Latency.Max != 500 {
		t.Fatalf("latency = %+v", st.Latency)
	}
	if st.Buckets.Total() != 50 {
		t.Fatalf("buckets hold %d, want 50", st.Buckets.Total())
	}
}

func TestLiveWindowForgetsOldSecondsButNotTheTotals(t *testing.T) {
	t0 := time.Unix(2_000_000, 0)
	l := NewLive(t0)
	for i := 0; i < 20; i++ {
		l.Record(res(5, 200), t0)
	}
	st := l.Snapshot(t0.Add(liveWindow * time.Second)) // the burst is now exactly out of the window
	if st.Requests != 20 {
		t.Fatalf("requests = %d: totals must keep everything", st.Requests)
	}
	if st.RPS != 0 || st.Buckets.Total() != 0 {
		t.Fatalf("rps=%v window=%d, want an empty window", st.RPS, st.Buckets.Total())
	}
	// The ring reuses a slot for a later second without leaking the old counts in.
	l.Record(res(7, 200), t0.Add(liveWindow*time.Second))
	if st := l.Snapshot(t0.Add(liveWindow * time.Second)); st.Buckets.Total() != 1 {
		t.Fatalf("window = %d after the slot was reused, want 1", st.Buckets.Total())
	}
}

func TestLiveFinishedReportsTheWholeRun(t *testing.T) {
	t0 := time.Unix(3_000_000, 0)
	l := NewLive(t0)
	for s := 0; s < 30; s++ {
		for i := 0; i < 4; i++ {
			l.Record(res(float64(1+s), 200), t0.Add(time.Duration(s)*time.Second))
		}
	}
	l.Finish(t0.Add(30 * time.Second))
	st := l.Snapshot(t0.Add(time.Hour))
	if st.Requests != 120 || st.WindowS != 0 || st.ElapsedMs != 30000 {
		t.Fatalf("stats = %+v", st)
	}
	if st.RPS != 4 {
		t.Fatalf("whole-run rps = %v, want 4", st.RPS)
	}
	if st.Latency.Max != 30 || st.Buckets.Total() != 120 {
		t.Fatalf("latency=%+v buckets=%d", st.Latency, st.Buckets.Total())
	}
}

func TestNilLiveIsZero(t *testing.T) {
	if st := (*Live)(nil).Snapshot(time.Now()); st.Requests != 0 || st.RPS != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

// resonate that writes three results, lingers, then exits.
func lingeringResonate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "linger.sh")
	script := `#!/bin/sh
for ms in 10 20 30; do echo '{"latency_ms":'$ms',"status_code":200,"protocol":"http"}' >> results.jsonl; done
echo '{"latency_ms":400,"status_code":503,"protocol":"http"}' >> results.jsonl
sleep 2
echo '{"requests":4}' > report.json
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func getLive(t *testing.T, mux http.Handler, id string) (int, LiveStats) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/task/"+id+"/live", nil))
	var st LiveStats
	json.Unmarshal(rec.Body.Bytes(), &st)
	return rec.Code, st
}

func TestLiveEndpointShowsARunningTaskThenItsTotals(t *testing.T) {
	srv := newTestServer(t, 0)
	srv.Runner.ResonateBin = lingeringResonate(t)
	mux := http.NewServeMux()
	srv.Routes(mux)
	id := submit(t, mux, "protocol: http\n")

	var st LiveStats
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, st = getLive(t, mux, id)
		if st.Requests == 4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st.Status != StatusRunning || st.Requests != 4 || st.Errors != 1 || st.WindowS != liveWindow {
		t.Fatalf("mid-run: %+v, want running with 4 requests, 1 error, windowed", st)
	}

	waitDone(t, mux, id)
	code, st := getLive(t, mux, id)
	if code != http.StatusOK || st.Status != StatusDone || st.Requests != 4 || st.WindowS != 0 || st.Latency.Max != 400 {
		t.Fatalf("after: %d %+v, want done with the whole run's totals", code, st)
	}
}

func TestLiveEndpointUnknownTaskAndQueuedTask(t *testing.T) {
	srv := newTestServer(t, 0)
	mux := http.NewServeMux()
	srv.Routes(mux)
	if code, _ := getLive(t, mux, "nope"); code != http.StatusNotFound {
		t.Fatalf("unknown task = %d, want 404", code)
	}
	// A task still waiting for its start time has no tally yet: zeros, not an error.
	rec := submitAt(mux, "?startAt="+time.Now().Add(20*time.Second).UTC().Format(time.RFC3339Nano))
	var resp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resp)
	code, st := getLive(t, mux, resp["taskId"])
	if code != http.StatusOK || st.Status != StatusQueued || st.Requests != 0 {
		t.Fatalf("queued task: %d %+v", code, st)
	}
	// Cancel it and wait for it to finish: its goroutine writes task.json into the
	// test's temp dir, and returning first races the dir's cleanup.
	del(mux, "/task/"+resp["taskId"])
	task, _ := srv.Store.Get(resp["taskId"])
	for i := 0; i < 500 && task.snapshot().Status == StatusQueued; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if st := task.snapshot().Status; st != StatusCanceled {
		t.Fatalf("status = %s, want canceled", st)
	}
}
