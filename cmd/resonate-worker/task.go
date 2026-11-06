package main

import (
	"context"
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

	// StatusCanceled is a task stopped on request (DELETE /task/{id}) while it ran.
	StatusCanceled Status = "canceled"
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
	StartAt   time.Time // when resonate is to start, if the submitter asked for a synchronized start

	mu         sync.Mutex
	Status     Status
	ExitCode   int
	Err        string // resonate's own failure reason (e.g. "assertion failed"), if any
	FinishedAt time.Time

	ArtifactsURL string // where the task's files were uploaded, if an ArtifactStore is configured
	UploadError  string // why the upload failed, if it did; the run's own status is unaffected

	live     *Live              // request tally for GET /task/{id}/live; nil until Run starts the subprocess
	cancel   context.CancelFunc // stops the running subprocess; nil until Run starts it
	canceled bool               // Cancel was called, so a non-zero exit is "canceled", not "failed"
}

// LiveStats is the task's request tally with its current status, for
// GET /task/{id}/live. A task that has not started has zeros.
func (t *Task) LiveStats(now time.Time) LiveStats {
	t.mu.Lock()
	live, status := t.live, t.Status
	t.mu.Unlock()
	st := live.Snapshot(now)
	st.Status = status
	return st
}

// Cancel asks a queued or running task to stop. It reports false when the task has
// already finished, so a repeat call is harmless.
func (t *Task) Cancel() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Status != StatusRunning && t.Status != StatusQueued {
		return false
	}
	// A task still waiting for its start time (or just submitted, before Run has
	// set cancel) is stopped by the canceled flag, which Run checks before starting.
	t.canceled = true
	if t.cancel != nil {
		t.cancel()
	}
	return true
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
	t.finishLocked(exitCode, err)
}

// finishUploaded is finish plus the outcome of the artifact upload, recorded in
// the same write so a poller never sees a done task whose upload is still pending.
func (t *Task) finishUploaded(exitCode int, err error, url string, uploadErr error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ArtifactsURL = url
	if uploadErr != nil {
		t.UploadError = uploadErr.Error()
	}
	t.finishLocked(exitCode, err)
}

