package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"
)

// dataFile is an uploaded feeder/body file, forwarded to every worker as-is.
type dataFile struct {
	Name string
	Body []byte
}

// scenarioSpec is what a run executes on each worker: scenario bytes (plus data
// files) uploaded to it, or a URL for the worker to fetch itself.
type scenarioSpec struct {
	Name string
	Body []byte
	URL  string
	Data []dataFile
}

// WorkerRun is one worker's slice of a run.
type WorkerRun struct {
	Name     string `json:"name"`
	TaskID   string `json:"task_id,omitempty"`
	Status   string `json:"status"` // submitting, queued, running, done, failed, or error (controller-side)
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`

	url, token string
}

// Run is one scenario fanned out to the workers that were live when it started.
// Workers run independently; nothing is combined across them.
type Run struct {
	ID        string
	Scenario  string
	CreatedAt time.Time

	mu      sync.Mutex
	workers []*WorkerRun
	aborted bool // Abort was called; a worker still submitting is canceled once it has a task
}

type runSnapshot struct {
	ID        string       `json:"id"`
	Scenario  string       `json:"scenario"`
	Status    string       `json:"status"` // running, done, failed, or canceled
	CreatedAt time.Time    `json:"created_at"`
	Workers   []*WorkerRun `json:"workers"`
}

func finished(status string) bool {
	return status == "done" || status == "failed" || status == "error" || status == "canceled"
}

func (r *Run) snapshot() runSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := runSnapshot{ID: r.ID, Scenario: r.Scenario, CreatedAt: r.CreatedAt, Status: "done"}
	for _, w := range r.workers {
		cp := *w
		s.Workers = append(s.Workers, &cp)
		switch {
		case !finished(w.Status):
			s.Status = "running"
		case w.Status != "done" && s.Status != "running":
			s.Status = "failed"
		}
	}
	if s.Status != "running" && r.aborted {
		s.Status = "canceled"
	}
	return s
}

func (r *Run) update(w *WorkerRun, f func(*WorkerRun)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(w)
}

func (r *Run) isAborted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.aborted
}

func (r *Run) find(name string) (WorkerRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.workers {
		if w.Name == name {
			return *w, true
		}
	}
	return WorkerRun{}, false
}

// Runs holds every run this controller started, in memory.
type Runs struct {
	PollInterval time.Duration
	Timeout      time.Duration // per-worker cap on submit + poll
	HTTP         *http.Client

	mu sync.RWMutex
	m  map[string]*Run
}

func NewRuns(poll, timeout time.Duration) *Runs {
	return &Runs{PollInterval: poll, Timeout: timeout, HTTP: &http.Client{Timeout: 30 * time.Second}, m: map[string]*Run{}}
}

func (rs *Runs) Get(id string) (*Run, bool) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	r, ok := rs.m[id]
	return r, ok
}

// Start fans spec out to workers in the background and returns immediately.
func (rs *Runs) Start(spec scenarioSpec, workers []Entry) (*Run, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	run := &Run{ID: hex.EncodeToString(b), Scenario: spec.Name, CreatedAt: time.Now()}
	for _, e := range workers {
		run.workers = append(run.workers, &WorkerRun{Name: e.Name, Status: "submitting", url: e.URL, token: e.Token})
	}
	rs.mu.Lock()
	rs.m[run.ID] = run
	rs.mu.Unlock()

	for _, w := range run.workers {
		go rs.runOne(run, w, spec)
	}
	return run, nil
}

func (rs *Runs) runOne(run *Run, w *WorkerRun, spec scenarioSpec) {
	ctx, cancel := context.WithTimeout(context.Background(), rs.Timeout)
	defer cancel()
	fail := func(err error) { run.update(w, func(w *WorkerRun) { w.Status, w.Error = "error", err.Error() }) }

	id, err := rs.submit(ctx, w, spec)
	if err != nil {
		fail(err)
		return
	}
	run.update(w, func(w *WorkerRun) { w.TaskID, w.Status = id, "queued" })
	if run.isAborted() {
		rs.cancelWorker(w, id) // the abort came while this worker was still submitting
	}

	for {
		st, err := rs.status(ctx, w, id)
		if err != nil {
			fail(err)
			return
		}
		run.update(w, func(w *WorkerRun) { w.Status, w.ExitCode, w.Error = st.Status, st.ExitCode, st.Error })
		if finished(st.Status) {
			return
		}
		select {
		case <-ctx.Done():
			fail(ctx.Err())
			return
		case <-time.After(rs.PollInterval):
		}
	}
}

// Abort stops a run: every worker with a task is told to cancel it, and a worker
// still submitting is canceled as soon as its task exists. It returns the names of
// the workers that were asked to stop, and does nothing to a run that has finished.
func (rs *Runs) Abort(run *Run) []string {
	run.mu.Lock()
	if run.aborted {
		run.mu.Unlock()
		return nil
	}
	var todo []WorkerRun
	for _, w := range run.workers {
		if !finished(w.Status) {
			todo = append(todo, *w)
		}
	}
	if len(todo) == 0 {
		run.mu.Unlock()
		return nil
	}
	run.aborted = true
	run.mu.Unlock()

	var names []string
	for i := range todo {
		names = append(names, todo[i].Name)
		if todo[i].TaskID != "" {
			rs.cancelWorker(&todo[i], todo[i].TaskID)
		}
	}
	return names
}

// cancelWorker tells one worker to stop a task. The worker's own status (polled
// by runOne) is what records the outcome, so a failure here is only logged.
func (rs *Runs) cancelWorker(w *WorkerRun, taskID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := rs.do(ctx, w, http.MethodDelete, "/task/"+taskID, "", nil)
	if err != nil {
		log.Printf("abort: %v", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		log.Printf("abort: %s: cancel task %s: %s", w.Name, taskID, resp.Status)
	}
}

func (rs *Runs) do(ctx context.Context, w *WorkerRun, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.url+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	resp, err := rs.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", w.Name, err)
	}
	return resp, nil
}

func (rs *Runs) submit(ctx context.Context, w *WorkerRun, spec scenarioSpec) (string, error) {
	var (
		body io.Reader
		ct   string
	)
	if spec.URL != "" {
		j, _ := json.Marshal(map[string]string{"scenarioUrl": spec.URL})
		body, ct = bytes.NewReader(j), "application/json"
	} else {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		p, _ := mw.CreateFormFile("file", spec.Name)
		p.Write(spec.Body)
		for _, d := range spec.Data {
			p, _ := mw.CreateFormFile("files", d.Name)
			p.Write(d.Body)
		}
		mw.Close()
		body, ct = &buf, mw.FormDataContentType()
	}
	resp, err := rs.do(ctx, w, http.MethodPost, "/task", ct, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("%s: submit: %s: %s", w.Name, resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		TaskID string `json:"taskId"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.TaskID == "" {
		return "", fmt.Errorf("%s: submit: no taskId in response", w.Name)
	}
	return out.TaskID, nil
}

