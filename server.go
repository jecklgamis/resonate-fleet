package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Server wires the Store and Runner to HTTP handlers. It holds no other state.
type Server struct {
	Store     *Store
	Runner    *Runner
	Workspace string // base directory; each task gets Workspace/<taskID>/
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

// handleSubmit accepts a multipart upload with one field, "file" — the
// scenario YAML — writes it to a fresh task workspace as scenario.yaml, and
// starts `resonate run` against it in a background goroutine. It returns
// immediately with the taskId; the caller polls GET /task/{id}.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("parsing upload: %w", err))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf(`missing "file" field with the scenario YAML: %w`, err))
		return
	}
	defer file.Close()

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

	dst, err := os.Create(filepath.Join(dir, "scenario.yaml"))
	if err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Errorf("saving scenario: %w", err))
		return
	}
	if _, err := io.Copy(dst, file); err != nil {
		dst.Close()
		httpError(w, http.StatusInternalServerError, fmt.Errorf("saving scenario: %w", err))
		return
	}
	dst.Close()

	t := &Task{
		ID:        id,
		Dir:       dir,
		Scenario:  header.Filename,
		CreatedAt: time.Now(),
		Status:    StatusQueued,
	}
	s.Store.Add(t)

	go s.Runner.Run(t)

	writeJSON(w, http.StatusAccepted, map[string]string{"taskId": id})
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
