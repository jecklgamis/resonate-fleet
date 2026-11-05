package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Worker is one resonate-worker instance to fan a scenario out to.
type Worker struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`
	Token    string `yaml:"token"`     // discouraged: a literal token in a file that's usually checked in
	TokenEnv string `yaml:"token_env"` // preferred: the token is read from this env var at load time
}

// Discover finds workers by resolving a DNS name to one address per worker. In
// Kubernetes, point it at a headless Service (clusterIP: None): its DNS name
// answers with the IP of every ready pod, so each pod is addressed on its own.
// A normal Service would not work, since it load-balances each request to a
// random pod and a task's polls could land on a pod that never saw it.
type Discover struct {
	DNS      string `yaml:"dns"`       // name to resolve, e.g. resonate-worker-headless.perf.svc.cluster.local
	Port     int    `yaml:"port"`      // worker port; default 58080
	Scheme   string `yaml:"scheme"`    // http (default) or https
	Token    string `yaml:"token"`     // discouraged, as for Worker
	TokenEnv string `yaml:"token_env"` // preferred; all discovered workers share this token
}

// FleetConfig is a workers.yaml file: the workers a scenario is fanned out to,
// listed in Workers and/or found through Discover.
type FleetConfig struct {
	Workers  []Worker  `yaml:"workers"`
	Discover *Discover `yaml:"discover"`
}

// Lookup resolves a DNS name to the addresses behind it.
type Lookup func(ctx context.Context, host string) ([]string, error)

// Resolve appends the workers found by Discover (if configured) to Workers. It
// fails if discovery finds nothing, rather than silently running on fewer
// workers than intended. Discovered workers are named after their address and
// sorted, so a run's output directories are predictable.
func (c *FleetConfig) Resolve(ctx context.Context, lookup Lookup) error {
	d := c.Discover
	if d == nil {
		return nil
	}
	addrs, err := lookup(ctx, d.DNS)
	if err != nil {
		return fmt.Errorf("discover: resolving %s: %w", d.DNS, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("discover: %s resolved to no addresses", d.DNS)
	}
	sort.Strings(addrs)
	taken := make(map[string]bool, len(c.Workers))
	for _, w := range c.Workers {
		taken[w.Name] = true
	}
	for _, a := range addrs {
		name := a
		if taken[name] {
			return fmt.Errorf("discover: worker name %q (from %s) collides with a listed worker", name, d.DNS)
		}
		taken[name] = true
		c.Workers = append(c.Workers, Worker{
			Name:  name,
			URL:   d.Scheme + "://" + net.JoinHostPort(a, strconv.Itoa(d.Port)),
			Token: d.Token,
		})
	}
	return nil
}

// DNSLookup resolves host with the system resolver.
func DNSLookup(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// LoadFleetConfig reads and validates path, resolving each worker's token from
// its TokenEnv. It fails fast — before anything is submitted — on a missing
// name/url, both Token and TokenEnv set, an unset TokenEnv variable, or two
// workers sharing a name (which would collide in the output directory and the
// summary table).
func LoadFleetConfig(path string) (*FleetConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg FleetConfig
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(cfg.Workers) == 0 && cfg.Discover == nil {
		return nil, fmt.Errorf("%s: no workers listed and no discover section", path)
	}
	if d := cfg.Discover; d != nil {
		if d.DNS == "" {
			return nil, fmt.Errorf("discover: dns is required")
		}
		if d.Port == 0 {
			d.Port = 58080
		}
		if d.Scheme == "" {
			d.Scheme = "http"
		}
		if d.Scheme != "http" && d.Scheme != "https" {
			return nil, fmt.Errorf("discover: scheme must be http or https, got %q", d.Scheme)
		}
		if d.Token != "" && d.TokenEnv != "" {
			return nil, fmt.Errorf("discover: set token or token_env, not both")
		}
		if d.TokenEnv != "" {
			v, ok := os.LookupEnv(d.TokenEnv)
			if !ok {
				return nil, fmt.Errorf("discover: token_env %q is not set", d.TokenEnv)
			}
			d.Token = v
		}
	}

	seen := make(map[string]bool, len(cfg.Workers))
	for i := range cfg.Workers {
		w := &cfg.Workers[i]
		if w.Name == "" {
			return nil, fmt.Errorf("workers[%d]: name is required", i)
		}
		if seen[w.Name] {
			return nil, fmt.Errorf("workers[%d]: duplicate worker name %q", i, w.Name)
		}
		seen[w.Name] = true
		if w.URL == "" {
			return nil, fmt.Errorf("workers[%d] (%s): url is required", i, w.Name)
		}
		if w.Token != "" && w.TokenEnv != "" {
			return nil, fmt.Errorf("workers[%d] (%s): set token or token_env, not both", i, w.Name)
		}
		if w.TokenEnv != "" {
			v, ok := os.LookupEnv(w.TokenEnv)
			if !ok {
				return nil, fmt.Errorf("workers[%d] (%s): token_env %q is not set", i, w.Name, w.TokenEnv)
			}
			w.Token = v
		}
	}
	return &cfg, nil
}
