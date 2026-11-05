package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "workers.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiscoverResolvesSortedWithDefaults(t *testing.T) {
	t.Setenv("FLEET_TOK", "secret")
	cfg, err := LoadFleetConfig(writeCfg(t, "discover:\n  dns: w.perf.svc\n  token_env: FLEET_TOK\n"))
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(_ context.Context, host string) ([]string, error) {
		if host != "w.perf.svc" {
			t.Errorf("host = %q", host)
		}
		return []string{"10.0.0.9", "10.0.0.2", "fd00::1"}, nil
	}
	if err := cfg.Resolve(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	want := []Worker{
		{Name: "10.0.0.2", URL: "http://10.0.0.2:58080", Token: "secret"},
		{Name: "10.0.0.9", URL: "http://10.0.0.9:58080", Token: "secret"},
		{Name: "fd00::1", URL: "http://[fd00::1]:58080", Token: "secret"},
	}
	if len(cfg.Workers) != len(want) {
		t.Fatalf("workers = %+v", cfg.Workers)
	}
	for i := range want {
		if cfg.Workers[i] != want[i] {
			t.Errorf("workers[%d] = %+v, want %+v", i, cfg.Workers[i], want[i])
		}
	}
}

func TestDiscoverKeepsListedWorkers(t *testing.T) {
	cfg, err := LoadFleetConfig(writeCfg(t, "workers:\n  - {name: local, url: 'http://localhost:1'}\ndiscover: {dns: w, port: 9, scheme: https}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Resolve(context.Background(), func(context.Context, string) ([]string, error) { return []string{"1.2.3.4"}, nil })
	if len(cfg.Workers) != 2 || cfg.Workers[0].Name != "local" || cfg.Workers[1].URL != "https://1.2.3.4:9" {
		t.Fatalf("workers = %+v", cfg.Workers)
	}
}

func TestDiscoverFailures(t *testing.T) {
	cfg, _ := LoadFleetConfig(writeCfg(t, "discover: {dns: w}\n"))
	empty := func(context.Context, string) ([]string, error) { return nil, nil }
	if err := cfg.Resolve(context.Background(), empty); err == nil || !strings.Contains(err.Error(), "no addresses") {
		t.Errorf("empty result: %v", err)
	}
	fail := func(context.Context, string) ([]string, error) { return nil, errors.New("nxdomain") }
	if err := cfg.Resolve(context.Background(), fail); err == nil {
		t.Error("lookup error not reported")
	}
	for _, bad := range []string{"discover: {port: 1}\n", "discover: {dns: w, scheme: ftp}\n", "discover: {dns: w, token: a, token_env: B}\n", "discover: {dns: w, token_env: UNSET_FLEET_VAR}\n"} {
		if _, err := LoadFleetConfig(writeCfg(t, bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
