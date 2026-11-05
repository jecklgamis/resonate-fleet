package main

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

const maxScenarioBytes = 1 << 20

// Server serves the controller API: worker registration, and runs fanned out
// across the registered workers.
type Server struct {
	Registry *Registry
	Runs     *Runs
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /workers", s.handleRegister)
	mux.HandleFunc("GET /workers", s.handleWorkers)
	mux.HandleFunc("DELETE /workers/{name}", s.handleDeregister)
	mux.HandleFunc("POST /runs", s.handleStartRun)
	mux.HandleFunc("GET /runs/{id}", s.handleRun)
	mux.HandleFunc("GET /runs/{id}/{worker}/{file}", s.handleReport)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string            `json:"name"`
		URL    string            `json:"url"`
		Token  string            `json:"token"`
		Labels map[string]string `json:"labels"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("want a JSON body {name, url, token, labels}: %w", err))
		return
	}
	if err := s.Registry.Register(Entry{Name: req.Name, URL: req.URL, Token: req.Token, Labels: req.Labels}); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeregister(w http.ResponseWriter, r *http.Request) {
	s.Registry.Remove(r.PathValue("name"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	sel, err := parseSelector(r.URL.Query()["label"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	live := s.Registry.Live(sel)
	if live == nil {
		live = []Entry{}
	}
	writeJSON(w, http.StatusOK, live)
}

// handleStartRun starts a scenario on every live worker (or those matching
// ?label=key=value, repeatable). Like the worker's POST /task, the body is a
// multipart upload ("file", plus optional "files" data files) or JSON
// {"scenarioUrl": ...}.
func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	sel, err := parseSelector(r.URL.Query()["label"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	spec, status, err := readSpec(r)
	if err != nil {
		writeError(w, status, err)
		return
	}
	workers := s.Registry.Live(sel)
	if len(workers) == 0 {
		writeError(w, http.StatusConflict, fmt.Errorf("no live workers match"))
		return
	}
	run, err := s.Runs.Start(spec, workers)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	names := make([]string, len(workers))
	for i, e := range workers {
		names[i] = e.Name
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"runId": run.ID, "workers": names})
}

func readSpec(r *http.Request) (scenarioSpec, int, error) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "application/json" {
		var req struct {
			ScenarioURL string `json:"scenarioUrl"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil || req.ScenarioURL == "" {
			return scenarioSpec{}, http.StatusBadRequest, fmt.Errorf(`want a JSON body like {"scenarioUrl": "https://..."}`)
		}
		name := req.ScenarioURL[strings.LastIndex(req.ScenarioURL, "/")+1:]
		return scenarioSpec{Name: name, URL: req.ScenarioURL}, 0, nil
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return scenarioSpec{}, http.StatusBadRequest, fmt.Errorf("parsing upload: %w", err)
	}
	f, h, err := r.FormFile("file")
	if err != nil {
		return scenarioSpec{}, http.StatusBadRequest, fmt.Errorf(`missing "file" field with the scenario YAML (or send JSON with "scenarioUrl")`)
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxScenarioBytes+1))
	if err != nil {
		return scenarioSpec{}, http.StatusBadRequest, err
	}
	if len(body) > maxScenarioBytes {
		return scenarioSpec{}, http.StatusRequestEntityTooLarge, fmt.Errorf("scenario is larger than %d bytes", maxScenarioBytes)
	}
	spec := scenarioSpec{Name: h.Filename, Body: body}
	for _, fh := range r.MultipartForm.File["files"] {
		df, err := fh.Open()
		if err != nil {
			return scenarioSpec{}, http.StatusBadRequest, err
		}
		b, err := io.ReadAll(df)
		df.Close()
		if err != nil {
			return scenarioSpec{}, http.StatusBadRequest, err
		}
		spec.Data = append(spec.Data, dataFile{Name: fh.Filename, Body: b})
	}
	return spec, 0, nil
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.Runs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no such run"))
		return
	}
	writeJSON(w, http.StatusOK, run.snapshot())
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	run, ok := s.Runs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no such run"))
		return
	}
	file := r.PathValue("file")
	if _, ok := reportFiles[file]; !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no such file %q: want report.html, report.json, results.jsonl or log", file))
		return
	}
	wr, ok := run.find(r.PathValue("worker"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no such worker in this run"))
		return
	}
	if wr.TaskID == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("worker %s has no task (%s)", wr.Name, wr.Status))
		return
	}
	s.Runs.Proxy(r.Context(), w, wr, file)
}

func parseSelector(pairs []string) (map[string]string, error) {
	sel := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid label %q: want key=value", p)
		}
		sel[k] = v
	}
	return sel, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// withAuth requires "Authorization: Bearer <token>" on everything but /healthz
// when token is non-empty.
func withAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.Header.Get("Authorization") != want {
			writeError(w, http.StatusUnauthorized, fmt.Errorf("missing or bad Authorization header"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func envDuration(d time.Duration, v string) time.Duration {
	if x, err := time.ParseDuration(v); err == nil && x > 0 {
		return x
	}
	return d
}
