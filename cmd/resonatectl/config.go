package main

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
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

// --- editing -----------------------------------------------------------------
//
// set-context and delete-context edit the file as a YAML node tree, not by
// decoding into Config and re-encoding it, so comments and layout survive.

// contextUpdate is what `config set-context` changes. A nil Controller leaves
// the controller alone; TokenKind is "" (leave the token alone), "token",
// "token_env" or "token_file".
type contextUpdate struct {
	Controller *string
	TokenKind  string
	TokenValue string
	Use        bool
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapSet(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	k := &yaml.Node{}
	k.SetString(key)
	m.Content = append(m.Content, k, v)
}

func mapDel(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			// The parser hangs a comment above the first key on that key, so
			// deleting the key would delete the comment; pass it to the next key.
			if hc := m.Content[i].HeadComment; hc != "" && i+2 < len(m.Content) {
				next := m.Content[i+2]
				if next.HeadComment == "" {
					next.HeadComment = hc
				} else {
					next.HeadComment = hc + "\n" + next.HeadComment
				}
			}
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func scalar(s string) *yaml.Node {
	n := &yaml.Node{}
	n.SetString(s)
	return n
}

func newMapping() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }

// loadDoc parses the file at path into a node tree whose root is a mapping. A
// missing file gives an empty one (created is true).
func loadDoc(path string) (doc *yaml.Node, root *yaml.Node, created bool, err error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		root = newMapping()
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}, root, true, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	doc = &yaml.Node{}
	if err := yaml.Unmarshal(b, doc); err != nil {
		return nil, nil, false, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(doc.Content) == 0 { // an empty file
		root = newMapping()
		doc = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
		return doc, root, false, nil
	}
	root = doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil, false, fmt.Errorf("%s: want a mapping at the top level", path)
	}
	return doc, root, false, nil
}

// saveDoc writes doc to path atomically, after checking that the result still
// loads as a valid config. New files (and their directory) are private: 0600
// and 0700. An existing file keeps its mode.
func saveDoc(path string, doc *yaml.Node) error {
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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
	if _, _, err := loadConfig(tmp); err != nil { // never replace a good file with a bad one
		os.Remove(tmp)
		return fmt.Errorf("refusing to write an invalid config: %w", err)
	}
	return os.Rename(tmp, path)
}

// setContext creates or updates a context. Creating one needs a controller.
// Choosing a token source replaces any other, since a context holds at most
// one. The first context in a file becomes current; Use makes this one current.
func setContext(path, name string, u contextUpdate) (created bool, err error) {
	if name == "" {
		return false, fmt.Errorf("a context name is required")
	}
	if u.Controller != nil {
		if err := checkControllerURL(*u.Controller); err != nil {
			return false, err
		}
	}
	doc, root, _, err := loadDoc(path)
	if err != nil {
		return false, err
	}
	ctxs := mapGet(root, "contexts")
	if ctxs == nil || ctxs.Kind != yaml.MappingNode {
		ctxs = newMapping()
		mapSet(root, "contexts", ctxs)
	}
	ctx := mapGet(ctxs, name)
	if ctx == nil || ctx.Kind != yaml.MappingNode {
		if u.Controller == nil {
			return false, fmt.Errorf("context %q does not exist; give --controller to create it", name)
		}
		created = true
		ctx = newMapping()
		mapSet(ctxs, name, ctx)
	}
	if u.Controller != nil {
		mapSet(ctx, "controller", scalar(*u.Controller))
	}
	if u.TokenKind != "" {
		for _, k := range []string{"token", "token_env", "token_file"} {
			mapDel(ctx, k)
		}
		mapSet(ctx, u.TokenKind, scalar(u.TokenValue))
	}
	cur := mapGet(root, "current-context")
	if u.Use || cur == nil || cur.Value == "" {
		mapSet(root, "current-context", scalar(name))
	}
	return created, saveDoc(path, doc)
}

// deleteContext removes a context; it reports whether it was the current one,
// in which case current-context is cleared.
func deleteContext(path, name string) (wasCurrent bool, err error) {
	cfg, exists, err := loadConfig(path)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, fmt.Errorf("no config file at %s", path)
	}
	if _, ok := cfg.Contexts[name]; !ok {
		return false, fmt.Errorf("context %q not found in %s (have: %s)", name, path, contextNames(cfg))
	}
	doc, root, _, err := loadDoc(path)
	if err != nil {
		return false, err
	}
	mapDel(mapGet(root, "contexts"), name)
	if cfg.CurrentContext == name {
		mapDel(root, "current-context")
		wasCurrent = true
	}
	return wasCurrent, saveDoc(path, doc)
}

func checkControllerURL(u string) error {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || p.User != nil {
		return fmt.Errorf("invalid controller %q: want http(s)://host[:port]", u)
	}
	return nil
}
