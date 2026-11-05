package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Registrar announces this worker to a resonate-controller and keeps doing so as
// a heartbeat; the controller forgets a worker that stops. It is plain HTTP and
// assumes nothing about where the worker runs.
type Registrar struct {
	ControllerURL   string
	ControllerToken string
	Name            string
	URL             string // address the controller can reach this worker at
	WorkerToken     string // this worker's API_TOKEN, which the controller sends back to it
	Labels          map[string]string
	Interval        time.Duration

	client *http.Client
}

// Run registers immediately and then every Interval until ctx is done. Failures
// are logged once when they start and once when they clear, not every beat.
func (r *Registrar) Run(ctx context.Context) {
	if r.client == nil {
		r.client = &http.Client{Timeout: 10 * time.Second}
	}
	failing := false
	for {
		err := r.register(ctx)
		switch {
		case err != nil && !failing:
			log.Printf("controller registration failed (will keep retrying): %v", err)
		case err == nil && failing:
			log.Printf("controller registration recovered")
		}
		failing = err != nil
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.Interval):
		}
	}
}

// Deregister tells the controller this worker is going away, so it stops being
// handed runs at once instead of lingering until its heartbeat expires. Call it
// after Run has returned, or a late heartbeat could re-register the worker.
func (r *Registrar) Deregister(ctx context.Context) error {
	if r.client == nil {
		r.client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimRight(r.ControllerURL, "/")+"/workers/"+url.PathEscape(r.Name), nil)
	if err != nil {
		return err
	}
	if r.ControllerToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.ControllerToken)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func (r *Registrar) register(ctx context.Context) error {
	body, _ := json.Marshal(map[string]any{"name": r.Name, "url": r.URL, "token": r.WorkerToken, "labels": r.Labels})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.ControllerURL, "/")+"/workers", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.ControllerToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.ControllerToken)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// workerIdentity is the worker's name (WORKER_NAME, else the hostname) and its
// labels (WORKER_LABELS), shared by controller registration and metrics.
func workerIdentity() (string, map[string]string, error) {
	name := os.Getenv("WORKER_NAME")
	if name == "" {
		h, err := os.Hostname()
		if err != nil {
			return "", nil, fmt.Errorf("WORKER_NAME is unset and the hostname is unavailable: %w", err)
		}
		name = h
	}
	labels, err := parseLabels(os.Getenv("WORKER_LABELS"))
	if err != nil {
		return "", nil, err
	}
	return name, labels, nil
}

// registrarFromEnv builds a Registrar when CONTROLLER_URL is set, else returns
// nil. WORKER_NAME defaults to the hostname; ADVERTISE_URL defaults to this
// host's address on the interface that routes to the controller.
func registrarFromEnv(addr, workerToken string) (*Registrar, error) {
	ctrl := os.Getenv("CONTROLLER_URL")
	if ctrl == "" {
		return nil, nil
	}
	name, labels, err := workerIdentity()
	if err != nil {
		return nil, err
	}
	advertise := os.Getenv("ADVERTISE_URL")
	if advertise == "" {
		var err error
		if advertise, err = detectAdvertiseURL(ctrl, addr); err != nil {
			return nil, fmt.Errorf("ADVERTISE_URL is unset and could not be detected: %w", err)
		}
	}
	interval := 10 * time.Second
	if v := os.Getenv("HEARTBEAT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		}
	}
	return &Registrar{
		ControllerURL: ctrl, ControllerToken: os.Getenv("CONTROLLER_TOKEN"),
		Name: name, URL: advertise, WorkerToken: workerToken, Labels: labels, Interval: interval,
	}, nil
}

// detectAdvertiseURL finds the local address used to reach the controller (a UDP
// "connect" sends nothing, it only asks the OS for a route) and pairs it with
// the port in listenAddr.
func detectAdvertiseURL(controllerURL, listenAddr string) (string, error) {
	u, err := url.Parse(controllerURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("bad CONTROLLER_URL %q", controllerURL)
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	conn, err := net.Dial("udp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	ip := conn.LocalAddr().(*net.UDPAddr).IP.String()
	_, lport, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("bad ADDR %q: %w", listenAddr, err)
	}
	if _, err := strconv.Atoi(lport); err != nil {
		return "", fmt.Errorf("ADDR %q has no numeric port", listenAddr)
	}
	return "http://" + net.JoinHostPort(ip, lport), nil
}

// parseLabels reads "key=value,key=value".
func parseLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("WORKER_LABELS: invalid entry %q: want key=value", p)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}
