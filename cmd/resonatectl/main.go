// Command resonatectl runs one scenario on every resonate-worker registered
// with a resonate-controller, and pulls back each worker's report. It talks to
// the controller only: the controller fans the run out, so this tool needs a
// route to the controller and nothing else. Each worker's report is kept as is,
// and a fleet total is computed over all of them (see aggregate.go).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const usage = `usage:
  resonatectl run [flags] <scenario.yaml|scenario-url>   run a scenario on the registered workers
  resonatectl watch [flags] <run-id>                     follow a run live: rate, errors, latency per worker
  resonatectl abort [flags] <run-id>                     stop a run on all its workers
  resonatectl workers [flags]                            list the registered workers
  resonatectl config view                                show the contexts in the config file
  resonatectl config use <context>                       switch the current context
  resonatectl config set-context <name> [--controller URL] [--token-stdin | --token T |
                                  --token-env VAR | --token-file F] [--use]   create or update a context
  resonatectl config delete-context <name>               remove a context
  resonatectl version                                    print the version

flags (run, workers):
  --context NAME     use this context from the config file
  --controller URL   resonate-controller to talk to (overrides the context's)
  --config FILE      config file (default ~/.config/resonatectl/config.yaml, or $RESONATECTL_CONFIG)
  --label k=v        only workers with this label (repeatable)
  --workers N|P%     (run) a random N, or P%, of those workers; default all
  --total-rate R     (run) make R req/s the whole fleet's peak rate, not each worker's:
                     the scenario's rate and users are scaled to R and split across
                     the workers (a scenario file only, not a URL)
  --start-delay D    (run) every worker waits D (default 5s) so they start together; 0
                     starts each as it receives the scenario. Needs workers' clocks in
                     sync (NTP); a drifting clock is reported.
  --live auto|on|off (run) live rate and latency line while the run goes; auto = on a
                     terminal only. Ctrl-C there aborts the run; in watch it only detaches.
  --threshold EXPR   (run) fail unless the fleet total meets it, e.g. p95<300ms,
                     success_rate>=99%, error_rate<1%, rps>=100 (repeatable)

run prints each worker's row, then a fleet total computed over every worker's
requests, and writes it to <out>/aggregate.json. Ctrl-C aborts the run on the
workers and still reports what they finished. Exit status: 0 success, 1 a worker
failed or errored, 3 only a --threshold failed, 2 usage error.
settings, highest first: flags, then $RESONATE_CONTROLLER and $CONTROLLER_TOKEN,
then the config file's current context.`

// version is stamped at release time with -ldflags "-X main.version=<tag>".
var version = "dev"

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	args := hoistGlobalFlags(os.Args[1:])
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "run":
		os.Exit(runCmd(args[1:]))
	case "abort":
		os.Exit(abortCmd(args[1:]))
	case "watch":
		os.Exit(watchCmd(args[1:]))
	case "workers":
		os.Exit(workersCmd(args[1:]))
	case "config":
		os.Exit(configCmd(args[1:]))
	case "version", "-v", "--version":
		fmt.Println("resonatectl", version)
	case "-h", "--help", "help":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s\n", args[0], usage)
		os.Exit(2)
	}
}

// clientFlags are the flags that choose a controller; they are shared by run
// and workers.
type clientFlags struct {
	controller, context, config *string
	labels                      *multiFlag
}

func commonFlags(fs *flag.FlagSet) clientFlags {
	labels := &multiFlag{}
	fs.Var(labels, "label", "Only workers with this label, key=value (repeatable)")
	return clientFlags{
		controller: fs.String("controller", "", "URL of the resonate-controller (overrides the context's)"),
		context:    fs.String("context", "", "Context to use from the config file"),
		config:     fs.String("config", "", "Config file (default ~/.config/resonatectl/config.yaml, or $RESONATECTL_CONFIG)"),
		labels:     labels,
	}
}

