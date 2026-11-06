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
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
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

// isScenarioURL reports whether the scenario argument is a URL for the workers
// to fetch themselves (it must be under their SCENARIO_SOURCES), rather than a
// local file to upload.
func isScenarioURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
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
		Warning  string `json:"warning"`
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

// startOptions say which workers a run goes to and what goes with the scenario.
type startOptions struct {
	DataFiles []string // feeder/body files uploaded alongside the scenario
	Labels    []string // only workers with all of these key=value labels
	Workers   string   // "" for all of them, else a count ("4") or percentage ("25%") of those
	TotalRate float64  // 0 to run the scenario as written on every worker, else the fleet's peak req/s to split across them

	// StartDelay is how long from now the controller has every worker wait before
	// it starts, so they begin together. 0 starts each as soon as it has the scenario.
	StartDelay time.Duration
}

// startedRun is what the controller says it started.
type startedRun struct {
	ID        string
	Workers   []string // the workers chosen
	Available int      // how many were live and matched the labels

	// SelectionIgnored is true when a --workers selection was asked for but the
	// controller did not confirm it (an older controller ignores the parameter),
	// so the run started on every matching worker.
	SelectionIgnored bool

	// Warnings are the controller's notes on how it split the load.
	Warnings []string

	// TotalRateIgnored is true when a total rate was asked for but the controller
	// did not confirm it (an older controller ignores the parameter), so every
	// worker is running the scenario unsplit.
	TotalRateIgnored bool

	// StartAt is when the workers were told to start (zero if not synchronized), and
	// StartIgnored is true when a start delay was asked for, several workers were
	// chosen, and the controller did not confirm it (an older controller).
	StartAt      time.Time
	StartIgnored bool
}

