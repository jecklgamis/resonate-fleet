package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeWorker implements the slice of resonate-worker's API the controller uses.
func fakeWorker(t *testing.T, token string, code string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", 401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("POST /task", auth(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			r.ParseMultipartForm(1 << 20)
			if f, _, err := r.FormFile("file"); err != nil {
				t.Errorf("no scenario file: %v", err)
			} else if b, _ := io.ReadAll(f); string(b) != "protocol: http\n" {
				t.Errorf("scenario = %q", b)
			}
			if len(r.MultipartForm.File["files"]) != 1 {
				t.Errorf("data files not forwarded")
			}
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"taskId":"t1"}`))
	}))
	mux.HandleFunc("GET /task/t1", auth(func(w http.ResponseWriter, r *http.Request) {
		st := "running"
		if polls.Add(1) > 1 {
			st = code
		}
		fmt.Fprintf(w, `{"status":%q,"exit_code":0}`, st)
	}))
	mux.HandleFunc("GET /task/t1/report.json", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"requests":7}`))
	}))
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s, &polls
}

func newController(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	srv := &Server{Registry: NewRegistry(time.Minute), Runs: NewRuns(5*time.Millisecond, 5*time.Second)}
	mux := http.NewServeMux()
	srv.Routes(mux)
	return srv, withAuth("ct", mux)
}

func call(h http.Handler, method, path, ct string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer ct")
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func register(t *testing.T, h http.Handler, name, url, token string, labels map[string]string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"name": name, "url": url, "token": token, "labels": labels})
	if rec := call(h, "POST", "/workers", "application/json", bytes.NewReader(b)); rec.Code != 204 {
		t.Fatalf("register = %d %s", rec.Code, rec.Body)
	}
}

func startRun(t *testing.T, h http.Handler, query string) string {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	p, _ := mw.CreateFormFile("file", "s.yaml")
	p.Write([]byte("protocol: http\n"))
	p, _ = mw.CreateFormFile("files", "users.csv")
	p.Write([]byte("id\n1\n"))
	mw.Close()
	rec := call(h, "POST", "/runs"+query, mw.FormDataContentType(), &buf)
	if rec.Code != 202 {
		t.Fatalf("start run = %d %s", rec.Code, rec.Body)
	}
	var out struct{ RunID string }
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out.RunID
}