// client resolves which controller to talk to from the flags, the environment
// and the config file, and warns about anything fishy on stderr.
func (cf clientFlags) client() (*ControllerClient, error) {
	conn, err := resolveConnection(connectionInput{
		ControllerFlag: *cf.controller,
		ContextFlag:    *cf.context,
		ConfigFlag:     *cf.config,
		Getenv:         os.Getenv,
	})
	if err != nil {
		return nil, err
	}
	for _, w := range conn.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if conn.Controller == "" {
		return nil, fmt.Errorf("no controller: pass --controller or --context, set $RESONATE_CONTROLLER, or set current-context in the config file")
	}
	return newControllerClient(conn.Controller, conn.Token), nil
}

func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cf := commonFlags(fs)
	var dataFiles, thresholdExprs multiFlag
	fs.Var(&thresholdExprs, "threshold", "Fail unless the fleet total meets this, like p95<300ms or success_rate>=99% (repeatable)")
	liveMode := fs.String("live", "auto", "Show the run's live rate and latency while it goes: auto (on a terminal), on, or off")
	fs.Var(&dataFiles, "data", "A feeder/body file to upload with the scenario (repeatable; not with a scenario URL)")
	workers := fs.String("workers", "", "Run on a random subset of the matching workers: a count (4) or a percentage (25%); default all")
	totalRate := fs.Float64("total-rate", 0, "Make this the fleet's peak request rate: scale the scenario's load to it and split it across the workers (default: every worker runs the scenario as written)")
	startDelay := fs.Duration("start-delay", 5*time.Second, "Have every worker wait this long before starting, so they begin together; 0 starts each as soon as it has the scenario (a single worker never waits)")
	outDir := fs.String("out", "", "Directory for each worker's reports, one subdirectory per worker (default ./resonatectl-run-<timestamp>)")
	poll := fs.Duration("poll", 2*time.Second, "How often to check the run's progress")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage); fs.PrintDefaults() }
	// The scenario can be given anywhere on the command line, not just last:
	// flag.FlagSet otherwise stops parsing at the first non-flag argument and
	// would silently leave later flags unparsed.
	scenario, flagArgs := extractPositional(args)
	fs.Parse(flagArgs)
	if scenario == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	if *totalRate < 0 || (*totalRate == 0 && flagSet(fs, "total-rate")) {
		fmt.Fprintln(os.Stderr, "error: --total-rate must be a number above 0")
		return 2
	}
	thresholds, err := parseThresholds(thresholdExprs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	c, err := cf.client()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if !isScenarioURL(scenario) {
		if _, err := os.Stat(scenario); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	}
	if *outDir == "" {
		*outDir = fmt.Sprintf("resonatectl-run-%d", time.Now().Unix())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var live *liveDisplay
	switch *liveMode {
	case "auto":
		if isTerminal(os.Stderr) {
			live = newLiveDisplay(os.Stderr, false)
		}
	case "on":
		live = newLiveDisplay(os.Stderr, false)
	case "off":
	default:
		fmt.Fprintf(os.Stderr, "error: --live must be auto, on or off, not %q\n", *liveMode)
		return 2
	}
	outcome, err := runViaController(ctx, c, scenario, startOptions{DataFiles: dataFiles, Labels: *cf.labels, Workers: *workers, TotalRate: *totalRate, StartDelay: *startDelay}, *outDir, *poll, live)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	results := outcome.Results
	printTable(os.Stdout, results)
	for _, r := range results {
		if r.Warning != "" {
			fmt.Fprintln(os.Stderr, "warning:", r.Warning)
		}
	}
	agg := buildAggregate(*outDir, results)
	if agg != nil {
		printAggregate(os.Stdout, agg)
		if err := writeAggregate(filepath.Join(*outDir, "aggregate.json"), agg); err != nil {
			fmt.Fprintln(os.Stderr, "warning: writing aggregate.json:", err)
		}
	}
	checks := evalThresholds(thresholds, agg)
	printThresholds(os.Stdout, checks)
	fmt.Printf("\nreports in %s\n", *outDir)
	if agg != nil {
		reportPath := filepath.Join(*outDir, "report.html")
		err := writeFleetReport(reportPath, reportInput{
			RunID: outcome.ID, Scenario: filepath.Base(scenario), Generated: time.Now(), Version: version,
			Aggregate: agg, Results: results, Thresholds: checks, Series: buildSeries(*outDir, results),
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "warning: writing report.html:", err)
		} else {
			fmt.Printf("fleet report: %s\n", reportPath)
		}
	}
	switch {
	case anyFailed(results):
		return 1
	case thresholdsFailed(checks):
		return 3
	}
	return 0
}

// flagSet reports whether the flag was given on the command line.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// watchCmd follows a run that is already going (started by this tool or anything
// else), drawing a per-worker table until it ends. Ctrl-C only stops watching; the
// run keeps going.
func watchCmd(args []string) int {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	cf := commonFlags(fs)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage); fs.PrintDefaults() }
	id, flagArgs := extractPositional(args)
	fs.Parse(flagArgs)
	if id == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	c, err := cf.client()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	last, err := newLiveDisplay(os.Stdout, true).follow(ctx, c, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	switch {
	case last.Status == "done":
		return 0
	case last.Status == "running": // interrupted
		fmt.Fprintf(os.Stderr, "\nstopped watching; run %s is still going (resonatectl abort %s stops it)\n", id, id)
		return 0
	}
	return 1
}

