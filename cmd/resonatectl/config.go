package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"

	"gopkg.in/yaml.v3"
)

// Config is resonatectl's config file: named contexts, each a controller and
// the token for it, and which one is current. It follows kubectl's shape.
//
//	current-context: local
//	contexts:
//	  local: {controller: http://localhost:58090, token: ctl-secret}
//	  prod:  {controller: https://resonate.example.com, token_env: PROD_TOKEN}
type Config struct {
	CurrentContext string             `yaml:"current-context"`
	Contexts       map[string]Context `yaml:"contexts"`
}

// Context is one controller. Give at most one of Token (stored in the file, as
// kubectl does), TokenEnv (an environment variable to read) or TokenFile (a file
// to read, "~" expanded).
type Context struct {
	Controller string `yaml:"controller"`
	Token      string `yaml:"token"`
	TokenEnv   string `yaml:"token_env"`
	TokenFile  string `yaml:"token_file"`
}

// defaultConfigPath is $XDG_CONFIG_HOME/resonatectl/config.yaml, falling back to
// ~/.config/resonatectl/config.yaml.
func defaultConfigPath(getenv func(string) string) string {
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "resonatectl", "config.yaml")
	}
	if h := getenv("HOME"); h != "" {
		return filepath.Join(h, ".config", "resonatectl", "config.yaml")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".config", "resonatectl", "config.yaml")
	}
	return ""
}

// loadConfig reads path. A missing file is not an error (exists is false); a
// malformed one, or one with unknown fields, is.
func loadConfig(path string) (cfg Config, exists bool, err error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && err != io.EOF {
		return Config{}, true, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, true, nil
}

// connectionInput is everything that decides which controller and token to use.
type connectionInput struct {
	ControllerFlag string // --controller, "" if unset
	ContextFlag    string // --context, "" if unset
	ConfigFlag     string // --config, "" if unset
	Getenv         func(string) string
}

// connection is the resolved controller and token, plus anything worth telling
// the user (warnings go to stderr).
type connection struct {
	Controller string
	Token      string
	Warnings   []string
}

// resolveConnection picks the controller and token. Order, highest first:
//
//	explicit flags (--controller, --context)
//	environment ($RESONATE_CONTROLLER, $CONTROLLER_TOKEN)
//	the config file's current-context
//
// An explicit --context beats the environment, so a stale exported variable
// can't send `--context prod` to the wrong place; --controller still overrides
// that context's controller.
func resolveConnection(in connectionInput) (connection, error) {
	getenv := in.Getenv
	var out connection

	path, explicitPath := in.ConfigFlag, in.ConfigFlag != ""
	if !explicitPath {
		if v := getenv("RESONATECTL_CONFIG"); v != "" {
			path, explicitPath = v, true
		} else {
			path = defaultConfigPath(getenv)
		}
	}
	var cfg Config
	var exists bool
	if path != "" {
		var err error
		if cfg, exists, err = loadConfig(path); err != nil {
			return out, err
		}
	}
	if !exists && explicitPath {
		return out, fmt.Errorf("config file %s not found", path)
	}

	name, explicitCtx := in.ContextFlag, in.ContextFlag != ""
	if !explicitCtx {
		name = cfg.CurrentContext
	}
	var ctx *Context
	if name != "" {
		c, ok := cfg.Contexts[name]
		if !ok {
			return out, fmt.Errorf("context %q not found in %s (have: %s)", name, path, contextNames(cfg))
		}
		ctx = &c
	}

	switch {
	case in.ControllerFlag != "":
		out.Controller = in.ControllerFlag
	case explicitCtx && ctx != nil:
		out.Controller = ctx.Controller
	case getenv("RESONATE_CONTROLLER") != "":
		out.Controller = getenv("RESONATE_CONTROLLER")
	case ctx != nil:
		out.Controller = ctx.Controller
	}

	switch {
	case !explicitCtx && getenv("CONTROLLER_TOKEN") != "":
		out.Token = getenv("CONTROLLER_TOKEN")
	case ctx != nil:
		tok, err := ctx.resolveToken(name, getenv)
		if err != nil {
			return out, err
		}
		out.Token = tok
		if ctx.Token != "" && tooOpen(path) {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s holds a token and is readable by other users; run: chmod 600 %s", path, path))
		}
	}
	return out, nil
}

func (c Context) resolveToken(name string, getenv func(string) string) (string, error) {
	set := 0
	for _, s := range []string{c.Token, c.TokenEnv, c.TokenFile} {
		if s != "" {
			set++
		}
	}
	if set > 1 {
		return "", fmt.Errorf("context %q: set only one of token, token_env and token_file", name)
	}
	switch {
	case c.TokenEnv != "":
		v := getenv(c.TokenEnv)
		if v == "" {
			return "", fmt.Errorf("context %q: token_env %s is not set", name, c.TokenEnv)
		}
		return v, nil
	case c.TokenFile != "":
		p := c.TokenFile
		if strings.HasPrefix(p, "~/") {
			if h := getenv("HOME"); h != "" {
				p = filepath.Join(h, p[2:])
			}
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("context %q: reading token_file: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return c.Token, nil
}

// tooOpen reports whether path is readable by group or others. Windows has no
// such mode bits, so it never warns there.
func tooOpen(path string) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().Perm()&0o077 != 0
}

func contextNames(cfg Config) string {
	names := make([]string, 0, len(cfg.Contexts))
	for n := range cfg.Contexts {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// viewConfig prints the contexts, with the inline token redacted.
func viewConfig(w io.Writer, path string, cfg Config, exists bool) {
	if !exists {
		fmt.Fprintf(w, "no config file at %s\n", path)
		return
	}
	fmt.Fprintf(w, "# %s\n", path)
	if len(cfg.Contexts) == 0 {
		fmt.Fprintln(w, "no contexts")
		return
	}
	names := strings.Split(contextNames(cfg), ", ")
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CURRENT\tNAME\tCONTROLLER\tTOKEN")
	for _, n := range names {
		c := cfg.Contexts[n]
		mark := ""
		if n == cfg.CurrentContext {
			mark = "*"
		}
		tok := "-"
		switch {
		case c.Token != "":
			tok = "(in file, hidden)"
		case c.TokenEnv != "":
			tok = "$" + c.TokenEnv
		case c.TokenFile != "":
			tok = "file " + c.TokenFile
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", mark, n, c.Controller, tok)
	}
	tw.Flush()
}

// useContext sets current-context in the file at path, editing the YAML in place
// so comments and the rest of the file are kept.
func useContext(path, name string) error {
	cfg, exists, err := loadConfig(path)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("no config file at %s", path)
	}
	if _, ok := cfg.Contexts[name]; !ok {
		return fmt.Errorf("context %q not found in %s (have: %s)", name, path, contextNames(cfg))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	root := doc.Content[0]
	set := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "current-context" {
			root.Content[i+1].SetString(name)
			set = true
		}
	}
	if !set {
		k, v := &yaml.Node{}, &yaml.Node{}
		k.SetString("current-context")
		v.SetString(name)
		root.Content = append([]*yaml.Node{k, v}, root.Content...)
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
