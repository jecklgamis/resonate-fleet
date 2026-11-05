package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const remoteScenario = "protocol: http\nload: {rate: 1}\n"

// configServer serves scenarios under /scenarios/ and something that must stay
// out of reach under /secret/.
func configServer(t *testing.T, gotAuth *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/scenarios/checkout.yaml", func(w http.ResponseWriter, r *http.Request) {
		if gotAuth != nil {
			*gotAuth = r.Header.Get("Authorization")
		}
		w.Write([]byte(remoteScenario))
	})
	mux.HandleFunc("/scenarios/huge.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), 4096))
	})
	mux.HandleFunc("/scenarios/gone.yaml", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/scenarios/redirect-out.yaml", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/secret/creds.yaml", http.StatusFound)
	})
	mux.HandleFunc("/scenarios/redirect-in.yaml", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/scenarios/checkout.yaml", http.StatusFound)
	})
	mux.HandleFunc("/secret/creds.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("password: hunter2\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func urlServer(t *testing.T, allow []string, token string) (*Server, http.Handler) {
	t.Helper()
	srv, mux := newMux(t, 0)
	f, err := NewSourceFetcher(allow, token)
	if err != nil {
		t.Fatal(err)
	}
	f.MaxBytes = 1024
	srv.Fetcher = f
	return srv, mux
}

func postURL(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/task", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func submitURL(t *testing.T, h http.Handler, u string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"scenarioUrl": u})
	rec := postURL(t, h, string(b))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST scenarioUrl %s = %d, body %s", u, rec.Code, rec.Body.String())
	}
	var resp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp["taskId"]
}

func TestSubmitByURLFetchesSnapshotsAndRuns(t *testing.T) {
	cfg := configServer(t, nil)
	srv, mux := urlServer(t, []string{cfg.URL + "/scenarios"}, "")

	u := cfg.URL + "/scenarios/checkout.yaml"
	id := submitURL(t, mux, u)
	s := waitDone(t, mux, id)

	sum := sha256.Sum256([]byte(remoteScenario))
	if s.Status != StatusDone || s.Scenario != "checkout.yaml" || s.Source != u || s.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("snapshot = %+v, want done, named checkout.yaml, with source and sha256", s)
	}
	got, err := os.ReadFile(filepath.Join(srv.Workspace, id, "scenario.yaml"))
	if err != nil || string(got) != remoteScenario {
		t.Fatalf("saved scenario = %q (err %v), want the fetched bytes", got, err)
	}
}

func TestUploadRecordsHashButNoSource(t *testing.T) {
	_, mux := newMux(t, 0)
	id := submit(t, mux, remoteScenario)
	s := waitDone(t, mux, id)
	if s.Source != "" || s.SHA256 == "" {
		t.Fatalf("snapshot = %+v, want a sha256 and no source for an upload", s)
	}
}

func TestSubmitByURLDisabledByDefault(t *testing.T) {
	cfg := configServer(t, nil)
	_, mux := newMux(t, 0) // no Fetcher at all
	rec := postURL(t, mux, `{"scenarioUrl":"`+cfg.URL+`/scenarios/checkout.yaml"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "SCENARIO_SOURCES") {
		t.Fatalf("got %d %s, want 403 pointing at SCENARIO_SOURCES", rec.Code, rec.Body.String())
	}

	_, mux = urlServer(t, nil, "") // a Fetcher with an empty allowlist
	if rec := postURL(t, mux, `{"scenarioUrl":"`+cfg.URL+`/scenarios/checkout.yaml"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("empty allowlist = %d, want 403", rec.Code)
	}
}

func TestSubmitByURLRejectsEverythingOutsideTheAllowlist(t *testing.T) {
	cfg := configServer(t, nil)
	_, mux := urlServer(t, []string{cfg.URL + "/scenarios"}, "")
	host := strings.TrimPrefix(cfg.URL, "http://")

	for name, u := range map[string]string{
		"other path":         cfg.URL + "/secret/creds.yaml",
		"sibling prefix":     cfg.URL + "/scenarios-evil/x.yaml",
		"dot-dot escape":     cfg.URL + "/scenarios/../secret/creds.yaml",
		"encoded dot-dot":    cfg.URL + "/scenarios/%2e%2e/secret/creds.yaml",
		"userinfo trick":     "http://user@" + host + "/scenarios/checkout.yaml",
		"host suffix trick":  "http://127.0.0.1.evil.example:" + strings.Split(host, ":")[1] + "/scenarios/checkout.yaml",
		"other scheme (tls)": "https://" + host + "/scenarios/checkout.yaml",
		"other host":         "http://169.254.169.254/latest/meta-data/",
	} {
		b, _ := json.Marshal(map[string]string{"scenarioUrl": u})
		if rec := postURL(t, mux, string(b)); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %s = %d, want 403", name, u, rec.Code)
		}
	}
}

