package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// startAtWorker records the startAt it was submitted with and echoes it back like a
// current worker does (now is the worker's clock, offset by skew).
func startAtWorker(t *testing.T, skew time.Duration, echo bool) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /task", func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 20)
		mu.Lock()
		got = r.URL.Query().Get("startAt")
		mu.Unlock()
		resp := map[string]string{"taskId": "t1"}
		if echo && got != "" {
			resp["startAt"] = got
			resp["now"] = time.Now().Add(skew).UTC().Format(time.RFC3339Nano)
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /task/t1", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"done"}`))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s, func() string { mu.Lock(); defer mu.Unlock(); return got }
}

func TestStartDelayGivesEveryWorkerTheSameStartTime(t *testing.T) {
	_, h := newController(t)
	w1, got1 := startAtWorker(t, 0, true)
	w2, got2 := startAtWorker(t, 0, true)
	register(t, h, "w1", w1.URL, "", nil)
	register(t, h, "w2", w2.URL, "", nil)

	before := time.Now()
	rec := postScenario(h, "?start_delay=3s", "load:\n  rate: 5\n")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		RunID   string `json:"runId"`
		StartAt string `json:"startAt"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	s := waitRun(t, h, out.RunID) // by now both workers have been submitted to
	if got1() == "" || got1() != got2() || got1() != out.StartAt {
		t.Fatalf("startAt: response=%q w1=%q w2=%q, want all equal", out.StartAt, got1(), got2())
	}
	at, err := time.Parse(time.RFC3339Nano, out.StartAt)
	if err != nil || at.Sub(before) < 2900*time.Millisecond || at.Sub(before) > 4*time.Second {
		t.Fatalf("startAt = %v (err %v), want ~3s after %v", at, err, before)
	}
	for _, w := range s.Workers {
		if w.Warning != "" {
			t.Errorf("%s: unexpected warning %q", w.Name, w.Warning)
		}
	}
}

func TestSingleWorkerAndNoDelayGetNoStartAt(t *testing.T) {
	_, h := newController(t)
	w1, got1 := startAtWorker(t, 0, true)
	register(t, h, "w1", w1.URL, "", nil)
	// one worker has nobody to line up with
	rec := postScenario(h, "?start_delay=3s", "load:\n  rate: 5\n")
	var out struct {
		RunID   string `json:"runId"`
		StartAt string `json:"startAt"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	waitRun(t, h, out.RunID)
	if out.StartAt != "" || got1() != "" {
		t.Fatalf("one worker was given a start time: response %q, worker %q", out.StartAt, got1())
	}

	w2, got2 := startAtWorker(t, 0, true)
	register(t, h, "w2", w2.URL, "", nil)
	rec = postScenario(h, "", "load:\n  rate: 5\n") // no start_delay at all
	json.Unmarshal(rec.Body.Bytes(), &out)
	waitRun(t, h, out.RunID)
	if got1() != "" || got2() != "" || strings.Contains(rec.Body.String(), "startAt") {
		t.Fatal("a run without start_delay was given a start time")
	}
}

func TestBadStartDelay(t *testing.T) {
	_, h := newController(t)
	w1, _ := startAtWorker(t, 0, true)
	register(t, h, "w1", w1.URL, "", nil)
	for _, q := range []string{"?start_delay=soon", "?start_delay=-5s", "?start_delay=1h"} {
		if rec := postScenario(h, q, "load:\n  rate: 5\n"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %s, want 400", q, rec.Code, rec.Body)
		}
	}
}

func TestOldWorkerThatIgnoresStartAtIsFlagged(t *testing.T) {
	_, h := newController(t)
	oldW, _ := startAtWorker(t, 0, false) // never echoes startAt
	newW, _ := startAtWorker(t, 0, true)
	register(t, h, "old", oldW.URL, "", nil)
	register(t, h, "new", newW.URL, "", nil)
	rec := postScenario(h, "?start_delay=2s", "load:\n  rate: 5\n")
	var out struct{ RunID string }
	json.Unmarshal(rec.Body.Bytes(), &out)
	s := waitRun(t, h, out.RunID)
	for _, w := range s.Workers {
		switch w.Name {
		case "old":
			if !strings.Contains(w.Warning, "predates synchronized start") {
				t.Errorf("old worker warning = %q", w.Warning)
			}
		case "new":
			if w.Warning != "" {
				t.Errorf("new worker warning = %q", w.Warning)
			}
		}
	}
}

func TestStartWarning(t *testing.T) {
	now := time.Now()
	startAt := now.Add(5 * time.Second)
	at := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339Nano) }
	echo := startAt.UTC().Format(time.RFC3339Nano)

	for name, tc := range map[string]struct {
		got        time.Time
		echoed, nw string
		want       string // substring of the warning, "" for none
	}{
		"fine":            {now.Add(50 * time.Millisecond), echo, at(40 * time.Millisecond), ""},
		"old worker":      {now, "", "", "predates"},
		"delivered late":  {startAt.Add(2 * time.Second), echo, at(7 * time.Second), "started late"},
		"clock ahead":     {now, echo, at(3 * time.Second), "ahead of"},
		"clock behind":    {now, echo, at(-2 * time.Second), "behind"},
		"small skew ok":   {now, echo, at(-200 * time.Millisecond), ""},
		"no worker clock": {now, echo, "", ""},
	} {
		got := startWarning("w", startAt, tc.got, tc.echoed, tc.nw)
		if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: warning = %q, want it to contain %q", name, got, tc.want)
		}
	}
}
