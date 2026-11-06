package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func submitAt(mux http.Handler, query string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	p, _ := mw.CreateFormFile("file", "scenario.yaml")
	p.Write([]byte("protocol: http\nload: {requests: 1, workers: 1}\n"))
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/task"+query, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestStartAtHoldsTheTaskUntilThen(t *testing.T) {
	srv := newTestServer(t, 0)
	mux := http.NewServeMux()
	srv.Routes(mux)

	start := time.Now().Add(400 * time.Millisecond)
	rec := submitAt(mux, "?startAt="+start.UTC().Format(time.RFC3339Nano))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body)
	}
	var resp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["startAt"] == "" || resp["now"] == "" {
		t.Fatalf("response lacks startAt/now: %v", resp)
	}
	task, _ := srv.Store.Get(resp["taskId"])
	time.Sleep(100 * time.Millisecond)
	if st := task.snapshot().Status; st != StatusQueued {
		t.Fatalf("status before startAt = %s, want queued", st)
	}
	if _, err := os.Stat(filepath.Join(task.Dir, "report.json")); err == nil {
		t.Fatal("resonate ran before startAt")
	}
	s := waitDone(t, mux, resp["taskId"])
	if s.Status != StatusDone {
		t.Fatalf("status = %s", s.Status)
	}
	if s.FinishedAt.Before(start) {
		t.Fatalf("finished %s, before startAt %s", s.FinishedAt, start)
	}
	if s.StartAt == nil || s.StartAt.Before(start.Add(-time.Millisecond)) {
		t.Fatalf("start_at not reported: %v", s.StartAt)
	}
}

func TestStartAtInThePastStartsAtOnce(t *testing.T) {
	srv := newTestServer(t, 0)
	mux := http.NewServeMux()
	srv.Routes(mux)
	rec := submitAt(mux, "?startAt="+time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body)
	}
	var resp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if s := waitDone(t, mux, resp["taskId"]); s.Status != StatusDone {
		t.Fatalf("status = %s", s.Status)
	}
}

func TestStartAtValidation(t *testing.T) {
	srv := newTestServer(t, 0)
	mux := http.NewServeMux()
	srv.Routes(mux)
	for name, q := range map[string]string{
		"not a time":   "?startAt=tomorrow",
		"too far away": "?startAt=" + time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	} {
		if rec := submitAt(mux, q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: = %d %s, want 400", name, rec.Code, rec.Body)
		}
	}
	if n := len(srv.Store.List()); n != 0 {
		t.Fatalf("a rejected submission left %d task(s) behind", n)
	}
	// Without startAt the response is unchanged: just the task id.
	var resp map[string]string
	json.Unmarshal(submitAt(mux, "").Body.Bytes(), &resp)
	if len(resp) != 1 || resp["taskId"] == "" {
		t.Fatalf("response = %v, want only taskId", resp)
	}
	waitDone(t, mux, resp["taskId"]) // don't leave the task running past the test
}

func TestCancelWhileWaitingForStartAtNeverRuns(t *testing.T) {
	srv := newTestServer(t, 0)
	mux := http.NewServeMux()
	srv.Routes(mux)
	rec := submitAt(mux, "?startAt="+time.Now().Add(30*time.Second).UTC().Format(time.RFC3339Nano))
	var resp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resp)
	task, _ := srv.Store.Get(resp["taskId"])

	if rec := del(mux, "/task/"+resp["taskId"]); rec.Code != http.StatusAccepted {
		t.Fatalf("DELETE queued task = %d %s", rec.Code, rec.Body)
	}
	var s snapshot
	for i := 0; i < 300; i++ {
		if s = task.snapshot(); s.Status != StatusQueued {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.Status != StatusCanceled {
		t.Fatalf("status = %s, want canceled", s.Status)
	}
	if _, err := os.Stat(filepath.Join(task.Dir, "report.json")); err == nil {
		t.Fatal("canceled task still ran resonate")
	}
}
