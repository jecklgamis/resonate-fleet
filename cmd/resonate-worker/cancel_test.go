package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// slowResonate stands in for a resonate that runs until it is interrupted.
func slowResonate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "slow-resonate.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func del(mux http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, path, nil))
	return rec
}

func TestCancelStopsARunningTask(t *testing.T) {
	srv := newTestServer(t, 0)
	srv.Runner.ResonateBin = slowResonate(t)
	mux := http.NewServeMux()
	srv.Routes(mux)

	id := submit(t, mux, "protocol: http\n")
	task, _ := srv.Store.Get(id)
	for i := 0; i < 500 && task.snapshot().Status != StatusRunning; i++ {
		time.Sleep(10 * time.Millisecond)
	}

	if rec := del(mux, "/task/"+id); rec.Code != http.StatusAccepted {
		t.Fatalf("DELETE running task = %d %s", rec.Code, rec.Body)
	}
	var s snapshot
	for i := 0; i < 500; i++ {
		if s = task.snapshot(); s.Status != StatusRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.Status != StatusCanceled || s.Error != "canceled" {
		t.Fatalf("after cancel: status=%s error=%q, want canceled", s.Status, s.Error)
	}
	// Canceling again, now that it has finished, is harmless.
	if rec := del(mux, "/task/"+id); rec.Code != http.StatusOK {
		t.Fatalf("DELETE finished task = %d, want 200", rec.Code)
	}
}

func TestCancelUnknownTask(t *testing.T) {
	srv := newTestServer(t, 0)
	mux := http.NewServeMux()
	srv.Routes(mux)
	if rec := del(mux, "/task/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE unknown = %d, want 404", rec.Code)
	}
}
