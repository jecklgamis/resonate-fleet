package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// capturingWorker accepts a task and records the scenario it was sent.
func capturingWorker(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /task", func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 20)
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("no scenario file: %v", err)
		} else {
			b, _ := io.ReadAll(f)
			mu.Lock()
			got = string(b)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"taskId":"t1"}`))
	})
	mux.HandleFunc("GET /task/t1", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"done"}`))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s, func() string { mu.Lock(); defer mu.Unlock(); return got }
}

func postScenario(h http.Handler, query, scenario string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	p, _ := mw.CreateFormFile("file", "s.yaml")
	p.Write([]byte(scenario))
	mw.Close()
	return call(h, "POST", "/runs"+query, mw.FormDataContentType(), &buf)
}

func TestTotalRateGivesEachWorkerItsOwnSlice(t *testing.T) {
	_, h := newController(t)
	w1, got1 := capturingWorker(t)
	w2, got2 := capturingWorker(t)
	register(t, h, "w1", w1.URL, "", nil)
	register(t, h, "w2", w2.URL, "", nil)

	rec := postScenario(h, "?total_rate=100", "load:\n  rate: 10\n  workers: 4\n  duration: 5s\nhttp: {}\n")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		RunID     string   `json:"runId"`
		TotalRate float64  `json:"totalRate"`
		Warnings  []string `json:"warnings"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.TotalRate != 100 || len(out.Warnings) != 0 {
		t.Fatalf("response = %s", rec.Body)
	}
	waitRun(t, h, out.RunID)

	l1, l2 := parseLoad(t, []byte(got1())), parseLoad(t, []byte(got2()))
	if l1.Rate != 50 || l2.Rate != 50 {
		t.Fatalf("rates = %v and %v, want 50 each", l1.Rate, l2.Rate)
	}
	if l1.Workers+l2.Workers != 40 {
		t.Fatalf("fleet users = %d, want 40 (4 scaled by 10)", l1.Workers+l2.Workers)
	}
}

func TestWithoutTotalRateEveryWorkerGetsTheScenarioAsIs(t *testing.T) {
	_, h := newController(t)
	w1, got1 := capturingWorker(t)
	w2, got2 := capturingWorker(t)
	register(t, h, "w1", w1.URL, "", nil)
	register(t, h, "w2", w2.URL, "", nil)
	const scn = "load:\n  rate: 10\n  duration: 5s\nhttp: {}\n"
	rec := postScenario(h, "", scn)
	var out struct{ RunID string }
	json.Unmarshal(rec.Body.Bytes(), &out)
	waitRun(t, h, out.RunID)
	if got1() != scn || got2() != scn {
		t.Fatalf("scenario was changed without total_rate:\n%q\n%q", got1(), got2())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("totalRate")) {
		t.Fatal("response should not mention totalRate")
	}
}

func TestTotalRateRejections(t *testing.T) {
	_, h := newController(t)
	w1, _ := capturingWorker(t)
	register(t, h, "w1", w1.URL, "", nil)

	for name, tc := range map[string]struct{ query, scenario string }{
		"not a number": {"?total_rate=fast", "load:\n  rate: 5\n"},
		"zero":         {"?total_rate=0", "load:\n  rate: 5\n"},
		"negative":     {"?total_rate=-5", "load:\n  rate: 5\n"},
		"no rate":      {"?total_rate=10", "load:\n  workers: 5\n  duration: 5s\n"},
	} {
		if rec := postScenario(h, tc.query, tc.scenario); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: = %d %s, want 400", name, rec.Code, rec.Body)
		}
	}

	// A scenario URL can't be rewritten, so it is refused rather than run unsplit.
	rec := call(h, "POST", "/runs?total_rate=10", "application/json", bytes.NewReader([]byte(`{"scenarioUrl":"https://x/y.yaml"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("scenarioUrl with total_rate = %d %s, want 400", rec.Code, rec.Body)
	}
}
