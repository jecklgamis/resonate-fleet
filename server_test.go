package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeResonateScript stands in for the real resonate binary: it writes the
// three report files resonate would, then exits with the code baked into its
// own name's suffix (via the SCRIPT_EXIT env var the test sets), so these
// tests don't depend on resonate being built or on-PATH.
func fakeResonateScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-resonate.sh")
	script := `#!/bin/sh
echo "fake resonate run: $@"
echo '{"requests":1}' > report.json
echo '<html>report</html>' > report.html
echo '{"ok":true}' > results.jsonl
exit "${SCRIPT_EXIT:-0}"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestServer(t *testing.T, exitCode int) *Server {
	t.Helper()
	t.Setenv("SCRIPT_EXIT", "")
	if exitCode != 0 {
		t.Setenv("SCRIPT_EXIT", "1")
	}
	return &Server{
		Store:     NewStore(),
		Runner:    &Runner{ResonateBin: fakeResonateScript(t)},
		Workspace: t.TempDir(),
	}
}

func submit(t *testing.T, mux http.Handler, scenario string) string {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "scenario.yaml")
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte(scenario))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/task", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /task = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["taskId"] == "" {
		t.Fatalf("response has no taskId: %s", rec.Body.String())
	}
	return resp["taskId"]
}

func waitDone(t *testing.T, mux http.Handler, id string) snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req := httptest.NewRequest(http.MethodGet, "/task/"+id, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var s snapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		if s.Status == StatusDone || s.Status == StatusFailed {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task did not finish within the deadline")
	return snapshot{}
}

func TestSubmitRunsAndServesReports(t *testing.T) {
	srv := newTestServer(t, 0)
	mux := http.NewServeMux()
	srv.Routes(mux)

	id := submit(t, mux, "protocol: http\nload: {requests: 1, workers: 1}\n")
	s := waitDone(t, mux, id)
	if s.Status != StatusDone || s.ExitCode != 0 {
		t.Fatalf("snapshot = %+v, want done/0", s)
	}

	for path, want := range map[string]string{
		"/task/" + id + "/report.html":   "<html>report</html>",
		"/task/" + id + "/report.json":   `{"requests":1}`,
		"/task/" + id + "/results.jsonl": `{"ok":true}`,
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		if got := strings.TrimRight(rec.Body.String(), "\n"); got != want {
			t.Errorf("GET %s = %q, want %q", path, got, want)
		}
	}
}

func TestSubmitFailureIsReportedAsFailed(t *testing.T) {
	srv := newTestServer(t, 1)
	mux := http.NewServeMux()
	srv.Routes(mux)

	id := submit(t, mux, "protocol: http\nload: {requests: 1, workers: 1}\n")
	s := waitDone(t, mux, id)
	if s.Status != StatusFailed || s.ExitCode == 0 {
		t.Fatalf("snapshot = %+v, want failed/non-zero", s)
	}
}

func TestUnknownTaskIs404(t *testing.T) {
	srv := newTestServer(t, 0)
	mux := http.NewServeMux()
	srv.Routes(mux)

	for _, path := range []string{"/task/nope", "/task/nope/report.html", "/task/nope/log"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestReportNotYetWrittenIs404WithStatus(t *testing.T) {
	srv := newTestServer(t, 0)
	mux := http.NewServeMux()
	srv.Routes(mux)

	t.Setenv("SCRIPT_EXIT", "")
	// A task whose workspace exists but whose runner hasn't written files yet —
	// simulate by adding it directly rather than racing the real goroutine.
	dir := t.TempDir()
	tk := &Task{ID: "pending", Dir: dir, Status: StatusRunning, CreatedAt: time.Now()}
	srv.Store.Add(tk)

	req := httptest.NewRequest(http.MethodGet, "/task/pending/report.html", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET report before it exists = %d, want 404", rec.Code)
	}
}

func TestAuthRequiresBearerTokenWhenSet(t *testing.T) {
	srv := newTestServer(t, 0)
	mux := http.NewServeMux()
	srv.Routes(mux)
	handler := withAuth("secret", mux)

	// No token: rejected.
	req := httptest.NewRequest(http.MethodGet, "/task", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}

	// Health check is exempt.
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz without token: got %d, want 200", rec.Code)
	}

	// Right token: accepted.
	req = httptest.NewRequest(http.MethodGet, "/task", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("right token: got %d, want 200", rec.Code)
	}
}

func TestAuthDisabledWhenTokenEmpty(t *testing.T) {
	srv := newTestServer(t, 0)
	mux := http.NewServeMux()
	srv.Routes(mux)
	handler := withAuth("", mux)

	req := httptest.NewRequest(http.MethodGet, "/task", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no token configured: got %d, want 200", rec.Code)
	}
}
