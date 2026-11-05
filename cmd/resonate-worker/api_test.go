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
)

func newMux(t *testing.T, exitCode int) (*Server, http.Handler) {
	t.Helper()
	srv := newTestServer(t, exitCode)
	mux := http.NewServeMux()
	srv.Routes(mux)
	return srv, mux
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	_, mux := newMux(t, 0)
	rec := get(t, mux, "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("GET /healthz = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
}

func TestListTasks(t *testing.T) {
	_, mux := newMux(t, 0)

	rec := get(t, mux, "/task")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /task = %d", rec.Code)
	}
	var empty []snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil || len(empty) != 0 {
		t.Fatalf("empty list = %q (err %v), want []", rec.Body.String(), err)
	}

	a := submit(t, mux, "protocol: http\n")
	b := submit(t, mux, "protocol: http\n")
	waitDone(t, mux, a)
	waitDone(t, mux, b)

	var list []snapshot
	if err := json.Unmarshal(get(t, mux, "/task").Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, s := range list {
		ids[s.ID] = true
	}
	if len(list) != 2 || !ids[a] || !ids[b] {
		t.Fatalf("list = %+v, want tasks %s and %s", list, a, b)
	}
}

func TestLogEndpointServesResonateOutput(t *testing.T) {
	_, mux := newMux(t, 0)
	id := submit(t, mux, "protocol: http\n")
	waitDone(t, mux, id)

	rec := get(t, mux, "/task/"+id+"/log")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET log = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "fake resonate run: run scenario.yaml") {
		t.Errorf("log = %q, want the fake resonate's output", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("log Content-Type = %q, want text/plain", ct)
	}
}

func TestContentTypes(t *testing.T) {
	_, mux := newMux(t, 0)
	id := submit(t, mux, "protocol: http\n")
	waitDone(t, mux, id)

	for path, want := range map[string]string{
		"/task/" + id + "/report.html":   "text/html",
		"/task/" + id + "/report.json":   "application/json",
		"/task/" + id + "/results.jsonl": "application/x-ndjson",
	} {
		if ct := get(t, mux, path).Header().Get("Content-Type"); !strings.HasPrefix(ct, want) {
			t.Errorf("GET %s Content-Type = %q, want %s", path, ct, want)
		}
	}
}

func TestFailedRunStillServesReports(t *testing.T) {
	_, mux := newMux(t, 1)
	id := submit(t, mux, "protocol: http\n")
	if s := waitDone(t, mux, id); s.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", s.Status)
	}
	if rec := get(t, mux, "/task/"+id+"/report.json"); rec.Code != http.StatusOK {
		t.Fatalf("report.json after failed run = %d, want 200", rec.Code)
	}
}

func TestSubmitSavesScenarioAndRecordsOriginalName(t *testing.T) {
	srv, mux := newMux(t, 0)
	const scenario = "protocol: http\nload: {rate: 1}\n"
	id := submit(t, mux, scenario)
	s := waitDone(t, mux, id)

	if s.Scenario != "scenario.yaml" {
		t.Errorf("snapshot scenario = %q, want the uploaded filename", s.Scenario)
	}
	got, err := os.ReadFile(filepath.Join(srv.Workspace, id, "scenario.yaml"))
	if err != nil || string(got) != scenario {
		t.Fatalf("saved scenario = %q (err %v), want %q", got, err, scenario)
	}
}

func TestSubmitWithoutFileFieldIs400(t *testing.T) {
	_, mux := newMux(t, 0)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("wrong", "scenario.yaml")
	part.Write([]byte("protocol: http\n"))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/task", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST without file field = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("body = %q, want a JSON error", rec.Body.String())
	}
}

func TestSubmitNotMultipartIs400(t *testing.T) {
	_, mux := newMux(t, 0)
	req := httptest.NewRequest(http.MethodPost, "/task", strings.NewReader("protocol: http"))
	req.Header.Set("Content-Type", "application/x-yaml")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST non-multipart = %d, want 400", rec.Code)
	}
}

func TestMissingResonateBinaryIsReportedAsFailed(t *testing.T) {
	srv, mux := newMux(t, 0)
	srv.Runner.ResonateBin = filepath.Join(t.TempDir(), "does-not-exist")

	id := submit(t, mux, "protocol: http\n")
	s := waitDone(t, mux, id)
	if s.Status != StatusFailed || s.ExitCode == 0 || s.Error == "" {
		t.Fatalf("snapshot = %+v, want failed with non-zero exit and an error", s)
	}
}

func TestAuthRejectsWrongTokenAndMalformedHeader(t *testing.T) {
	_, mux := newMux(t, 0)
	h := withAuth("secret", mux)

	for _, auth := range []string{"Bearer wrong", "secret", "Basic secret", "bearer secret"} {
		req := httptest.NewRequest(http.MethodGet, "/task", nil)
		req.Header.Set("Authorization", auth)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q = %d, want 401", auth, rec.Code)
		}
	}
}

func TestAuthProtectsSubmitAndReports(t *testing.T) {
	_, mux := newMux(t, 0)
	h := withAuth("secret", mux)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/task"},
		{http.MethodGet, "/task/anything"},
		{http.MethodGet, "/task/anything/report.html"},
		{http.MethodGet, "/task/anything/log"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestRequestLogRecordsStatusAndNeverTheToken(t *testing.T) {
	var logBuf bytes.Buffer
	oldOut, oldFlags := logWriter(&logBuf)
	defer restoreLog(oldOut, oldFlags)

	_, mux := newMux(t, 0)
	h := withRequestLog(withAuth("super-secret-token", mux))

	req := httptest.NewRequest(http.MethodGet, "/task", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	h.ServeHTTP(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodGet, "/task", nil)
	req.Header.Set("Authorization", "Bearer super-secret-token")
	h.ServeHTTP(httptest.NewRecorder(), req)

	out := logBuf.String()
	if !strings.Contains(out, "GET /task -> 401") {
		t.Errorf("log missing the rejected request:\n%s", out)
	}
	if !strings.Contains(out, "GET /task -> 200") {
		t.Errorf("log missing the accepted request:\n%s", out)
	}
	if strings.Contains(out, "super-secret-token") || strings.Contains(out, "wrong-token") {
		t.Errorf("log leaked a bearer token:\n%s", out)
	}
}
