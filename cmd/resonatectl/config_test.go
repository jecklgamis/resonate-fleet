package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const sampleConfig = `# my contexts
current-context: local
contexts:
  local:
    controller: http://localhost:58090
    token: ctl-secret
  prod:
    controller: https://prod.example.com
    token_env: PROD_TOKEN
  filed:
    controller: https://filed.example.com
    token_file: ~/tok
  broken:
    controller: https://x
    token: a
    token_env: B
`

func writeConfig(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolvePrecedence(t *testing.T) {
	cfg := writeConfig(t, sampleConfig, 0o600)
	cases := []struct {
		name       string
		in         connectionInput
		env        map[string]string
		wantURL    string
		wantToken  string
		wantErrHas string
	}{
		{"current-context", connectionInput{ConfigFlag: cfg}, nil, "http://localhost:58090", "ctl-secret", ""},
		{"env beats current-context", connectionInput{ConfigFlag: cfg}, map[string]string{"RESONATE_CONTROLLER": "http://env", "CONTROLLER_TOKEN": "envtok"}, "http://env", "envtok", ""},
		{"--controller beats env and context", connectionInput{ConfigFlag: cfg, ControllerFlag: "http://flag"}, map[string]string{"RESONATE_CONTROLLER": "http://env"}, "http://flag", "ctl-secret", ""},
		{"--context beats exported env", connectionInput{ConfigFlag: cfg, ContextFlag: "prod"}, map[string]string{"RESONATE_CONTROLLER": "http://stale", "CONTROLLER_TOKEN": "stale", "PROD_TOKEN": "prodtok"}, "https://prod.example.com", "prodtok", ""},
		{"--controller overrides an explicit context's URL", connectionInput{ConfigFlag: cfg, ContextFlag: "prod", ControllerFlag: "http://other"}, map[string]string{"PROD_TOKEN": "prodtok"}, "http://other", "prodtok", ""},
		{"token_env unset", connectionInput{ConfigFlag: cfg, ContextFlag: "prod"}, nil, "", "", "PROD_TOKEN is not set"},
		{"unknown context lists the others", connectionInput{ConfigFlag: cfg, ContextFlag: "nope"}, nil, "", "", "have: broken, filed, local, prod"},
		{"two token sources", connectionInput{ConfigFlag: cfg, ContextFlag: "broken"}, nil, "", "", "only one of"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.in.Getenv = env(c.env)
			got, err := resolveConnection(c.in)
			if c.wantErrHas != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErrHas) {
					t.Fatalf("err = %v, want it to contain %q", err, c.wantErrHas)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Controller != c.wantURL || got.Token != c.wantToken {
				t.Fatalf("got controller=%q token=%q, want %q %q", got.Controller, got.Token, c.wantURL, c.wantToken)
			}
		})
	}
}

func TestTokenFileExpandsHome(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "tok"), []byte("  from-file\n"), 0o600)
	cfg := writeConfig(t, sampleConfig, 0o600)
	got, err := resolveConnection(connectionInput{ConfigFlag: cfg, ContextFlag: "filed", Getenv: env(map[string]string{"HOME": home})})
	if err != nil || got.Token != "from-file" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestMissingConfigFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	// A default location that doesn't exist is fine: env-only users have no file.
	got, err := resolveConnection(connectionInput{Getenv: env(map[string]string{"XDG_CONFIG_HOME": t.TempDir(), "RESONATE_CONTROLLER": "http://env", "CONTROLLER_TOKEN": "t"})})
	if err != nil || got.Controller != "http://env" || got.Token != "t" {
		t.Fatalf("env-only = %+v, %v", got, err)
	}
	// One that was asked for explicitly is an error.
	if _, err := resolveConnection(connectionInput{ConfigFlag: missing, Getenv: env(nil)}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("--config on a missing file: %v", err)
	}
	if _, err := resolveConnection(connectionInput{Getenv: env(map[string]string{"RESONATECTL_CONFIG": missing})}); err == nil {
		t.Fatal("$RESONATECTL_CONFIG on a missing file was accepted")
	}
}

func TestConfigMustNotHaveUnknownFields(t *testing.T) {
	p := writeConfig(t, "contexts:\n  a:\n    controler: http://typo\n", 0o600)
	if _, err := resolveConnection(connectionInput{ConfigFlag: p, Getenv: env(nil)}); err == nil {
		t.Fatal("a misspelled field was silently ignored")
	}
}

func TestOpenPermissionsWarnOnlyWithAnInlineToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	open := writeConfig(t, sampleConfig, 0o644)
	os.Chmod(open, 0o644) // WriteFile is subject to umask
	got, err := resolveConnection(connectionInput{ConfigFlag: open, Getenv: env(nil)})
	if err != nil || len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "chmod 600") {
		t.Fatalf("inline token in a 0644 file: %+v, %v", got, err)
	}
	if strings.Contains(got.Warnings[0], "ctl-secret") {
		t.Fatal("warning leaks the token")
	}
	// token_env contexts hold no secret, so there is nothing to warn about.
	got, _ = resolveConnection(connectionInput{ConfigFlag: open, ContextFlag: "prod", Getenv: env(map[string]string{"PROD_TOKEN": "x"})})
	if len(got.Warnings) != 0 {
		t.Fatalf("warned for a token_env context: %v", got.Warnings)
	}
	tight := writeConfig(t, sampleConfig, 0o600)
	got, _ = resolveConnection(connectionInput{ConfigFlag: tight, Getenv: env(nil)})
	if len(got.Warnings) != 0 {
		t.Fatalf("warned for a 0600 file: %v", got.Warnings)
	}
}

func TestUseContextKeepsCommentsAndMode(t *testing.T) {
	p := writeConfig(t, sampleConfig, 0o600)
	if err := useContext(p, "prod"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "# my contexts") || !strings.Contains(string(b), "current-context: prod") || !strings.Contains(string(b), "token: ctl-secret") {
		t.Fatalf("file after use:\n%s", b)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
		}
	}
	if err := useContext(p, "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown context: %v", err)
	}
	if err := useContext(filepath.Join(t.TempDir(), "none.yaml"), "x"); err == nil {
		t.Fatal("use on a missing file succeeded")
	}
	// A file without current-context gets one.
	q := writeConfig(t, "contexts:\n  a:\n    controller: http://a\n", 0o600)
	if err := useContext(q, "a"); err != nil {
		t.Fatal(err)
	}
	if cfg, _, _ := loadConfig(q); cfg.CurrentContext != "a" {
		t.Fatalf("current-context = %q", cfg.CurrentContext)
	}
}

func TestViewRedactsInlineTokens(t *testing.T) {
	p := writeConfig(t, sampleConfig, 0o600)
	cfg, exists, _ := loadConfig(p)
	var sb strings.Builder
	viewConfig(&sb, p, cfg, exists)
	out := sb.String()
	if strings.Contains(out, "ctl-secret") {
		t.Fatalf("view printed a token:\n%s", out)
	}
	for _, want := range []string{"*", "local", "$PROD_TOKEN", "file ~/tok", "(in file, hidden)"} {
		if !strings.Contains(out, want) {
			t.Errorf("view is missing %q:\n%s", want, out)
		}
	}
	sb.Reset()
	viewConfig(&sb, "/nowhere", Config{}, false)
	if !strings.Contains(sb.String(), "no config file at /nowhere") {
		t.Fatalf("missing file view: %q", sb.String())
	}
}
