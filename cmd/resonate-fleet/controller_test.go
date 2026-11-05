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
		if _, _, err := r.FormFile("file"); err != nil || len(r.MultipartForm.File["files"]) != 1 {
			t.Errorf("scenario/data not uploaded: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"runId":"r1","workers":["a","b"]}`))
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
	results, err := runViaController(t.Context(), c, scenario, []string{data}, []string{"region=sgp1"}, out, 5*time.Millisecond)
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
	if _, err := c.Start(t.Context(), "https://x/s.yaml", []string{"f.csv"}, nil); err == nil || !strings.Contains(err.Error(), "--data") {
		t.Fatalf("URL + --data: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no live workers match"}`, 409)
	}))
	defer srv.Close()
	c = newControllerClient(srv.URL, "")
	_, err := c.Start(t.Context(), "https://x/s.yaml", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no live workers") {
		t.Fatalf("409: %v", err)
	}
}
