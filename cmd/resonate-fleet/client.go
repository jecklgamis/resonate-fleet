package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// taskSnapshot mirrors resonate-worker's GET /task/{id} JSON shape. Kept as
// its own copy rather than imported, since resonate-worker's own types live
// in package main there and aren't importable.
type taskSnapshot struct {
	ID         string    `json:"id"`
	Scenario   string    `json:"scenario"`
	Status     string    `json:"status"`
	ExitCode   int       `json:"exit_code"`
	Error      string    `json:"error"`
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"finished_at"`
}

func (s taskSnapshot) finished() bool { return s.Status == "done" || s.Status == "failed" }

// WorkerClient talks to one resonate-worker's HTTP API.
type WorkerClient struct {
	Worker Worker
	HTTP   *http.Client
}

func newWorkerClient(w Worker) *WorkerClient {
	return &WorkerClient{Worker: w, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *WorkerClient) authorize(r *http.Request) {
	if c.Worker.Token != "" {
		r.Header.Set("Authorization", "Bearer "+c.Worker.Token)
	}
}

// isScenarioURL reports whether the scenario argument is a URL for the workers
// to fetch themselves, rather than a local file to upload.
func isScenarioURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// Submit sends the scenario and returns the worker's taskId. A URL is passed to
// the worker as {"scenarioUrl": ...} for it to fetch (it must be under that
// worker's SCENARIO_SOURCES); anything else is uploaded from the local file.
func (c *WorkerClient) Submit(ctx context.Context, scenarioPath string) (string, error) {
	if isScenarioURL(scenarioPath) {
		b, _ := json.Marshal(map[string]string{"scenarioUrl": scenarioPath})
		return c.post(ctx, "application/json", bytes.NewReader(b))
	}
	f, err := os.Open(scenarioPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filepath.Base(scenarioPath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	return c.post(ctx, mw.FormDataContentType(), &buf)
}

// post sends a submission body to POST /task and returns the taskId.
func (c *WorkerClient) post(ctx context.Context, contentType string, body io.Reader) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Worker.URL+"/task", body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	c.authorize(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("submitting to %s: %w", c.Worker.URL, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("submitting to %s: %s: %s", c.Worker.URL, resp.Status, strings.TrimSpace(string(respBody)))
	}

	var out struct {
		TaskID string `json:"taskId"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("parsing submit response from %s: %w", c.Worker.URL, err)
	}
	if out.TaskID == "" {
		return "", fmt.Errorf("%s returned no taskId", c.Worker.URL)
	}
	return out.TaskID, nil
}

// Poll blocks, checking the task's status every interval, until it reaches
// "done" or "failed", or ctx is done (deadline or cancellation — e.g. Ctrl+C).
func (c *WorkerClient) Poll(ctx context.Context, taskID string, interval time.Duration) (taskSnapshot, error) {
	for {
		s, err := c.status(ctx, taskID)
		if err != nil {
			return s, err
		}
		if s.finished() {
			return s, nil
		}
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (c *WorkerClient) status(ctx context.Context, taskID string) (taskSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Worker.URL+"/task/"+taskID, nil)
	if err != nil {
		return taskSnapshot{}, err
	}
	c.authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return taskSnapshot{}, fmt.Errorf("polling %s: %w", c.Worker.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return taskSnapshot{}, fmt.Errorf("polling %s: %s: %s", c.Worker.URL, resp.Status, strings.TrimSpace(string(body)))
	}
	var s taskSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return taskSnapshot{}, fmt.Errorf("parsing status from %s: %w", c.Worker.URL, err)
	}
	return s, nil
}

// FetchFile downloads one of a task's files (report.html, report.json,
// results.jsonl, log) to destPath. A 404 (the file genuinely doesn't exist —
// e.g. a task that never got far enough to write results.jsonl) is reported
// via the returned error rather than treated as fatal by the caller, which
// decides whether that's expected.
func (c *WorkerClient) FetchFile(ctx context.Context, taskID, name, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Worker.URL+"/task/"+taskID+"/"+name, nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("fetching %s from %s: %w", name, c.Worker.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fetching %s from %s: %s: %s", name, c.Worker.URL, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}
