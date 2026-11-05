package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ControllerClient runs a scenario through a resonate-controller, which fans it
// out to the workers registered with it. The caller needs a route to the
// controller only, never to the workers.
type ControllerClient struct {
	URL   string
	Token string
	HTTP  *http.Client
}

func newControllerClient(u, token string) *ControllerClient {
	return &ControllerClient{URL: strings.TrimRight(u, "/"), Token: token, HTTP: &http.Client{Timeout: 5 * time.Minute}}
}

// controllerRun mirrors the controller's GET /runs/{id} JSON.
type controllerRun struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Workers []struct {
		Name     string `json:"name"`
		TaskID   string `json:"task_id"`
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
		Error    string `json:"error"`
	} `json:"workers"`
}

func (c *ControllerClient) do(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("controller %s: %w", c.URL, err)
	}
	return resp, nil
}

// Start submits the scenario (a local file with optional data files, or a URL
// for the workers to fetch) to workers matching labels, and returns the run id.
func (c *ControllerClient) Start(ctx context.Context, scenarioPath string, dataFiles, labels []string) (string, error) {
	var (
		body io.Reader
		ct   string
	)
	if isScenarioURL(scenarioPath) {
		if len(dataFiles) > 0 {
			return "", fmt.Errorf("--data can't be combined with a scenario URL")
		}
		b, _ := json.Marshal(map[string]string{"scenarioUrl": scenarioPath})
		body, ct = bytes.NewReader(b), "application/json"
	} else {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if err := addFile(mw, "file", scenarioPath); err != nil {
			return "", err
		}
		for _, d := range dataFiles {
			if err := addFile(mw, "files", d); err != nil {
				return "", err
			}
		}
		mw.Close()
		body, ct = &buf, mw.FormDataContentType()
	}
	q := url.Values{}
	for _, l := range labels {
		q.Add("label", l)
	}
	path := "/runs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	resp, err := c.do(ctx, http.MethodPost, path, ct, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("starting run: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.RunID == "" {
		return "", fmt.Errorf("controller returned no runId")
	}
	return out.RunID, nil
}

func addFile(mw *multipart.Writer, field, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	p, err := mw.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return err
	}
	_, err = io.Copy(p, f)
	return err
}

// Wait polls the run until it is no longer "running".
func (c *ControllerClient) Wait(ctx context.Context, id string, interval time.Duration) (controllerRun, error) {
	for {
		var run controllerRun
		resp, err := c.do(ctx, http.MethodGet, "/runs/"+id, "", nil)
		if err != nil {
			return run, err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return run, fmt.Errorf("polling run: %s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		if err := json.Unmarshal(b, &run); err != nil {
			return run, fmt.Errorf("parsing run status: %w", err)
		}
		if run.Status != "running" {
			return run, nil
		}
		select {
		case <-ctx.Done():
			return run, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// fetch downloads one report file of one worker, through the controller.
func (c *ControllerClient) fetch(ctx context.Context, runID, worker, name, dest string) error {
	resp, err := c.do(ctx, http.MethodGet, "/runs/"+runID+"/"+worker+"/"+name, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

// runViaController runs the scenario through the controller and returns one
// result per worker, with each worker's reports written under outDir/<worker>/.
func runViaController(ctx context.Context, c *ControllerClient, scenarioPath string, dataFiles, labels []string, outDir string, poll time.Duration) ([]workerResult, error) {
	id, err := c.Start(ctx, scenarioPath, dataFiles, labels)
	if err != nil {
		return nil, err
	}
	fmt.Printf("run %s started on the controller\n", id)
	run, err := c.Wait(ctx, id, poll)
	if err != nil {
		return nil, err
	}
	var results []workerResult
	for _, w := range run.Workers {
		res := workerResult{Worker: Worker{Name: w.Name}, TaskID: w.TaskID, Status: w.Status, ExitCode: w.ExitCode, Err: w.Error}
		if w.TaskID != "" {
			for _, name := range []string{"report.html", "report.json", "results.jsonl", "log"} {
				dest := filepath.Join(outDir, w.Name, name)
				if err := c.fetch(ctx, id, w.Name, name, dest); err != nil {
					fmt.Fprintf(os.Stderr, "warning: %s: %s: %v\n", w.Name, name, err)
					continue
				}
				if name == "report.json" {
					if b, err := os.ReadFile(dest); err == nil {
						var rs reportSummary
						if json.Unmarshal(b, &rs) == nil {
							res.Report = &rs
						}
					}
				}
			}
		}
		results = append(results, res)
	}
	return results, nil
}