func (t *Task) finishLocked(exitCode int, err error) {
	t.ExitCode = exitCode
	t.FinishedAt = time.Now()
	switch {
	case t.canceled:
		t.Status = StatusCanceled
		t.Err = "canceled"
	case exitCode == 0:
		t.Status = StatusDone
	default:
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
	ID         string     `json:"id"`
	Scenario   string     `json:"scenario"`
	Source     string     `json:"source,omitempty"`
	SHA256     string     `json:"sha256,omitempty"`
	Status     Status     `json:"status"`
	ExitCode   int        `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartAt    *time.Time `json:"start_at,omitempty"`
	FinishedAt time.Time  `json:"finished_at,omitempty"`

	ArtifactsURL string `json:"artifacts_url,omitempty"`
	UploadError  string `json:"upload_error,omitempty"`
}

func (t *Task) snapshot() snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

func (t *Task) snapshotLocked() snapshot {
	s := snapshot{ID: t.ID, Scenario: t.Scenario, Source: t.Source, SHA256: t.SHA256, Status: t.Status, ExitCode: t.ExitCode, Error: t.Err, CreatedAt: t.CreatedAt, ArtifactsURL: t.ArtifactsURL, UploadError: t.UploadError}
	if !t.FinishedAt.IsZero() {
		s.FinishedAt = t.FinishedAt
	}
	if !t.StartAt.IsZero() {
		st := t.StartAt
		s.StartAt = &st
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

			ArtifactsURL: rec.ArtifactsURL,
			UploadError:  rec.UploadError,
		}
		if rec.StartAt != nil {
			t.StartAt = *rec.StartAt
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

// cancelGrace is how long a canceled resonate gets to write its reports before
// it is killed.
const cancelGrace = 10 * time.Second

// Runner executes resonate against a task's scenario file as a subprocess.
type Runner struct {
	ResonateBin string // path to the resonate binary, e.g. from $RESONATE_BIN or "resonate" on $PATH

	// Artifacts, if set, receives the task's files once resonate has exited.
	Artifacts ArtifactStore

	// Metrics, if set, receives live request metrics while resonate runs.
	Metrics *Metrics
}

// Run blocks until resonate exits. It is meant to be called in its own
// goroutine per task — concurrent tasks run as concurrent subprocesses, with no
// cap in this minimal version (see README's "Known limitations").
func (r *Runner) Run(t *Task) {
	// The cancel func is in place before the task shows as running, so a cancel
	// that arrives the moment a poller sees "running" always finds it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.mu.Lock()
	t.cancel = cancel
	pre := t.canceled // canceled between submit and here
	t.mu.Unlock()

	// A synchronized start: sit queued until the requested time, so every worker of
	// a run starts together. A time already past starts at once.
	if !pre && !t.StartAt.IsZero() {
		if d := time.Until(t.StartAt); d > 0 {
			log.Printf("task %s: waiting %s to start at %s", t.ID, d.Round(time.Millisecond), t.StartAt.Format(time.RFC3339Nano))
			timer := time.NewTimer(d)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			}
		}
	}
	if ctx.Err() != nil || pre {
		t.finish(1, nil) // canceled before it started: finishLocked records "canceled"
		log.Printf("task %s: canceled before it started", t.ID)
		return
	}

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

	cmd := exec.CommandContext(ctx, r.ResonateBin, "run", "scenario.yaml",
		"--quiet",
		"--html-report", "report.html",
		"--json-report", "report.json",
		"--results-file", "results.jsonl",
	)
	// Ask resonate to stop (so it can still write its reports) and kill it if it
	// does not within a grace period. Windows has no interrupt, so it is killed.
	cmd.Cancel = func() error {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = cancelGrace
	cmd.Dir = t.Dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	// Tally the requests live, whether or not metrics are exported anywhere.
	live := NewLive(time.Now())
	t.mu.Lock()
	t.live = live
	t.mu.Unlock()
	liveCtx, stopLive := context.WithCancel(context.Background())
	liveDone := make(chan struct{})
	go func() {
		defer close(liveDone)
		tailResults(liveCtx, filepath.Join(t.Dir, "results.jsonl"), func(res resultLine) { live.Record(res, time.Now()) })
	}()

	stopMetrics := r.Metrics.TrackTask(filepath.Join(t.Dir, "results.jsonl"))
	runErr := cmd.Run()
	stopMetrics()
	stopLive()
	<-liveDone
	live.Finish(time.Now())
	exitCode := 0
	if runErr != nil {
		exitCode = 1
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		}
	}
	r.finish(t, exitCode, runErr)
	if st := t.snapshot().Status; st == StatusCanceled {
		log.Printf("task %s: canceled after %s", t.ID, time.Since(t.CreatedAt).Round(time.Millisecond))
	} else if exitCode == 0 {
		log.Printf("task %s: done in %s", t.ID, time.Since(t.CreatedAt).Round(time.Millisecond))
	} else {
		log.Printf("task %s: failed (exit %d, err=%v) after %s; see %s", t.ID, exitCode, runErr, time.Since(t.CreatedAt).Round(time.Millisecond), logPath)
	}
}

// finish uploads the task's artifacts (when a store is configured) and then
// records the final status. A failed upload is recorded on the task but does not
// change the run's outcome.
func (r *Runner) finish(t *Task, exitCode int, runErr error) {
	if r.Artifacts == nil {
		t.finish(exitCode, runErr)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	url, err := r.Artifacts.Upload(ctx, t.ID, t.Dir)
	if err != nil {
		log.Printf("task %s: artifact upload failed: %v", t.ID, err)
		url = ""
	} else {
		log.Printf("task %s: artifacts uploaded to %s", t.ID, url)
	}
	t.finishUploaded(exitCode, runErr, url, err)
}
