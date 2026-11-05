package main

import (
	"encoding/json"
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
	Source    string // URL the scenario was fetched from, if it was submitted by URL
	SHA256    string // hash of the scenario bytes that were saved and run
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
	t.saveLocked()
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
	t.saveLocked()
}

// taskFile is the per-task state file inside the task's workspace directory.
const taskFile = "task.json"

// save persists the task's current state to Dir/task.json so the task list
// survives a worker restart. It writes a temp file and renames it into place,
// so a crash mid-write never leaves a truncated task.json. A failure is
// logged, not fatal: the in-memory state is still correct, and losing one
// status write only matters if the worker also restarts.
func (t *Task) save() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.saveLocked()
}

// saveLocked is save for a caller already holding t.mu. State changes call it
// before releasing the lock, so a new status is never visible to a poll (or a
// restart) before it is on disk.
func (t *Task) saveLocked() {
	b, err := json.Marshal(t.snapshotLocked())
	if err != nil {
		log.Printf("task %s: encoding state: %v", t.ID, err)
		return
	}
	tmp := filepath.Join(t.Dir, taskFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("task %s: saving state: %v", t.ID, err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(t.Dir, taskFile)); err != nil {
		log.Printf("task %s: saving state: %v", t.ID, err)
	}
}

// snapshot is Task's data under lock, safe to hand to a JSON encoder without
// racing a concurrent finish().
type snapshot struct {
	ID         string    `json:"id"`
	Scenario   string    `json:"scenario"`
	Source     string    `json:"source,omitempty"`
	SHA256     string    `json:"sha256,omitempty"`
	Status     Status    `json:"status"`
	ExitCode   int       `json:"exit_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

func (t *Task) snapshot() snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

func (t *Task) snapshotLocked() snapshot {
	s := snapshot{ID: t.ID, Scenario: t.Scenario, Source: t.Source, SHA256: t.SHA256, Status: t.Status, ExitCode: t.ExitCode, Error: t.Err, CreatedAt: t.CreatedAt}
	if !t.FinishedAt.IsZero() {
		s.FinishedAt = t.FinishedAt
	}
	return s
}

// Store holds every task this worker knows about, in memory. Each task is also
// persisted as Dir/task.json (see Task.save) and LoadStore rebuilds the Store
// from those files at startup.
type Store struct {
	mu    sync.RWMutex
	tasks map[string]*Task
}

func NewStore() *Store { return &Store{tasks: make(map[string]*Task)} }

// LoadStore rebuilds a Store from the task.json files under workspace, one
// per task subdirectory. Subdirectories without a readable task.json are
// skipped. A task that was queued or running when the previous worker process
// died has no subprocess any more, so it is marked failed rather than left
// "running" forever.
func LoadStore(workspace string) (*Store, error) {
	s := NewStore()
	entries, err := os.ReadDir(workspace)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(workspace, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, taskFile))
		if err != nil {
			continue // not a task directory, or from before persistence existed
		}
		var rec snapshot
		if err := json.Unmarshal(b, &rec); err != nil || rec.ID != e.Name() {
			log.Printf("skipping %s: unreadable or mismatched %s", dir, taskFile)
			continue
		}
		t := &Task{
			ID:         rec.ID,
			Dir:        dir,
			Scenario:   rec.Scenario,
			Source:     rec.Source,
			SHA256:     rec.SHA256,
			CreatedAt:  rec.CreatedAt,
			Status:     rec.Status,
			ExitCode:   rec.ExitCode,
			Err:        rec.Error,
			FinishedAt: rec.FinishedAt,
		}
		if t.Status == StatusQueued || t.Status == StatusRunning {
			t.Status = StatusFailed
			t.ExitCode = 1
			t.Err = "worker restarted before the task finished"
			t.FinishedAt = time.Now()
			t.save()
			log.Printf("task %s: was %s when the worker stopped; marked failed", t.ID, rec.Status)
		}
		s.Add(t)
	}
	return s, nil
}

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
