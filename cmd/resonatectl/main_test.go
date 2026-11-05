package main

import (
	"strings"
	"testing"
)

func TestExtractPositionalAllowsScenarioBeforeOrAfterFlags(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantPos  string
		wantRest []string
	}{
		{
			name:     "scenario last",
			args:     []string{"--controller", "http://c", "--poll", "1s", "scenario.yaml"},
			wantPos:  "scenario.yaml",
			wantRest: []string{"--controller", "http://c", "--poll", "1s"},
		},
		{
			name:     "scenario first (the order that used to silently drop flags)",
			args:     []string{"scenario.yaml", "--controller", "http://c", "--poll", "1s"},
			wantPos:  "scenario.yaml",
			wantRest: []string{"--controller", "http://c", "--poll", "1s"},
		},
		{
			name:     "scenario in the middle",
			args:     []string{"--controller", "http://c", "scenario.yaml", "--poll", "1s"},
			wantPos:  "scenario.yaml",
			wantRest: []string{"--controller", "http://c", "--poll", "1s"},
		},
		{
			name:     "equals form",
			args:     []string{"--controller=http://c", "scenario.yaml"},
			wantPos:  "scenario.yaml",
			wantRest: []string{"--controller=http://c"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pos, rest := extractPositional(c.args)
			if pos != c.wantPos {
				t.Errorf("positional = %q, want %q", pos, c.wantPos)
			}
			if len(rest) != len(c.wantRest) {
				t.Fatalf("rest = %v, want %v", rest, c.wantRest)
			}
			for i := range rest {
				if rest[i] != c.wantRest[i] {
					t.Errorf("rest[%d] = %q, want %q", i, rest[i], c.wantRest[i])
				}
			}
		})
	}
}

func TestExtractPositionalSecondPositionalIsLeftForFlagSetToReject(t *testing.T) {
	pos, rest := extractPositional([]string{"scenario.yaml", "extra.yaml"})
	if pos != "scenario.yaml" {
		t.Errorf("positional = %q, want scenario.yaml", pos)
	}
	if len(rest) != 1 || rest[0] != "extra.yaml" {
		t.Errorf("rest = %v, want [extra.yaml] (so fs.NArg() != 0 catches it)", rest)
	}
}

func TestHoistGlobalFlags(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"workers"}, []string{"workers"}},
		{[]string{"--context", "prod", "workers"}, []string{"workers", "--context", "prod"}},
		{[]string{"--context=prod", "--config", "f.yaml", "run", "s.yaml", "--out", "d"}, []string{"run", "--context=prod", "--config", "f.yaml", "s.yaml", "--out", "d"}},
		{[]string{"--controller", "http://c", "config", "view"}, []string{"config", "--controller", "http://c", "view"}},
		{[]string{"--version"}, []string{"--version"}},
		{[]string{"--context", "prod"}, []string{"--context", "prod"}}, // no subcommand: unchanged
		{nil, nil},
	}
	for _, c := range cases {
		got := hoistGlobalFlags(c.in)
		if strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("hoistGlobalFlags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitPositionals(t *testing.T) {
	pos, fl := splitPositionals([]string{"--config", "f.yaml", "use", "prod", "--context=x"})
	if strings.Join(pos, ",") != "use,prod" || strings.Join(fl, ",") != "--config,f.yaml,--context=x" {
		t.Fatalf("pos=%q flags=%q", pos, fl)
	}
}

func TestSplitPositionalsKnowsBooleanFlags(t *testing.T) {
	// --use and --token-stdin take no value, so they must not swallow the name.
	pos, fl := splitPositionals([]string{"set-context", "--use", "prod", "--token-stdin", "--controller", "http://c"})
	if strings.Join(pos, ",") != "set-context,prod" || strings.Join(fl, ",") != "--use,--token-stdin,--controller,http://c" {
		t.Fatalf("pos=%q flags=%q", pos, fl)
	}
}
