package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// These tests drive the real resonate binary against a throwaway local HTTP
// target, so they need `resonate` on $PATH (or $RESONATE_BIN). They skip
// themselves otherwise, so `go test ./...` still passes on a bare machine.
func realResonate(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("RESONATE_BIN")
	if bin == "" {
		bin = "resonate"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		t.Skipf("resonate binary %q not found; skipping integration test", bin)
	}
	return path
}

func integrationServer(t *testing.T, bin string) http.Handler {
	t.Helper()
	srv := &Server{Store: NewStore(), Runner: &Runner{ResonateBin: bin}, Workspace: t.TempDir()}
	mux := http.NewServeMux()
	srv.Routes(mux)
	return mux
}

func scenarioFor(targetURL, assertions string) string {
	return fmt.Sprintf(`protocol: http
load:
  duration: 2s
  rate: 10
  workers: 2
http:
  timeout: 2s
  targets:
    - method: GET
      url: %s/health
%s`, targetURL, assertions)
}

func TestIntegrationRealResonateEndToEnd(t *testing.T) {
	bin := realResonate(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer target.Close()

	mux := integrationServer(t, bin)
	id := submit(t, mux, scenarioFor(target.URL, "assertions:\n  - metric: success_rate\n    min: \"0.99\"\n"))
	s := waitDoneWithin(t, mux, id, 60)
	if s.Status != StatusDone || s.ExitCode != 0 {
		t.Fatalf("snapshot = %+v, want done/0; log:\n%s", s, get(t, mux, "/task/"+id+"/log").Body.String())
	}

	for _, name := range []string{"report.html", "report.json", "results.jsonl", "log"} {
		rec := get(t, mux, "/task/"+id+"/"+name)
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("GET %s = %d with %d bytes, want 200 and non-empty", name, rec.Code, rec.Body.Len())
		}
	}
	if !strings.Contains(get(t, mux, "/task/"+id+"/report.html").Body.String(), "<html") {
		t.Error("report.html doesn't look like HTML")
	}
}

func TestIntegrationFailingAssertionIsFailedButStillHasReport(t *testing.T) {
	bin := realResonate(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()

	mux := integrationServer(t, bin)
	id := submit(t, mux, scenarioFor(target.URL, "assertions:\n  - metric: success_rate\n    min: \"0.99\"\n"))
	s := waitDoneWithin(t, mux, id, 60)
	if s.Status != StatusFailed || s.ExitCode == 0 {
		t.Fatalf("snapshot = %+v, want failed/non-zero", s)
	}
	if rec := get(t, mux, "/task/"+id+"/report.json"); rec.Code != http.StatusOK {
		t.Errorf("report.json after failed assertions = %d, want 200", rec.Code)
	}
}

func TestIntegrationInvalidScenarioIsFailed(t *testing.T) {
	bin := realResonate(t)
	mux := integrationServer(t, bin)
	id := submit(t, mux, "this: is: not: [a valid scenario\n")
	s := waitDoneWithin(t, mux, id, 30)
	if s.Status != StatusFailed || s.ExitCode == 0 {
		t.Fatalf("snapshot = %+v, want failed/non-zero", s)
	}
	if get(t, mux, "/task/"+id+"/log").Body.Len() == 0 {
		t.Error("log is empty; want resonate's complaint about the bad scenario")
	}
}