func waitRun(t *testing.T, h http.Handler, id string) runSnapshot {
	t.Helper()
	for i := 0; i < 400; i++ {
		var s runSnapshot
		json.Unmarshal(call(h, "GET", "/runs/"+id, "", nil).Body.Bytes(), &s)
		if s.Status != "running" {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run did not finish")
	return runSnapshot{}
}

func TestRunFansOutAndProxiesReports(t *testing.T) {
	_, h := newController(t)
	a, _ := fakeWorker(t, "ta", "done")
	b, _ := fakeWorker(t, "tb", "failed")
	register(t, h, "a", a.URL, "ta", map[string]string{"region": "sgp1"})
	register(t, h, "b", b.URL, "tb", map[string]string{"region": "nyc1"})

	s := waitRun(t, h, startRun(t, h, ""))
	if s.Status != "failed" || len(s.Workers) != 2 || s.Workers[0].Status != "done" || s.Workers[1].Status != "failed" {
		t.Fatalf("run = %+v", s)
	}
	rec := call(h, "GET", "/runs/"+s.ID+"/a/report.json", "", nil)
	if rec.Code != 200 || rec.Body.String() != `{"requests":7}` {
		t.Fatalf("proxy = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/runs/"+s.ID+"/a/../etc", "", nil); rec.Code == 200 {
		t.Fatal("unexpected 200")
	}
	if rec := call(h, "GET", "/runs/"+s.ID+"/a/secrets", "", nil); rec.Code != 404 {
		t.Fatalf("unknown file = %d", rec.Code)
	}
}

func TestRunLabelSelectorAndNoWorkers(t *testing.T) {
	_, h := newController(t)
	a, _ := fakeWorker(t, "ta", "done")
	b, _ := fakeWorker(t, "tb", "done")
	register(t, h, "a", a.URL, "ta", map[string]string{"region": "sgp1"})
	register(t, h, "b", b.URL, "tb", map[string]string{"region": "nyc1"})

	s := waitRun(t, h, startRun(t, h, "?label=region=nyc1"))
	if s.Status != "done" || len(s.Workers) != 1 || s.Workers[0].Name != "b" {
		t.Fatalf("run = %+v", s)
	}
	rec := call(h, "POST", "/runs?label=region=mars", "application/json", strings.NewReader(`{"scenarioUrl":"http://x/s.yaml"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("no match = %d", rec.Code)
	}
}

func TestUnreachableWorkerIsAnErrorRow(t *testing.T) {
	_, h := newController(t)
	register(t, h, "gone", "http://127.0.0.1:1", "", nil)
	s := waitRun(t, h, startRun(t, h, ""))
	if s.Status != "failed" || s.Workers[0].Status != "error" {
		t.Fatalf("run = %+v", s)
	}
}

func TestAuthAndRegistryRules(t *testing.T) {
	srv, h := newController(t)
	req := httptest.NewRequest("GET", "/workers", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("no token = %d", rec.Code)
	}
	for _, bad := range []string{`{"name":"a/b","url":"http://x"}`, `{"name":"a","url":"ftp://x"}`, `{"name":"","url":"http://x"}`} {
		if rec := call(h, "POST", "/workers", "application/json", strings.NewReader(bad)); rec.Code != 400 {
			t.Errorf("accepted %s", bad)
		}
	}
	register(t, h, "a", "http://x:1", "secret-token", nil)
	if rec := call(h, "GET", "/workers", "", nil); strings.Contains(rec.Body.String(), "secret-token") {
		t.Fatal("worker token leaked in listing")
	}
	srv.Registry.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if len(srv.Registry.Live(nil)) != 0 {
		t.Fatal("expired worker still live")
	}
}

func TestRegistryOutcomesAndReap(t *testing.T) {
	r := NewRegistry(time.Minute)
	now := time.Now()
	r.now = func() time.Time { return now }
	e := Entry{Name: "a", URL: "http://x:1", Labels: map[string]string{"r": "1"}}
	for i, want := range []string{Registered, Refreshed} {
		if got, err := r.Register(e); err != nil || got != want {
			t.Fatalf("register #%d = %q, %v; want %q", i, got, err, want)
		}
	}
	e.Labels = map[string]string{"r": "2"}
	if got, _ := r.Register(e); got != Changed {
		t.Fatalf("label change = %q", got)
	}
	e.URL = "http://x:2"
	if got, _ := r.Register(e); got != Changed {
		t.Fatalf("url change = %q", got)
	}
	if gone := r.Reap(); len(gone) != 0 {
		t.Fatalf("reaped live worker: %v", gone)
	}
	now = now.Add(2 * time.Minute)
	if gone := r.Reap(); len(gone) != 1 || gone[0].Name != "a" {
		t.Fatalf("reap = %v", gone)
	}
	if got, _ := r.Register(e); got != Registered {
		t.Fatalf("after expiry = %q, want a fresh registration", got)
	}
	if _, ok := r.Remove("a"); !ok {
		t.Fatal("remove of a live worker reported not found")
	}
	if _, ok := r.Remove("a"); ok {
		t.Fatal("second remove reported found")
	}
}

func TestRegistrationLogging(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	_, h := newController(t)
	register(t, h, "a", "http://x:1", "secret-token", map[string]string{"r": "1"})
	register(t, h, "a", "http://x:1", "secret-token", map[string]string{"r": "1"}) // heartbeat: silent
	register(t, h, "a", "http://x:2", "secret-token", map[string]string{"r": "1"})
	call(h, "DELETE", "/workers/a", "", nil)
	call(h, "DELETE", "/workers/ghost", "", nil)
	out := buf.String()
	for _, want := range []string{
		"worker registered: a url=http://x:1 labels=[r=1] (1 live)",
		"worker re-registered with changes: a url=http://x:2",
		"worker deregistered: a url=http://x:2",
		`deregistration for unknown worker "ghost"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "worker registered"); n != 1 {
		t.Errorf("heartbeat was logged: %d registrations in\n%s", n, out)
	}
	if strings.Contains(out, "secret-token") {
		t.Error("worker token leaked into the log")
	}
}

func TestReapLoopLogsExpiredWorkers(t *testing.T) {
	var buf syncBuffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	srv := &Server{Registry: NewRegistry(30 * time.Millisecond), Runs: NewRuns(time.Second, time.Second)}
	stop := make(chan struct{})
	defer close(stop)
	go srv.ReapLoop(10*time.Millisecond, stop)
	if _, err := srv.Registry.Register(Entry{Name: "gone", URL: "http://x:9", Labels: map[string]string{"r": "1"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if strings.Contains(buf.String(), "worker expired (no heartbeat for 30ms): gone url=http://x:9 labels=[r=1] (0 live)") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no expiry log line:\n%s", buf.String())
}

// syncBuffer is a bytes.Buffer safe for the log package to write while a test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
