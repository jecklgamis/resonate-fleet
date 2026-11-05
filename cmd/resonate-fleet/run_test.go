package main

import (
	"bytes"
	"context"
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

// fakeWorker is a minimal stand-in for resonate-worker's HTTP API: it accepts
// one submission, requires the given token (if any), and reports the task as
// "running" for one status check before "done" or "failed" — enough to
// exercise resonate-fleet's submit/poll/fetch loop without a real resonate
// binary or real HTTP wrapper.
func fakeWorker(t *testing.T, token string, finalStatus string, reportJSON string) *httptest.Server {
	t.Helper()
	var checks int32
	mux := http.NewServeMux()
	mux.HandleFunc("/task", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, _, err := r.FormFile("file"); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"taskId":"t1"}`)
	})
	mux.HandleFunc("/task/t1", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n := atomic.AddInt32(&checks, 1)
		status := "running"
		if n >= 2 {
			status = finalStatus
		}
		fmt.Fprintf(w, `{"id":"t1","scenario":"s.yaml","status":%q,"exit_code":%d}`, status, exitCodeFor(finalStatus))
	})
	mux.HandleFunc("/task/t1/report.html", serveString(token, "<html>ok</html>"))
	mux.HandleFunc("/task/t1/report.json", serveString(token, reportJSON))
	mux.HandleFunc("/task/t1/results.jsonl", serveString(token, `{"ok":true}`))
	mux.HandleFunc("/task/t1/log", serveString(token, "fake log"))
	return httptest.NewServer(mux)
}

func exitCodeFor(status string) int {
	if status == "failed" {
		return 1
	}
	return 0
}

func serveString(token, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, body)
	}
}

func writeScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte("protocol: http\nload: {requests: 1, workers: 1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunFleetSucceedsAcrossIndependentWorkers(t *testing.T) {
	a := fakeWorker(t, "", "done", `{"requests":10,"success_rate":1,"requests_per_sec":5,"duration":2000000000,"latencies":{"p95":15000000}}`)
	defer a.Close()
	b := fakeWorker(t, "tok-b", "done", `{"requests":20,"success_rate":0.95,"requests_per_sec":8,"duration":2500000000,"latencies":{"p95":22000000}}`)
	defer b.Close()

	cfg := &FleetConfig{Workers: []Worker{
		{Name: "us-east", URL: a.URL},
		{Name: "eu-west", URL: b.URL, Token: "tok-b"},
	}}

	outDir := t.TempDir()
	results := runFleet(context.Background(), cfg, writeScenario(t), outDir, 10*time.Millisecond)

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	for _, r := range results {
		if r.Status != "done" {
			t.Errorf("worker %s: status = %q, want done (err: %s)", r.Worker.Name, r.Status, r.Err)
		}
		if r.Report == nil {
			t.Errorf("worker %s: no report parsed", r.Worker.Name)
			continue
		}
		for _, name := range []string{"report.html", "report.json", "results.jsonl", "log"} {
			if _, err := os.Stat(filepath.Join(outDir, r.Worker.Name, name)); err != nil {
				t.Errorf("worker %s: %s not written: %v", r.Worker.Name, name, err)
			}
		}
	}
	if anyFailed(results) {
		t.Error("anyFailed = true, want false")
	}
}

func TestRunFleetOneWorkerFailingDoesNotAffectTheOther(t *testing.T) {
	ok := fakeWorker(t, "", "done", `{"requests":5,"success_rate":1,"duration":1000000000,"latencies":{"p95":5000000}}`)
	defer ok.Close()
	bad := fakeWorker(t, "", "failed", `{"requests":5,"success_rate":0.2,"duration":1000000000,"latencies":{"p95":900000000}}`)
	defer bad.Close()

	cfg := &FleetConfig{Workers: []Worker{
		{Name: "healthy", URL: ok.URL},
		{Name: "flaky", URL: bad.URL},
	}}

	results := runFleet(context.Background(), cfg, writeScenario(t), t.TempDir(), 10*time.Millisecond)

	byName := map[string]workerResult{}
	for _, r := range results {
		byName[r.Worker.Name] = r
	}
	if byName["healthy"].Status != "done" {
		t.Errorf("healthy worker status = %q, want done", byName["healthy"].Status)
	}
	if byName["flaky"].Status != "failed" {
		t.Errorf("flaky worker status = %q, want failed", byName["flaky"].Status)
	}
	if !anyFailed(results) {
		t.Error("anyFailed = false, want true (one worker failed)")
	}
}

func TestRunFleetUnreachableWorkerIsReportedNotFatal(t *testing.T) {
	ok := fakeWorker(t, "", "done", `{"requests":5,"success_rate":1,"duration":1000000000,"latencies":{"p95":5000000}}`)
	defer ok.Close()

	cfg := &FleetConfig{Workers: []Worker{
		{Name: "up", URL: ok.URL},
		{Name: "down", URL: "http://127.0.0.1:1"}, // nothing listening
	}}

	results := runFleet(context.Background(), cfg, writeScenario(t), t.TempDir(), 10*time.Millisecond)

	byName := map[string]workerResult{}
	for _, r := range results {
		byName[r.Worker.Name] = r
	}
	if byName["up"].Status != "done" {
		t.Errorf("up worker status = %q, want done", byName["up"].Status)
	}
	if byName["down"].Status != "error" || byName["down"].Err == "" {
		t.Errorf("down worker = %+v, want status=error with a message", byName["down"])
	}
	if !anyFailed(results) {
		t.Error("anyFailed = false, want true (one worker unreachable)")
	}
}

func TestPrintTableIsSortedByWorkerName(t *testing.T) {
	results := []workerResult{
		{Worker: Worker{Name: "zulu"}, Status: "done", Report: &reportSummary{Requests: 1, SuccessRate: 1}},
		{Worker: Worker{Name: "alpha"}, Status: "failed", Err: "boom"},
	}
	var buf bytes.Buffer
	printTable(&buf, results)
	out := buf.String()
	if strings.Index(out, "alpha") > strings.Index(out, "zulu") {
		t.Errorf("expected alpha before zulu in table:\n%s", out)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("expected error message in table:\n%s", out)
	}
}

func TestSubmitByURLSendsScenarioURLAsJSONAndUploadsNothing(t *testing.T) {
	var gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		b := new(bytes.Buffer)
		b.ReadFrom(r.Body)
		gotBody = b.String()
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"taskId":"t9"}`)
	}))
	defer srv.Close()

	c := newWorkerClient(Worker{Name: "w", URL: srv.URL})
	id, err := c.Submit(context.Background(), "https://config.example.com/scenarios/a.yaml")
	if err != nil || id != "t9" {
		t.Fatalf("Submit = %q, %v; want t9, nil", id, err)
	}
	if gotType != "application/json" || gotBody != `{"scenarioUrl":"https://config.example.com/scenarios/a.yaml"}` {
		t.Errorf("worker got %q %q, want a JSON scenarioUrl body", gotType, gotBody)
	}
}

