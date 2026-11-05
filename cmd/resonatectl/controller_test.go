package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunViaController(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()
	check := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer ct" {
				http.Error(w, "no", 401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /runs", check(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(1 << 20)
		if got := r.URL.Query()["label"]; len(got) != 1 || got[0] != "region=sgp1" {
			t.Errorf("labels = %v", got)
		}
		if got := r.URL.Query().Get("workers"); got != "25%" {
			t.Errorf("workers = %q, want 25%%", got)
		}
		if _, _, err := r.FormFile("file"); err != nil || len(r.MultipartForm.File["files"]) != 1 {
			t.Errorf("scenario/data not uploaded: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"runId":"r1","workers":["a","b"],"available":8}`))
	}))
	mux.HandleFunc("GET /runs/r1", check(func(w http.ResponseWriter, r *http.Request) {
		st := "running"
		if polls.Add(1) > 1 {
			st = "failed"
		}
		fmt.Fprintf(w, `{"id":"r1","status":%q,"workers":[
			{"name":"a","task_id":"t1","status":"done"},
			{"name":"b","status":"error","error":"unreachable"}]}`, st)
	}))
	mux.HandleFunc("GET /runs/r1/a/{file}", check(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("file") == "report.json" {
			w.Write([]byte(`{"requests":9,"success_rate":1}`))
			return
		}
		w.Write([]byte("x"))
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	scenario := filepath.Join(dir, "s.yaml")
	data := filepath.Join(dir, "users.csv")
	os.WriteFile(scenario, []byte("protocol: http\n"), 0o644)
	os.WriteFile(data, []byte("id\n"), 0o644)
	out := filepath.Join(dir, "out")

	c := newControllerClient(srv.URL+"/", "ct")
	results, err := runViaController(t.Context(), c, scenario, startOptions{DataFiles: []string{data}, Labels: []string{"region=sgp1"}, Workers: "25%"}, out, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Status != "done" || results[0].Report == nil || results[0].Report.Requests != 9 {
		t.Fatalf("results = %+v", results)
	}
	if results[1].Status != "error" || results[1].Err != "unreachable" {
		t.Fatalf("results[1] = %+v", results[1])
	}
	if !anyFailed(results) {
		t.Fatal("an errored worker must fail the run")
	}
	if b, err := os.ReadFile(filepath.Join(out, "a", "report.html")); err != nil || string(b) != "x" {
		t.Fatalf("report.html = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(out, "b")); err == nil {
		t.Fatal("fetched files for a worker with no task")
	}
}

func TestControllerStartRejections(t *testing.T) {
	c := newControllerClient("http://127.0.0.1:1", "")
	if _, err := c.Start(t.Context(), "https://x/s.yaml", startOptions{DataFiles: []string{"f.csv"}}); err == nil || !strings.Contains(err.Error(), "--data") {
		t.Fatalf("URL + --data: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no live workers match"}`, 409)
	}))
	defer srv.Close()
	c = newControllerClient(srv.URL, "")
	_, err := c.Start(t.Context(), "https://x/s.yaml", startOptions{})
	if err == nil || !strings.Contains(err.Error(), "no live workers") {
		t.Fatalf("409: %v", err)
	}
}

func TestWorkersListing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workers" || r.URL.Query().Get("label") != "region=sgp1" || r.Header.Get("Authorization") != "Bearer ct" {
			t.Errorf("bad request: %s auth=%q", r.URL, r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `[{"name":"b","url":"http://10.0.0.2:58080","labels":{"region":"sgp1","az":"a"},"last_seen":"2026-01-01T00:00:00Z"},
			{"name":"a","url":"http://10.0.0.1:58080","last_seen":"2026-01-01T00:00:00Z"}]`)
	}))
	defer srv.Close()
	ws, err := newControllerClient(srv.URL, "ct").Workers(t.Context(), []string{"region=sgp1"})
	if err != nil || len(ws) != 2 {
		t.Fatalf("workers = %v, %v", ws, err)
	}
	var buf strings.Builder
	printWorkers(&buf, ws, time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC))
	out := buf.String()
	if strings.Index(out, "\na ") > strings.Index(out, "\nb ") || !strings.Contains(out, "az=a,region=sgp1") || !strings.Contains(out, "30s ago") {
		t.Fatalf("table:\n%s", out)
	}
}

func TestPrintTableIsSortedByWorkerName(t *testing.T) {
	var buf strings.Builder
	printTable(&buf, []workerResult{
		{Name: "zulu", Status: "done", Report: &reportSummary{Requests: 1, SuccessRate: 1}},
		{Name: "alpha", Status: "failed", Err: "boom"},
	})
	out := buf.String()
	if strings.Index(out, "alpha") > strings.Index(out, "zulu") || !strings.Contains(out, "boom") {
		t.Errorf("table:\n%s", out)
	}
}

func TestStartSendsNoWorkersParamByDefaultAndToleratesOlderControllers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, present := r.URL.Query()["workers"]; present {
			t.Errorf("sent workers=%q although none was asked for", r.URL.Query().Get("workers"))
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"runId":"r9","workers":["a","b","c"]}`)) // no "available": an older controller
	}))
	defer srv.Close()
	got, err := newControllerClient(srv.URL, "").Start(t.Context(), "https://x/s.yaml", startOptions{})
	if err != nil || got.ID != "r9" || len(got.Workers) != 3 || got.Available != 3 || got.SelectionIgnored {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// An older controller ignores ?workers=, so the run goes to every worker. That is
// a much bigger load than asked for, and must be reported rather than hidden.
func TestStartFlagsAControllerThatIgnoredTheSelection(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"runId":"r1","workers":["a","b","c","d"]}`))
	}))
	defer old.Close()
	got, err := newControllerClient(old.URL, "").Start(t.Context(), "https://x/s.yaml", startOptions{Workers: "1"})
	if err != nil || !got.SelectionIgnored || len(got.Workers) != 4 {
		t.Fatalf("got %+v, %v", got, err)
	}
	// A current controller confirms with "available", so nothing is flagged.
	cur := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"runId":"r2","workers":["a"],"available":4}`))
	}))
	defer cur.Close()
	got, err = newControllerClient(cur.URL, "").Start(t.Context(), "https://x/s.yaml", startOptions{Workers: "1"})
	if err != nil || got.SelectionIgnored || got.Available != 4 {
		t.Fatalf("got %+v, %v", got, err)
	}
}
