package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTasksSurviveRestart(t *testing.T) {
	srv, mux := newMux(t, 0)
	done := submit(t, mux, "protocol: http\n")
	waitDone(t, mux, done)

	// A "restart": a brand-new Store loaded from the same workspace.
	store, err := LoadStore(srv.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	srv2 := &Server{Store: store, Runner: srv.Runner, Workspace: srv.Workspace}
	mux2 := http.NewServeMux()
	srv2.Routes(mux2)

	rec := get(t, mux2, "/task/"+done)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status after restart = %d, want 200", rec.Code)
	}
	s := waitDone(t, mux2, done)
	if s.Status != StatusDone || s.Scenario != "scenario.yaml" || s.CreatedAt.IsZero() || s.FinishedAt.IsZero() {
		t.Fatalf("restored snapshot = %+v, want done with scenario and timestamps", s)
	}
	// Reports are still served for the restored task.
	if rec := get(t, mux2, "/task/"+done+"/report.json"); rec.Code != http.StatusOK {
		t.Errorf("report.json after restart = %d, want 200", rec.Code)
	}
}

func TestFailedTaskStaysFailedAfterRestart(t *testing.T) {
	srv, mux := newMux(t, 1)
	id := submit(t, mux, "protocol: http\n")
	waitDone(t, mux, id)

	store, err := LoadStore(srv.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := store.Get(id)
	if !ok {
		t.Fatal("failed task missing after restart")
	}
	if s := got.snapshot(); s.Status != StatusFailed || s.ExitCode == 0 {
		t.Fatalf("restored snapshot = %+v, want failed/non-zero", s)
	}
}

func TestInFlightTasksAreMarkedFailedOnLoad(t *testing.T) {
	ws := t.TempDir()
	for _, st := range []Status{StatusQueued, StatusRunning} {
		id := "inflight-" + string(st)
		dir := filepath.Join(ws, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		(&Task{ID: id, Dir: dir, Scenario: "s.yaml", CreatedAt: time.Now(), Status: st}).save()
	}

	store, err := LoadStore(ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"inflight-queued", "inflight-running"} {
		got, ok := store.Get(id)
		if !ok {
			t.Fatalf("%s missing after load", id)
		}
		s := got.snapshot()
		if s.Status != StatusFailed || s.ExitCode == 0 || s.Error == "" || s.FinishedAt.IsZero() {
			t.Errorf("%s = %+v, want failed with exit code, error and finish time", id, s)
		}
	}

	// The correction is persisted, so a second restart sees the same thing
	// without re-marking.
	again, _ := LoadStore(ws)
	got, _ := again.Get("inflight-running")
	if got.snapshot().Status != StatusFailed {
		t.Error("failed state wasn't persisted")
	}
}

func TestLoadStoreSkipsJunk(t *testing.T) {
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "stray-file.txt"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(ws, "no-state"), 0o755) // task dir from before persistence
	bad := filepath.Join(ws, "corrupt")
	os.MkdirAll(bad, 0o755)
	os.WriteFile(filepath.Join(bad, taskFile), []byte("{not json"), 0o644)
	mismatch := filepath.Join(ws, "mismatch")
	os.MkdirAll(mismatch, 0o755)
	os.WriteFile(filepath.Join(mismatch, taskFile), []byte(`{"id":"other","status":"done"}`), 0o644)

	store, err := LoadStore(ws)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(store.List()); n != 0 {
		t.Fatalf("loaded %d tasks from junk, want 0", n)
	}
}

func TestLoadStoreMissingWorkspaceIsEmpty(t *testing.T) {
	store, err := LoadStore(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil || len(store.List()) != 0 {
		t.Fatalf("LoadStore(missing) = %d tasks, err %v; want empty, nil", len(store.List()), err)
	}
}

func TestSaveLeavesNoTempFile(t *testing.T) {
	srv, mux := newMux(t, 0)
	id := submit(t, mux, "protocol: http\n")
	waitDone(t, mux, id)
	if _, err := os.Stat(filepath.Join(srv.Workspace, id, taskFile+".tmp")); !os.IsNotExist(err) {
		t.Errorf("temp state file left behind (stat err: %v)", err)
	}
}
