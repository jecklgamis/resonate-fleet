package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type stageT struct {
	Duration string  `yaml:"duration"`
	Rate     float64 `yaml:"rate"`
	Workers  int     `yaml:"workers"`
}

type loadT struct {
	Duration   string   `yaml:"duration"`
	Rate       float64  `yaml:"rate"`
	Workers    int      `yaml:"workers"`
	MaxWorkers int      `yaml:"max_workers"`
	Requests   int      `yaml:"requests"`
	Iterations int      `yaml:"iterations"`
	Stages     []stageT `yaml:"stages"`
}

func parseLoad(t *testing.T, b []byte) loadT {
	t.Helper()
	var s struct {
		Load loadT `yaml:"load"`
	}
	if err := yaml.Unmarshal(b, &s); err != nil {
		t.Fatalf("bad output YAML: %v\n%s", err, b)
	}
	return s.Load
}

const stagedScenario = `# my scenario
protocol: http

# ramp it
load:
  stages:
    - { duration: 10s, rate: 20, workers: 5 }   # warm up
    - { duration: 30s, rate: 100, workers: 20 }
    - { duration: 10s, rate: 0, workers: 0 }

http:
  targets:
    - url: http://example.com/   # keep me
`

func TestSplitStagesScalesToTotalAndDividesAcrossWorkers(t *testing.T) {
	// Peak 100, total 150: factor 1.5, so fleet stages are rate 30/150/0 and
	// users 8/30/0 (7.5 rounds up), split across 3 workers.
	bodies, warns, err := splitScenario([]byte(stagedScenario), 150, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 || len(warns) != 0 {
		t.Fatalf("bodies=%d warnings=%v", len(bodies), warns)
	}
	wantUsers := [][]int{{3, 10, 0}, {3, 10, 0}, {2, 10, 0}} // 8 = 3+3+2, 30 = 10x3
	sumRate := make([]float64, 3)
	sumUsers := make([]int, 3)
	for i, b := range bodies {
		l := parseLoad(t, b)
		if len(l.Stages) != 3 {
			t.Fatalf("worker %d stages = %d", i, len(l.Stages))
		}
		for j, s := range l.Stages {
			if s.Workers != wantUsers[i][j] {
				t.Errorf("worker %d stage %d users = %d, want %d", i, j, s.Workers, wantUsers[i][j])
			}
			if s.Duration != []string{"10s", "30s", "10s"}[j] {
				t.Errorf("duration changed: %q", s.Duration)
			}
			sumRate[j] += s.Rate
			sumUsers[j] += s.Workers
		}
	}
	for j, want := range []float64{30, 150, 0} {
		if d := sumRate[j] - want; d > 0.001 || d < -0.001 {
			t.Errorf("stage %d fleet rate = %v, want %v", j, sumRate[j], want)
		}
	}
	if sumUsers[0] != 8 || sumUsers[1] != 30 || sumUsers[2] != 0 {
		t.Errorf("fleet users = %v", sumUsers)
	}
}

func TestSplitKeepsCommentsAndTheRestOfTheScenario(t *testing.T) {
	bodies, _, err := splitScenario([]byte(stagedScenario), 150, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# my scenario", "# ramp it", "# warm up", "# keep me", "url: http://example.com/", "protocol: http"} {
		if !strings.Contains(string(bodies[0]), want) {
			t.Errorf("rewritten scenario lost %q:\n%s", want, bodies[0])
		}
	}
}

func TestSplitFlatLoad(t *testing.T) {
	flat := "load:\n  duration: 10s\n  rate: 20\n  workers: 5\n  iterations: 7\nhttp: {}\n"
	bodies, warns, err := splitScenario([]byte(flat), 100, 4) // factor 5: rate 100, users 25
	if err != nil {
		t.Fatal(err)
	}
	var rate float64
	var users int
	for _, b := range bodies {
		l := parseLoad(t, b)
		rate += l.Rate
		users += l.Workers
		if l.Duration != "10s" || l.Iterations != 7 {
			t.Errorf("duration/iterations must not change: %+v", l)
		}
	}
	if rate != 100 || users != 25 || len(warns) != 0 {
		t.Fatalf("fleet rate=%v users=%d warnings=%v", rate, users, warns)
	}
}

func TestSplitRequestsDividedNotScaled(t *testing.T) {
	s := "load:\n  rate: 10\n  requests: 1000\n  max_workers: 8\nhttp: {}\n"
	bodies, _, err := splitScenario([]byte(s), 20, 3) // factor 2
	if err != nil {
		t.Fatal(err)
	}
	var req, maxw int
	for _, b := range bodies {
		l := parseLoad(t, b)
		req += l.Requests
		maxw += l.MaxWorkers
	}
	if req != 1000 || maxw != 16 {
		t.Fatalf("fleet requests=%d (want 1000, divided not scaled) max_workers=%d (want 16, scaled)", req, maxw)
	}
}

func TestSplitFewerUsersThanWorkersGivesEachOneAndWarns(t *testing.T) {
	s := "load:\n  rate: 10\n  workers: 2\n  duration: 5s\nhttp: {}\n"
	bodies, warns, err := splitScenario([]byte(s), 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range bodies {
		if l := parseLoad(t, b); l.Workers != 1 || l.Rate != 2 {
			t.Errorf("worker %d: %+v, want 1 user at rate 2", i, l)
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "load.workers") {
		t.Fatalf("warnings = %v", warns)
	}
}

func TestSplitRejects(t *testing.T) {
	for name, tc := range map[string]struct {
		body  string
		total float64
		n     int
	}{
		"no load block":   {"protocol: http\n", 10, 2},
		"no rate at all":  {"load:\n  workers: 5\n  duration: 5s\n", 10, 2},
		"only rate zero":  {"load:\n  rate: 0\n  duration: 5s\n", 10, 2},
		"bad rate":        {"load:\n  rate: fast\n", 10, 2},
		"not yaml":        {"load: [\n", 10, 2},
		"zero total":      {"load:\n  rate: 5\n", 0, 2},
		"negative total":  {"load:\n  rate: 5\n", -1, 2},
		"no workers":      {"load:\n  rate: 5\n", 10, 0},
		"load not a map":  {"load: 5\n", 10, 2},
		"stage bad users": {"load:\n  stages:\n    - {rate: 5, workers: many}\n", 10, 2},
	} {
		if _, _, err := splitScenario([]byte(tc.body), tc.total, tc.n); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestSplitFractionalRatesAddUp(t *testing.T) {
	bodies, _, err := splitScenario([]byte("load:\n  rate: 1\n  duration: 5s\n"), 10, 3)
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, b := range bodies {
		sum += parseLoad(t, b).Rate
	}
	if d := sum - 10; d > 0.001 || d < -0.001 {
		t.Fatalf("fleet rate = %v, want 10", sum)
	}
}