func TestSubmitByURLBadRequests(t *testing.T) {
	cfg := configServer(t, nil)
	_, mux := urlServer(t, []string{cfg.URL}, "")

	for name, body := range map[string]string{
		"not json":      `scenarioUrl=http://x`,
		"missing field": `{}`,
		"empty url":     `{"scenarioUrl":""}`,
		"s3 scheme":     `{"scenarioUrl":"s3://bucket/key.yaml"}`,
		"file scheme":   `{"scenarioUrl":"file:///etc/passwd"}`,
		"no host":       `{"scenarioUrl":"/scenarios/checkout.yaml"}`,
	} {
		rec := postURL(t, mux, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
}

func TestSubmitByURLSourceProblems(t *testing.T) {
	cfg := configServer(t, nil)
	srv, mux := urlServer(t, []string{cfg.URL + "/scenarios"}, "")

	for name, tc := range map[string]struct {
		path string
		want int
	}{
		"source 404":                {"/scenarios/gone.yaml", http.StatusBadGateway},
		"over the size limit":       {"/scenarios/huge.yaml", http.StatusRequestEntityTooLarge},
		"redirect out of allowlist": {"/scenarios/redirect-out.yaml", http.StatusForbidden},
	} {
		b, _ := json.Marshal(map[string]string{"scenarioUrl": cfg.URL + tc.path})
		if rec := postURL(t, mux, string(b)); rec.Code != tc.want {
			t.Errorf("%s: got %d %s, want %d", name, rec.Code, rec.Body.String(), tc.want)
		}
	}

	// A refused submission must not leave a task or a half-written workspace.
	if n := len(srv.Store.List()); n != 0 {
		t.Errorf("%d task(s) recorded after failed fetches, want 0", n)
	}
	if entries, _ := os.ReadDir(srv.Workspace); len(entries) != 0 {
		t.Errorf("%d leftover workspace dir(s) after failed fetches, want 0", len(entries))
	}
}

func TestSubmitByURLFollowsRedirectsWithinTheAllowlist(t *testing.T) {
	cfg := configServer(t, nil)
	_, mux := urlServer(t, []string{cfg.URL + "/scenarios"}, "")
	id := submitURL(t, mux, cfg.URL+"/scenarios/redirect-in.yaml")
	if s := waitDone(t, mux, id); s.Status != StatusDone {
		t.Fatalf("snapshot = %+v, want done", s)
	}
}

func TestSubmitByURLSendsTokenWhenConfigured(t *testing.T) {
	var got string
	cfg := configServer(t, &got)
	_, mux := urlServer(t, []string{cfg.URL}, "fetch-secret")
	waitDone(t, mux, submitURL(t, mux, cfg.URL+"/scenarios/checkout.yaml"))
	if got != "Bearer fetch-secret" {
		t.Errorf("source saw Authorization %q, want the configured token", got)
	}

	_, mux = urlServer(t, []string{cfg.URL}, "")
	waitDone(t, mux, submitURL(t, mux, cfg.URL+"/scenarios/checkout.yaml"))
	if got != "" {
		t.Errorf("with no token configured the source saw Authorization %q, want none", got)
	}
}

func TestSourceSurvivesRestart(t *testing.T) {
	cfg := configServer(t, nil)
	srv, mux := urlServer(t, []string{cfg.URL}, "")
	u := cfg.URL + "/scenarios/checkout.yaml"
	id := submitURL(t, mux, u)
	want := waitDone(t, mux, id)

	store, err := LoadStore(srv.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := store.Get(id)
	if !ok {
		t.Fatal("task missing after restart")
	}
	if s := got.snapshot(); s.Source != u || s.SHA256 != want.SHA256 {
		t.Fatalf("restored %+v, want source %s and sha256 %s", s, u, want.SHA256)
	}
}

func TestSubmitByURLRequiresAuthLikeAnyOtherSubmit(t *testing.T) {
	cfg := configServer(t, nil)
	_, mux := urlServer(t, []string{cfg.URL}, "")
	h := withAuth("secret", mux)
	rec := postURL(t, h, `{"scenarioUrl":"`+cfg.URL+`/scenarios/checkout.yaml"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST scenarioUrl without token = %d, want 401", rec.Code)
	}
}

func TestNewSourceFetcherRejectsBadAllowlistEntries(t *testing.T) {
	for _, bad := range []string{"s3://bucket", "ftp://host/x", "not a url", "https://", "https://user:pw@host/"} {
		if _, err := NewSourceFetcher([]string{bad}, ""); err == nil {
			t.Errorf("NewSourceFetcher(%q) succeeded, want an error", bad)
		}
	}
	f, err := NewSourceFetcher([]string{" https://a.example/x ", "", "http://b.example:8080"}, "")
	if err != nil || len(f.Allow) != 2 {
		t.Fatalf("NewSourceFetcher = %+v, %v; want 2 trimmed entries", f, err)
	}
}

func TestAllowedMatching(t *testing.T) {
	f, _ := NewSourceFetcher([]string{"https://cfg.example.com/scenarios/", "http://internal:8080"}, "")
	for raw, want := range map[string]bool{
		"https://cfg.example.com/scenarios/a.yaml":      true,
		"https://cfg.example.com/scenarios/team/a.yaml": true,
		"https://CFG.example.com/scenarios/a.yaml":      true,
		"https://cfg.example.com/scenarios":             true,
		"https://cfg.example.com/scenariosX/a.yaml":     false,
		"https://cfg.example.com/other/a.yaml":          false,
		"https://cfg.example.com:444/scenarios/a.yaml":  false,
		"http://cfg.example.com/scenarios/a.yaml":       false,
		"http://internal:8080/anything/at/all":          true,
		"http://internal:9090/anything":                 false,
	} {
		u, _ := url.Parse(raw)
		if got := f.allowed(u); got != want {
			t.Errorf("allowed(%s) = %v, want %v", raw, got, want)
		}
	}
}