func TestSubmitByURLRefusedByOneWorkerDoesNotAffectTheOther(t *testing.T) {
	ok := fakeWorkerAcceptingJSON(t, http.StatusAccepted)
	defer ok.Close()
	denied := fakeWorkerAcceptingJSON(t, http.StatusForbidden)
	defer denied.Close()

	cfg := &FleetConfig{Workers: []Worker{{Name: "a", URL: ok.URL}, {Name: "b", URL: denied.URL}}}
	results := runFleet(context.Background(), cfg, "https://config.example.com/s.yaml", t.TempDir(), 10*time.Millisecond)

	by := map[string]workerResult{}
	for _, r := range results {
		by[r.Worker.Name] = r
	}
	if by["a"].Status != "done" {
		t.Errorf("worker a = %q (%s), want done", by["a"].Status, by["a"].Err)
	}
	if by["b"].Status != "error" || !strings.Contains(by["b"].Err, "403") {
		t.Errorf("worker b = %q (%s), want an error mentioning 403", by["b"].Status, by["b"].Err)
	}
	if !anyFailed(results) {
		t.Error("anyFailed = false, want true")
	}
}

// fakeWorkerAcceptingJSON answers POST /task with the given status; on 202 it
// then behaves like a finished worker.
func fakeWorkerAcceptingJSON(t *testing.T, submitStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/task", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(submitStatus)
		if submitStatus == http.StatusAccepted {
			fmt.Fprint(w, `{"taskId":"t1"}`)
		} else {
			fmt.Fprint(w, `{"error":"scenario URL is not under an allowed source"}`)
		}
	})
	mux.HandleFunc("/task/t1", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"t1","status":"done"}`)
	})
	for _, name := range []string{"report.html", "report.json", "results.jsonl", "log"} {
		mux.HandleFunc("/task/t1/"+name, serveString("", "{}"))
	}
	return httptest.NewServer(mux)
}
