package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// cancelableWorker runs until DELETE /task/t1, then reports "canceled".
func cancelableWorker(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var canceled atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /task", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"taskId":"t1"}`))
	})
	mux.HandleFunc("GET /task/t1", func(w http.ResponseWriter, r *http.Request) {
		st := "running"
		if canceled.Load() > 0 {
			st = "canceled"
		}
		w.Write([]byte(`{"status":"` + st + `"}`))
	})
	mux.HandleFunc("DELETE /task/t1", func(w http.ResponseWriter, r *http.Request) {
		canceled.Add(1)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{}`))
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s, &canceled
}

func TestAbortCancelsWorkersAndMarksRunCanceled(t *testing.T) {
	_, h := newController(t)
	w, canceled := cancelableWorker(t)
	register(t, h, "w1", w.URL, "", nil)
	id := startRun(t, h, "")

	// Wait until the worker has a task, so abort has something to cancel.
	for i := 0; i < 400; i++ {
		var s runSnapshot
		json.Unmarshal(call(h, "GET", "/runs/"+id, "", nil).Body.Bytes(), &s)
		if s.Workers[0].Status == "running" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rec := call(h, "DELETE", "/runs/"+id, "", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("abort = %d %s", rec.Code, rec.Body)
	}
	s := waitRun(t, h, id)
	if s.Status != "canceled" || s.Workers[0].Status != "canceled" {
		t.Fatalf("run=%s worker=%s, want canceled", s.Status, s.Workers[0].Status)
	}
	if canceled.Load() != 1 {
		t.Fatalf("worker cancel calls = %d, want 1", canceled.Load())
	}
	// Aborting a finished run is a no-op, not an error.
	if rec := call(h, "DELETE", "/runs/"+id, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("second abort = %d, want 200", rec.Code)
	}
	if canceled.Load() != 1 {
		t.Fatal("second abort canceled the task again")
	}
}

func TestAbortUnknownRun(t *testing.T) {
	_, h := newController(t)
	if rec := call(h, "DELETE", "/runs/nope", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("abort unknown = %d, want 404", rec.Code)
	}
}
