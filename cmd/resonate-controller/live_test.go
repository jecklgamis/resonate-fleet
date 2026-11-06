package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jecklgamis/resonate-fleet/internal/hist"
)

// liveWorker serves a fixed live body for task t1 (or the given status for it).
func liveBodyWorker(t *testing.T, liveStatus int, liveBody string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /task", func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 20)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"taskId":"t1"}`))
	})
	mux.HandleFunc("GET /task/t1", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"running"}`))
	})
	mux.HandleFunc("GET /task/t1/live", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(liveStatus)
		w.Write([]byte(liveBody))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func liveJSON(requests, errors int64, rps float64, counts hist.Counts, maxMs float64) string {
	b, _ := json.Marshal(map[string]any{
		"status": "running", "elapsed_ms": 4000, "requests": requests, "errors": errors, "rps": rps,
		"latency_ms": map[string]float64{"p95": counts.Percentile(95), "max": maxMs}, "buckets": counts,
	})
	return string(b)
}

func getLiveView(t *testing.T, h http.Handler, id string) LiveView {
	t.Helper()
	rec := call(h, "GET", "/runs/"+id+"/live", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("live = %d %s", rec.Code, rec.Body)
	}
	var v LiveView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func waitForTasks(t *testing.T, h http.Handler, id string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		var s runSnapshot
		json.Unmarshal(call(h, "GET", "/runs/"+id, "", nil).Body.Bytes(), &s)
		ready := true
		for _, w := range s.Workers {
			ready = ready && w.TaskID != ""
		}
		if ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("workers never got a task")
}

func TestLiveMergesWorkersIntoFleetPercentiles(t *testing.T) {
	_, h := newController(t)
	// One fast worker (10ms) and one slow (1000ms), 100 requests each. Averaging
	// their p95s would say ~505ms; over all 200 requests it is the slow worker's.
	fast, slow := hist.Counts{}, hist.Counts{}
	for i := 0; i < 100; i++ {
		fast.Add(10)
		slow.Add(1000)
	}
	register(t, h, "fast", liveBodyWorker(t, 200, liveJSON(100, 0, 25, fast, 12)).URL, "", nil)
	register(t, h, "slow", liveBodyWorker(t, 200, liveJSON(100, 10, 15, slow, 1100)).URL, "", nil)
	id := startRun(t, h, "")
	waitForTasks(t, h, id)

	v := getLiveView(t, h, id)
	if v.Reporting != 2 || v.Requests != 200 || v.Errors != 10 || v.RPS != 40 {
		t.Fatalf("view = %+v", v)
	}
	if v.ErrorRate != 0.05 {
		t.Fatalf("error rate = %v", v.ErrorRate)
	}
	if v.Latency.P50 < 10 || v.Latency.P50 > 10.6 {
		t.Fatalf("p50 = %v, want the fast worker's ~10ms (100th of 200)", v.Latency.P50)
	}
	if v.Latency.P95 < 1000 || v.Latency.P95 > 1060 || v.Latency.Max != 1100 {
		t.Fatalf("p95=%v max=%v, want ~1000 and 1100", v.Latency.P95, v.Latency.Max)
	}
	if len(v.Workers) != 2 || v.Workers[0].Name != "fast" || v.Workers[1].Name != "slow" || v.Workers[1].Errors != 10 {
		t.Fatalf("workers = %+v", v.Workers)
	}
	if v.ElapsedMs != 4000 {
		t.Fatalf("elapsed = %d", v.ElapsedMs)
	}
}

func TestLiveSurvivesAnOldOrBrokenWorker(t *testing.T) {
	_, h := newController(t)
	ok := hist.Counts{}
	ok.Add(20)
	register(t, h, "good", liveBodyWorker(t, 200, liveJSON(7, 0, 3, ok, 20)).URL, "", nil)
	register(t, h, "old", liveBodyWorker(t, 404, `{"error":"no"}`).URL, "", nil)
	register(t, h, "broken", liveBodyWorker(t, 200, `not json`).URL, "", nil)
	register(t, h, "failing", liveBodyWorker(t, 500, `x`).URL, "", nil)
	id := startRun(t, h, "")
	waitForTasks(t, h, id)

	v := getLiveView(t, h, id)
	if v.Reporting != 1 || v.Requests != 7 {
		t.Fatalf("only the good worker should count: %+v", v)
	}
	notes := map[string]string{}
	for _, w := range v.Workers {
		notes[w.Name] = w.Note
	}
	for name, want := range map[string]string{"good": "", "old": "older worker", "broken": "unreadable", "failing": "500"} {
		if want == "" && notes[name] != "" || want != "" && !strings.Contains(notes[name], want) {
			t.Errorf("%s note = %q, want it to mention %q", name, notes[name], want)
		}
	}
}

func TestLiveUnreachableWorkerAndUnknownRun(t *testing.T) {
	_, h := newController(t)
	dead := liveBodyWorker(t, 200, "{}")
	register(t, h, "dead", dead.URL, "", nil)
	id := startRun(t, h, "")
	waitForTasks(t, h, id)
	dead.Close()
	v := getLiveView(t, h, id)
	if v.Reporting != 0 || v.Workers[0].Note != "unreachable" {
		t.Fatalf("view = %+v", v)
	}
	if rec := call(h, "GET", "/runs/nope/live", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run = %d", rec.Code)
	}
}
