package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxScenarioBytes = 1 << 20

// maxStartDelay is the longest start_delay a run may ask for; the workers refuse a
// start time further out than this.
const maxStartDelay = 10 * time.Minute

// Server serves the controller API: worker registration, and runs fanned out
// across the registered workers.
type Server struct {
	Registry *Registry
	Runs     *Runs

	// Shuffle randomizes which workers a partial run picks. Nil means
	// math/rand/v2's Shuffle; tests set a fixed permutation.
	Shuffle func(n int, swap func(i, j int))
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", handleRoot)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /workers", s.handleRegister)
	mux.HandleFunc("GET /workers", s.handleWorkers)
	mux.HandleFunc("DELETE /workers/{name}", s.handleDeregister)
	mux.HandleFunc("POST /runs", s.handleStartRun)
	mux.HandleFunc("GET /runs/{id}", s.handleRun)
	mux.HandleFunc("DELETE /runs/{id}", s.handleAbort)
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
	outcome, err := s.Registry.Register(Entry{Name: req.Name, URL: req.URL, Token: req.Token, Labels: req.Labels})
	if err != nil {
		log.Printf("registration from %s rejected: %v", r.RemoteAddr, err)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// Heartbeats arrive every few seconds per worker; only log what changes.
	switch outcome {
	case Registered:
		log.Printf("worker registered: %s (%s)", describe(req.Name, req.URL, req.Labels), s.liveCount())
	case Changed:
		log.Printf("worker re-registered with changes: %s (%s)", describe(req.Name, req.URL, req.Labels), s.liveCount())
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeregister(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if e, ok := s.Registry.Remove(name); ok {
		log.Printf("worker deregistered: %s (%s)", describe(e.Name, e.URL, e.Labels), s.liveCount())
	} else {
		log.Printf("deregistration for unknown worker %q from %s", name, r.RemoteAddr)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) liveCount() string {
	return fmt.Sprintf("%d live", len(s.Registry.Live(nil)))
}

// describe renders a worker as name url=... labels=k=v,k=v for log lines.
func describe(name, url string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, len(keys))
	for i, k := range keys {
		pairs[i] = k + "=" + labels[k]
	}
	return fmt.Sprintf("%s url=%s labels=[%s]", name, url, strings.Join(pairs, ","))
}

// ReapLoop forgets workers that stopped heartbeating, logging each, until stop
// is closed.
func (s *Server) ReapLoop(every time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			for _, e := range s.Registry.Reap() {
				log.Printf("worker expired (no heartbeat for %s): %s (%s)", s.Registry.TTL, describe(e.Name, e.URL, e.Labels), s.liveCount())
			}
		}
	}
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
// ?label=key=value, repeatable), narrowed to a random subset by ?workers=4 or
// ?workers=25% (after the label filter). Like the worker's POST /task, the body is a
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
	live := s.Registry.Live(sel)
	if len(live) == 0 {
		writeError(w, http.StatusConflict, fmt.Errorf("no live workers match"))
		return
	}
	shuffle := s.Shuffle
	if shuffle == nil {
		shuffle = rand.Shuffle
	}
	workers, err := selectWorkers(live, r.URL.Query().Get("workers"), shuffle)
	if err != nil {
		status := http.StatusBadRequest
		if errors.As(err, &notEnoughLive{}) {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	var warnings []string
	totalRate := 0.0
	if q := r.URL.Query().Get("total_rate"); q != "" {
		v, err := strconv.ParseFloat(q, 64)
		if err != nil || !(v > 0) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid total_rate %q: want a number above 0", q))
			return
		}
		if spec.URL != "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("total_rate needs an uploaded scenario: the controller can't rewrite one the workers fetch themselves"))
			return
		}
		spec.Bodies, warnings, err = splitScenario(spec.Body, v, len(workers))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		totalRate = v
	}
	var startAt time.Time
	if q := r.URL.Query().Get("start_delay"); q != "" {
		d, err := time.ParseDuration(q)
		if err != nil || d < 0 || d > maxStartDelay {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid start_delay %q: want a duration from 0s to %s, like 5s", q, maxStartDelay))
			return
		}
		// One worker has nobody to line up with.
		if d > 0 && len(workers) > 1 {
			startAt = time.Now().Add(d)
			spec.StartAt = startAt
		}
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
	log.Printf("run %s started on %d of %d live workers", run.ID, len(workers), len(live))
	resp := map[string]any{"runId": run.ID, "workers": names, "available": len(live)}
	if totalRate > 0 {
		resp["totalRate"] = totalRate
		resp["warnings"] = warnings
	}
	if !startAt.IsZero() {
		resp["startAt"] = startAt.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusAccepted, resp)
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

// handleAbort stops a run's workers. It answers 202 with the workers it asked to
// stop, or 200 with none when the run had already finished (or was already aborted).
func (s *Server) handleAbort(w http.ResponseWriter, r *http.Request) {
	run, ok := s.Runs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("no such run"))
		return
	}
	names := s.Runs.Abort(run)
	if len(names) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"runId": run.ID, "aborted": []string{}})
		return
	}
	log.Printf("run %s aborted on %d worker(s)", run.ID, len(names))
	writeJSON(w, http.StatusAccepted, map[string]any{"runId": run.ID, "aborted": names})
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

// withAuth requires "Authorization: Bearer <token>" on everything but / and
// /healthz when token is non-empty.
func withAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/healthz" && r.Header.Get("Authorization") != want {
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

// handleRoot is a banner for anyone who opens the bare URL.
func handleRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"app": "resonate-controller", "message": "Relax, perf test it"})
}
