package main

import "testing"

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
