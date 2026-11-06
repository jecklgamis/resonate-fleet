package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func scenarioFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.yaml")
	os.WriteFile(p, []byte("load:\n  rate: 10\n"), 0o644)
	return p
}

func TestStartSendsTotalRateAndReturnsWarnings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("total_rate"); got != "250.5" {
			t.Errorf("total_rate = %q, want 250.5", got)
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"runId":"r1","workers":["a"],"available":1,"totalRate":250.5,"warnings":["load.workers: 1 across 3 workers"]}`))
	}))
	defer srv.Close()
	run, err := newControllerClient(srv.URL, "").Start(t.Context(), scenarioFile(t), startOptions{TotalRate: 250.5})
	if err != nil {
		t.Fatal(err)
	}
	if run.TotalRateIgnored || len(run.Warnings) != 1 {
		t.Fatalf("run = %+v", run)
	}
}

func TestNoTotalRateLeavesTheParameterOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("total_rate") {
			t.Error("total_rate sent when not asked for")
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"runId":"r1","workers":["a"],"available":1}`))
	}))
	defer srv.Close()
	run, err := newControllerClient(srv.URL, "").Start(t.Context(), scenarioFile(t), startOptions{})
	if err != nil || run.TotalRateIgnored {
		t.Fatalf("run=%+v err=%v", run, err)
	}
}

func TestTotalRateWithAScenarioURLIsRefusedLocally(t *testing.T) {
	_, err := newControllerClient("http://unused", "").Start(t.Context(), "https://x/y.yaml", startOptions{TotalRate: 10})
	if err == nil || !strings.Contains(err.Error(), "scenario file") {
		t.Fatalf("err = %v", err)
	}
}

// An older controller ignores total_rate and starts every worker on the full,
// unsplit load. The client must stop that run, not report on it.
func TestOlderControllerIgnoringTotalRateAbortsTheRun(t *testing.T) {
	var aborted atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"runId":"r1","workers":["a"],"available":1}`)) // no totalRate echo
	})
	mux.HandleFunc("DELETE /runs/r1", func(w http.ResponseWriter, r *http.Request) {
		aborted.Add(1)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"aborted":["a"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := runViaController(t.Context(), newControllerClient(srv.URL, ""), scenarioFile(t),
		startOptions{TotalRate: 100}, t.TempDir(), time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "does not support --total-rate") {
		t.Fatalf("err = %v", err)
	}
	if aborted.Load() != 1 {
		t.Fatalf("abort calls = %d, want 1", aborted.Load())
	}
}

func TestStartSendsStartDelayAndReadsStartAt(t *testing.T) {
	at := time.Now().Add(5 * time.Second).UTC()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("start_delay"); got != "5s" {
			t.Errorf("start_delay = %q, want 5s", got)
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"runId":"r1","workers":["a","b"],"available":2,"startAt":"` + at.Format(time.RFC3339Nano) + `"}`))
	}))
	defer srv.Close()
	run, err := newControllerClient(srv.URL, "").Start(t.Context(), scenarioFile(t), startOptions{StartDelay: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !run.StartAt.Equal(at) || run.StartIgnored {
		t.Fatalf("run = %+v", run)
	}
}

func TestOlderControllerIgnoringStartDelayIsFlaggedNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"runId":"r1","workers":["a","b"],"available":2}`))
	}))
	defer srv.Close()
	run, err := newControllerClient(srv.URL, "").Start(t.Context(), scenarioFile(t), startOptions{StartDelay: 5 * time.Second})
	if err != nil || !run.StartIgnored {
		t.Fatalf("run=%+v err=%v, want StartIgnored", run, err)
	}
	// One worker is never synchronized, so that is not a warning.
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"runId":"r1","workers":["a"],"available":1}`))
	}))
	defer srv1.Close()
	run, _ = newControllerClient(srv1.URL, "").Start(t.Context(), scenarioFile(t), startOptions{StartDelay: 5 * time.Second})
	if run.StartIgnored {
		t.Fatal("a single worker needs no start time")
	}
}
