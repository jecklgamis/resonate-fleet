package main

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Worker is one resonate-worker instance to fan a scenario out to.
type Worker struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`
	Token    string `yaml:"token"`     // discouraged: a literal token in a file that's usually checked in
	TokenEnv string `yaml:"token_env"` // preferred: the token is read from this env var at load time
}

// FleetConfig is a workers.yaml file: the fixed set of workers a scenario is
// fanned out to. There is no discovery — every worker to use is named here.
type FleetConfig struct {
	Workers []Worker `yaml:"workers"`
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
	if len(cfg.Workers) == 0 {
		return nil, fmt.Errorf("%s: no workers listed", path)
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