func abortCmd(args []string) int {
	fs := flag.NewFlagSet("abort", flag.ExitOnError)
	cf := commonFlags(fs)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage); fs.PrintDefaults() }
	id, flagArgs := extractPositional(args)
	fs.Parse(flagArgs)
	if id == "" || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	c, err := cf.client()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	names, err := c.Abort(ctx, id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if len(names) == 0 {
		fmt.Printf("run %s has nothing left to stop\n", id)
		return 0
	}
	fmt.Printf("run %s: asked %d worker(s) to stop: %s\n", id, len(names), strings.Join(names, ", "))
	return 0
}

func workersCmd(args []string) int {
	fs := flag.NewFlagSet("workers", flag.ExitOnError)
	cf := commonFlags(fs)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage); fs.PrintDefaults() }
	fs.Parse(args)
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	c, err := cf.client()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ws, err := c.Workers(ctx, *cf.labels)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if len(ws) == 0 {
		fmt.Println("no registered workers")
		return 0
	}
	printWorkers(os.Stdout, ws, time.Now())
	return 0
}

// configCmd handles `resonatectl config view|use|set-context|delete-context`.
func configCmd(args []string) int {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	file := fs.String("config", "", "Config file (default ~/.config/resonatectl/config.yaml, or $RESONATECTL_CONFIG)")
	controller := fs.String("controller", "", "set-context: the controller URL")
	token := fs.String("token", "", "set-context: the token, stored in the file (it lands in your shell history; prefer --token-stdin)")
	tokenStdin := fs.Bool("token-stdin", false, "set-context: read the token from standard input")
	tokenEnv := fs.String("token-env", "", "set-context: read the token from this environment variable at run time")
	tokenFile := fs.String("token-file", "", "set-context: read the token from this file at run time")
	use := fs.Bool("use", false, "set-context: also make this the current context")
	fs.String("context", "", "ignored by config")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage); fs.PrintDefaults() }

	positionals, flagArgs := splitPositionals(args)
	fs.Parse(flagArgs)
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	path := *file
	if path == "" {
		path = os.Getenv("RESONATECTL_CONFIG")
	}
	if path == "" {
		path = defaultConfigPath(os.Getenv)
	}
	fail := func(err error) int { fmt.Fprintln(os.Stderr, "error:", err); return 1 }
	if len(positionals) == 0 {
		fs.Usage()
		return 2
	}
	sub, rest := positionals[0], positionals[1:]

	switch {
	case sub == "view" && len(rest) == 0:
		cfg, exists, err := loadConfig(path)
		if err != nil {
			return fail(err)
		}
		viewConfig(os.Stdout, path, cfg, exists)
		return 0

	case sub == "use" && len(rest) == 1:
		if err := useContext(path, rest[0]); err != nil {
			return fail(err)
		}
		fmt.Printf("current context is now %q\n", rest[0])
		return 0

	case sub == "delete-context" && len(rest) == 1:
		wasCurrent, err := deleteContext(path, rest[0])
		if err != nil {
			return fail(err)
		}
		fmt.Printf("deleted context %q from %s\n", rest[0], path)
		if wasCurrent {
			fmt.Println("it was the current context; pick another with: resonatectl config use <name>")
		}
		return 0

	case sub == "set-context" && len(rest) == 1:
		var u contextUpdate
		if set["controller"] {
			u.Controller = controller
		}
		sources := 0
		for _, on := range []bool{set["token"], *tokenStdin, set["token-env"], set["token-file"]} {
			if on {
				sources++
			}
		}
		if sources > 1 {
			return fail(fmt.Errorf("give at most one of --token, --token-stdin, --token-env and --token-file"))
		}
		switch {
		case set["token"]:
			u.TokenKind, u.TokenValue = "token", *token
		case *tokenStdin:
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fail(err)
			}
			u.TokenKind, u.TokenValue = "token", strings.TrimSpace(string(b))
		case set["token-env"]:
			u.TokenKind, u.TokenValue = "token_env", *tokenEnv
		case set["token-file"]:
			u.TokenKind, u.TokenValue = "token_file", *tokenFile
		}
		if u.TokenKind != "" && u.TokenValue == "" {
			return fail(fmt.Errorf("the token is empty"))
		}
		u.Use = *use
		created, err := setContext(path, rest[0], u)
		if err != nil {
			return fail(err)
		}
		verb := "updated"
		if created {
			verb = "created"
		}
		fmt.Printf("context %q %s in %s\n", rest[0], verb, path)
		if cfg, _, err := loadConfig(path); err == nil {
			fmt.Printf("current context: %s\n", cfg.CurrentContext)
		}
		return 0
	}
	fs.Usage()
	return 2
}

