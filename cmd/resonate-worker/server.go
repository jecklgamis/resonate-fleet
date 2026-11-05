package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Server wires the Store and Runner to HTTP handlers. It holds no other state.
type Server struct {
	Store     *Store
	Runner    *Runner
	Workspace string // base directory; each task gets Workspace/<taskID>/

	// Fetcher downloads scenarios submitted by URL. Nil, or one with no allowed
	// sources, means URL submissions are refused.
	Fetcher *SourceFetcher
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /task", s.handleSubmit)
	mux.HandleFunc("GET /task", s.handleList)
	mux.HandleFunc("GET /task/{id}", s.handleStatus)
	mux.HandleFunc("GET /task/{id}/report.html", s.handleFile("report.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /task/{id}/report.json", s.handleFile("report.json", "application/json"))
	mux.HandleFunc("GET /task/{id}/results.jsonl", s.handleFile("results.jsonl", "application/x-ndjson"))
	mux.HandleFunc("GET /task/{id}/log", s.handleFile("log.txt", "text/plain; charset=utf-8"))
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handleSubmit starts a task from a scenario, given one of two ways:
//
//   - a multipart upload with a "file" field — the scenario YAML itself — and
//     any number of "files" fields: data files (feeders, body files) saved into
//     the task workspace under their own names for the scenario to refer to
//   - a JSON body {"scenarioUrl": "https://..."} — where to fetch it from, which
//     only works for URLs under the worker's SCENARIO_SOURCES allowlist
//
// Either way the scenario is saved into a fresh task workspace as scenario.yaml
// and `resonate run` starts against it in a background goroutine. It returns
// immediately with the taskId; the caller polls GET /task/{id}. A URL is fetched
// before the response, so a bad or unreachable URL is a 4xx/5xx here rather than
// a mysteriously failed task later.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var (
		body   io.Reader
		name   string
		source string
		data   []*multipart.FileHeader
	)
	if isJSON(r) {
		var req struct {
			ScenarioURL string `json:"scenarioUrl"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			httpError(w, http.StatusBadRequest, fmt.Errorf(`want a JSON body like {"scenarioUrl": "https://..."}: %w`, err))
			return
		}
		if req.ScenarioURL == "" {
			httpError(w, http.StatusBadRequest, fmt.Errorf(`missing "scenarioUrl"`))
			return
		}
		rc, err := s.Fetcher.Fetch(req.ScenarioURL)
		if err != nil {
			httpError(w, fetchStatus(err), err)
			return
		}
		defer rc.Close()
		body, name, source = rc, scenarioName(req.ScenarioURL), req.ScenarioURL
	} else {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			httpError(w, http.StatusBadRequest, fmt.Errorf("parsing upload: %w", err))
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			httpError(w, http.StatusBadRequest, fmt.Errorf(`missing "file" field with the scenario YAML (or send JSON with "scenarioUrl"): %w`, err))
			return
		}
		defer file.Close()
		body, name = file, header.Filename
		data = r.MultipartForm.File["files"]
		if len(data) > maxDataFiles {
			httpError(w, http.StatusBadRequest, fmt.Errorf("too many data files: %d (max %d)", len(data), maxDataFiles))
			return
		}
		for _, h := range data {
			if _, err := dataFileName(h.Filename); err != nil {
				httpError(w, http.StatusBadRequest, err)
				return
			}
		}
	}

	id, err := newTaskID()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	dir := filepath.Join(s.Workspace, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Errorf("creating task workspace: %w", err))
		return
	}

	scenarioPath := filepath.Join(dir, "scenario.yaml")
	sum, err := saveScenario(scenarioPath, body)
	if err == nil {
		err = checkSavedScenario(scenarioPath)
	}
	if err == nil {
		err = saveDataFiles(dir, data)
	}
	if err != nil {
		os.RemoveAll(dir)
		var bad badRequest
		if errors.As(err, &bad) {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		if errors.Is(err, errDataTooLarge) {
			httpError(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		if errors.Is(err, errSourceTooLarge) {
			httpError(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		def := http.StatusInternalServerError
		if source != "" {
			def = http.StatusBadGateway // the read that failed was from the remote source
		}
		httpError(w, fetchStatusOr(err, def), fmt.Errorf("saving scenario: %w", err))
		return
	}

	t := &Task{
		ID:        id,
		Dir:       dir,
		Scenario:  name,
		Source:    source,
		SHA256:    sum,
		CreatedAt: time.Now(),
		Status:    StatusQueued,
	}
	s.Store.Add(t)
	t.save()

	if source != "" {
		log.Printf("task %s: submitted (scenario=%q, source=%s, sha256=%s, from=%s)", id, name, source, sum[:12], r.RemoteAddr)
	} else {
		log.Printf("task %s: submitted (scenario=%q, from=%s)", id, name, r.RemoteAddr)
	}
	go s.Runner.Run(t)

	writeJSON(w, http.StatusAccepted, map[string]string{"taskId": id})
}

// badRequest marks an error caused by the submission itself.
type badRequest struct{ error }

func (b badRequest) Unwrap() error { return b.error }

// checkSavedScenario rejects a saved scenario that reads files outside its workspace.
func checkSavedScenario(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := checkScenarioPaths(b); err != nil {
		return badRequest{err}
	}
	return nil
}

// saveDataFiles writes the uploaded data files into the task workspace.
func saveDataFiles(dir string, files []*multipart.FileHeader) error {
	budget := int64(maxDataTotal)
	for _, h := range files {
		f, err := h.Open()
		if err != nil {
			return err
		}
		n, err := saveDataFile(dir, h.Filename, f, budget)
		f.Close()
		if err != nil {
			if strings.HasPrefix(err.Error(), "duplicate data file") {
				return badRequest{err}
			}
			return err
		}
		budget -= n
	}
	return nil
}

func isJSON(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return mt == "application/json"
}

// saveScenario copies src to path and returns the SHA-256 of what was written,
// so a task records exactly which bytes it ran.
func saveScenario(path string, src io.Reader) (string, error) {
	dst, err := os.Create(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(dst, h), src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchStatus maps a SourceFetcher error to the HTTP status that best describes
// it: the caller's mistake (4xx) versus the source being down (502).
func fetchStatus(err error) int {
	return fetchStatusOr(err, http.StatusBadGateway)
}

func fetchStatusOr(err error, def int) int {
	switch {
	case errors.Is(err, errSourceDisabled), errors.Is(err, errSourceNotAllowed):
		return http.StatusForbidden
	case errors.Is(err, errSourceBadURL):
		return http.StatusBadRequest
	case errors.Is(err, errSourceTooLarge):
		return http.StatusRequestEntityTooLarge
	}
	return def
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	tasks := s.Store.List()
	out := make([]snapshot, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.snapshot())
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	t, ok := s.Store.Get(r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusNotFound, fmt.Errorf("no such task"))
		return
	}
	writeJSON(w, http.StatusOK, t.snapshot())
}

// handleFile serves one file out of a task's workspace by its fixed name
// (report.html, report.json, results.jsonl, log.txt) — never a caller-supplied
// path, so there's no path-traversal surface here.
func (s *Server) handleFile(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, ok := s.Store.Get(r.PathValue("id"))
		if !ok {
			httpError(w, http.StatusNotFound, fmt.Errorf("no such task"))
			return
		}
		path := filepath.Join(t.Dir, name)
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				httpError(w, http.StatusNotFound, fmt.Errorf("%s isn't written yet (task is %s)", name, t.snapshot().Status))
				return
			}
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", contentType)
		io.Copy(w, f)
	}
}

func newTaskID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating task id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// withAuth requires "Authorization: Bearer <token>" on every request when
// token is non-empty. /healthz is always open, for load balancer health
// checks. token == "" disables auth entirely (local/dev use).
func withAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		// Plain equality, not constant-time: this guards a load-test trigger, not
		// a high-value secret, so the timing side channel isn't worth the extra
		// code. Revisit if this ever guards something more sensitive.
		if r.Header.Get("Authorization") != want {
			httpError(w, http.StatusUnauthorized, fmt.Errorf("missing or bad Authorization header"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusWriter records the status code a handler wrote, for request logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// withRequestLog logs one line per request: method, path, status, duration and
// client address. It never logs headers, so the bearer token stays out of the
// log. Wrap it outside withAuth so rejected (401) requests are logged too.
func withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s -> %d (%s) from %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond), r.RemoteAddr)
	})
}