// Start submits the scenario (a local file with optional data files, or a URL
// for the workers to fetch) to the workers chosen by opts.
func (c *ControllerClient) Start(ctx context.Context, scenarioPath string, opts startOptions) (startedRun, error) {
	dataFiles, labels := opts.DataFiles, opts.Labels
	var (
		body io.Reader
		ct   string
	)
	if isScenarioURL(scenarioPath) {
		if len(dataFiles) > 0 {
			return startedRun{}, fmt.Errorf("--data can't be combined with a scenario URL")
		}
		if opts.TotalRate > 0 {
			return startedRun{}, fmt.Errorf("--total-rate needs a scenario file: the controller can't rewrite a scenario the workers fetch themselves")
		}
		b, _ := json.Marshal(map[string]string{"scenarioUrl": scenarioPath})
		body, ct = bytes.NewReader(b), "application/json"
	} else {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if err := addFile(mw, "file", scenarioPath); err != nil {
			return startedRun{}, err
		}
		for _, d := range dataFiles {
			if err := addFile(mw, "files", d); err != nil {
				return startedRun{}, err
			}
		}
		mw.Close()
		body, ct = &buf, mw.FormDataContentType()
	}
	q := url.Values{}
	for _, l := range labels {
		q.Add("label", l)
	}
	if opts.Workers != "" {
		q.Set("workers", opts.Workers)
	}
	if opts.TotalRate > 0 {
		q.Set("total_rate", strconv.FormatFloat(opts.TotalRate, 'f', -1, 64))
	}
	if opts.StartDelay > 0 {
		q.Set("start_delay", opts.StartDelay.String())
	}
	path := "/runs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	resp, err := c.do(ctx, http.MethodPost, path, ct, body)
	if err != nil {
		return startedRun{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		return startedRun{}, fmt.Errorf("starting run: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		RunID     string   `json:"runId"`
		Workers   []string `json:"workers"`
		Available *int     `json:"available"` // absent from controllers that predate --workers
		TotalRate *float64 `json:"totalRate"` // absent from controllers that predate --total-rate
		Warnings  []string `json:"warnings"`
		StartAt   string   `json:"startAt"` // absent when not synchronized
	}
	if err := json.Unmarshal(b, &out); err != nil || out.RunID == "" {
		return startedRun{}, fmt.Errorf("controller returned no runId")
	}
	run := startedRun{ID: out.RunID, Workers: out.Workers, Available: len(out.Workers)}
	if out.Available != nil {
		run.Available = *out.Available
	} else if opts.Workers != "" {
		run.SelectionIgnored = true
	}
	run.Warnings = out.Warnings
	if t, err := time.Parse(time.RFC3339Nano, out.StartAt); err == nil {
		run.StartAt = t
	} else if opts.StartDelay > 0 && len(out.Workers) > 1 {
		run.StartIgnored = true
	}
	if opts.TotalRate > 0 && out.TotalRate == nil {
		run.TotalRateIgnored = true
	}
	return run, nil
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

// Abort asks the controller to stop a run's workers. It returns the workers that
// were asked to stop, none when the run had already finished.
func (c *ControllerClient) Abort(ctx context.Context, id string) ([]string, error) {
	resp, err := c.do(ctx, http.MethodDelete, "/runs/"+id, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("aborting run: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		Aborted []string `json:"aborted"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parsing abort response: %w", err)
	}
	return out.Aborted, nil
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
func runViaController(ctx context.Context, c *ControllerClient, scenarioPath string, opts startOptions, outDir string, poll time.Duration) ([]workerResult, error) {
	started, err := c.Start(ctx, scenarioPath, opts)
	if err != nil {
		return nil, err
	}
	id := started.ID
	if started.TotalRateIgnored {
		// Every worker is running the full, unsplit load: stop it rather than hit
		// the target at a rate nobody asked for.
		c.Abort(ctx, id)
		return nil, fmt.Errorf("this controller does not support --total-rate, so run %s was aborted; upgrade the controller", id)
	}
	for _, w := range started.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if started.SelectionIgnored {
		fmt.Fprintf(os.Stderr, "warning: this controller does not support --workers, so the run started on all %d matching workers; upgrade the controller\n", len(started.Workers))
	}
	if started.StartIgnored {
		fmt.Fprintln(os.Stderr, "warning: this controller does not support --start-delay, so the workers start as each receives the scenario, not together; upgrade the controller")
	}
	fmt.Printf("run %s started on %d of %d worker(s): %s\n", id, len(started.Workers), started.Available, strings.Join(started.Workers, ", "))
	if !started.StartAt.IsZero() {
		fmt.Printf("all workers start at %s (in %s)\n", started.StartAt.Local().Format("15:04:05.000"), time.Until(started.StartAt).Round(100*time.Millisecond))
	}
	run, err := c.Wait(ctx, id, poll)
	if err != nil && ctx.Err() != nil {
		// Interrupted (Ctrl-C): stop the workers rather than leave the load running,
		// then carry on with whatever they managed to report.
		fmt.Fprintln(os.Stderr, "interrupted: aborting the run on the workers...")
		actx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		ctx = actx
		if _, aerr := c.Abort(ctx, id); aerr != nil {
			return nil, aerr
		}
		run, err = c.Wait(ctx, id, time.Second)
	}
	if err != nil {
		return nil, err
	}
	var results []workerResult
	for _, w := range run.Workers {
		res := workerResult{Name: w.Name, TaskID: w.TaskID, Status: w.Status, ExitCode: w.ExitCode, Err: w.Error, Warning: w.Warning}
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

// registeredWorker mirrors one entry of the controller's GET /workers.
type registeredWorker struct {
	Name     string            `json:"name"`
	URL      string            `json:"url"`
	Labels   map[string]string `json:"labels"`
	LastSeen time.Time         `json:"last_seen"`
}

// Workers lists the live workers matching labels.
func (c *ControllerClient) Workers(ctx context.Context, labels []string) ([]registeredWorker, error) {
	q := url.Values{}
	for _, l := range labels {
		q.Add("label", l)
	}
	path := "/workers"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	resp, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing workers: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out []registeredWorker
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parsing workers: %w", err)
	}
	return out, nil
}

// printWorkers writes one row per worker, sorted by name.
func printWorkers(w io.Writer, ws []registeredWorker, now time.Time) {
	sort.Slice(ws, func(i, j int) bool { return ws[i].Name < ws[j].Name })
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tURL\tLABELS\tLAST SEEN")
	for _, x := range ws {
		keys := make([]string, 0, len(x.Labels))
		for k := range x.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, len(keys))
		for i, k := range keys {
			pairs[i] = k + "=" + x.Labels[k]
		}
		labels := strings.Join(pairs, ",")
		if labels == "" {
			labels = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s ago\n", x.Name, x.URL, labels, now.Sub(x.LastSeen).Round(time.Second))
	}
	tw.Flush()
}