// splitPositionals separates the non-flag arguments from the flags (and their
// values), in any order. Every flag takes a value except the booleans below.
func splitPositionals(args []string) (positionals, flagArgs []string) {
	boolFlags := map[string]bool{"use": true, "token-stdin": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
			if !strings.Contains(a, "=") && !boolFlags[name] && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		positionals = append(positionals, a)
	}
	return positionals, flagArgs
}

// extractPositional splits args into the one positional argument (the
// scenario) and the rest, which are handed to flag.FlagSet.Parse as-is. Every
// flag this command defines takes a value, so "a token starting with - consumes
// the next token as its value, unless joined with =" is unambiguous; a future
// boolean flag would need this taught about it explicitly.
func extractPositional(args []string) (positional string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 0 && a[0] == '-' {
			rest = append(rest, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if positional == "" {
			positional = a
		} else {
			rest = append(rest, a) // a second positional: let fs.NArg() catch it as an error
		}
	}
	return positional, rest
}

// hoistGlobalFlags lets the connection flags come before the subcommand, as in
// `resonatectl --context prod workers`, by moving them after it. Only
// --context, --config and --controller are hoisted; each takes a value.
func hoistGlobalFlags(args []string) []string {
	var global []string
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		name := strings.TrimLeft(strings.SplitN(args[i], "=", 2)[0], "-")
		if name != "context" && name != "config" && name != "controller" {
			break // not ours (-h, --version, ...): leave it for main to handle
		}
		global = append(global, args[i])
		if !strings.Contains(args[i], "=") && i+1 < len(args) {
			i++
			global = append(global, args[i])
		}
		i++
	}
	if len(global) == 0 || i >= len(args) {
		return args
	}
	rest := args[i:]
	return append(append([]string{rest[0]}, global...), rest[1:]...)
}
