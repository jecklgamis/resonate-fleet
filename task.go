package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Status is a task's lifecycle state.
type Status string

const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
)

// Task is one submitted scenario run: its own workspace directory (holding the
// scenario file, resonate's report.html/report.json/results.jsonl, and a log of
// resonate's stdout+stderr), and its current status.
//
// Only Status/ExitCode/Err/FinishedAt are mutated after creation, guarded by mu —
// everything else is set once at creation and read-only afterward.
type Task struct {
	ID        string
	Dir       string
	Scenario  string // the uploaded file's original name, for display only
	CreatedAt time.Time

	mu         sync.Mutex
	Status     Status
	ExitCode   int
	Err        string // resonate's own failure reason (e.g. "assertion failed"), if any
	FinishedAt time.Time
}

func (t *Task) setRunning() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Status = StatusRunning
}

func (t *Task) finish(exitCode int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ExitCode = exitCode
	t.FinishedAt = time.Now()
	if exitCode == 0 {
		t.Status = StatusDone
	} else {
		t.Status = StatusFailed
		if err != nil {
			t.Err = err.Error()
		}
	}
}

// snapshot is Task's data under lock, safe to hand to a JSON encoder without
// racing a concurrent finish().
type snapshot struct {
	ID         string    `json:"id"`
	Scenario   string    `json:"scenario"`
	Status     Status    `json:"status"`
	ExitCode   int       `json:"exit_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

func (t *Task) snapshot() snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := snapshot{ID: t.ID, Scenario: t.Scenario, Status: t.Status, ExitCode: t.ExitCode, Error: t.Err, CreatedAt: t.CreatedAt}
	if !t.FinishedAt.IsZero() {
		s.FinishedAt = t.FinishedAt
	}
	return s
}

// Store holds every task this worker has run, in memory. Restarting the worker
// loses task history — the workspace directories on disk survive, but there is
// nothing yet to re-index them. Good enough for a single long-lived worker
// process; a restart-safe store is future work if this is kept running long-term.
type Store struct {
	mu    sync.RWMutex
	tasks map[string]*Task
}

func NewStore() *Store { return &Store{tasks: make(map[string]*Task)} }

func (s *Store) Add(t *Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.ID] = t
}

func (s *Store) Get(id string) (*Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	return t, ok
}

func (s *Store) List() []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, t)
	}
	return out
}

// Runner executes resonate against a task's scenario file as a subprocess.
type Runner struct {
	ResonateBin string // path to the resonate binary, e.g. from $RESONATE_BIN or "resonate" on $PATH
}

// Run blocks until resonate exits. It is meant to be called in its own
// goroutine per task — concurrent tasks run as concurrent subprocesses, with no
// cap in this minimal version (see README's "Known limitations").
func (r *Runner) Run(t *Task) {
	t.setRunning()
	log.Printf("task %s: running %s run scenario.yaml", t.ID, r.ResonateBin)

	logPath := filepath.Join(t.Dir, "log.txt")
	logFile, err := os.Create(logPath)
	if err != nil {
		log.Printf("task %s: failed: creating log file: %v", t.ID, err)
		t.finish(1, fmt.Errorf("creating log file: %w", err))
		return
	}
	defer logFile.Close()

	cmd := exec.Command(r.ResonateBin, "run", "scenario.yaml",
		"--quiet",
		"--html-report", "report.html",
		"--json-report", "report.json",
		"--results-file", "results.jsonl",
	)
	cmd.Dir = t.Dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	runErr := cmd.Run()
	exitCode := 0
	if runErr != nil {
		exitCode = 1
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		}
	}
	t.finish(exitCode, runErr)
	if exitCode == 0 {
		log.Printf("task %s: done in %s", t.ID, time.Since(t.CreatedAt).Round(time.Millisecond))
	} else {
		log.Printf("task %s: failed (exit %d, err=%v) after %s; see %s", t.ID, exitCode, runErr, time.Since(t.CreatedAt).Round(time.Millisecond), logPath)
	}
}