type taskStatus struct {
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error"`
}

func (rs *Runs) status(ctx context.Context, w *WorkerRun, id string) (taskStatus, error) {
	resp, err := rs.do(ctx, w, http.MethodGet, "/task/"+id, "", nil)
	if err != nil {
		return taskStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return taskStatus{}, fmt.Errorf("%s: poll: %s: %s", w.Name, resp.Status, strings.TrimSpace(string(b)))
	}
	var st taskStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return taskStatus{}, fmt.Errorf("%s: poll: %w", w.Name, err)
	}
	return st, nil
}

// reportFiles maps the controller's file names to the worker's.
var reportFiles = map[string]string{
	"report.html": "report.html", "report.json": "report.json",
	"results.jsonl": "results.jsonl", "log": "log",
}

// Proxy streams one report file of a worker's task to w.
func (rs *Runs) Proxy(ctx context.Context, out http.ResponseWriter, wr WorkerRun, file string) {
	resp, err := rs.do(ctx, &wr, http.MethodGet, "/task/"+wr.TaskID+"/"+reportFiles[file], "", nil)
	if err != nil {
		writeError(out, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		out.Header().Set("Content-Type", ct)
	}
	out.WriteHeader(resp.StatusCode)
	io.Copy(out, resp.Body)
}
