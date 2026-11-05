// Command resonatectl runs one scenario on every resonate-worker registered
// with a resonate-controller, and pulls back each worker's report. It talks to
// the controller only: the controller fans the run out, so this tool needs a
// route to the controller and nothing else. Reports are never combined - each
// worker reports on its own slice of traffic.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const usage = `usage:
  resonatectl run [flags] <scenario.yaml|scenario-url>   run a scenario on the registered workers
  resonatectl workers [flags]                            list the registered workers
  resonatectl config view                                show the contexts in the config file
  resonatectl config use <context>                       switch the current context
  resonatectl version                                    print the version

flags (run, workers):
  --context NAME     use this context from the config file
  --controller URL   resonate-controller to talk to (overrides the context's)
  --config FILE      config file (default ~/.config/resonatectl/config.yaml, or $RESONATECTL_CONFIG)
  --label k=v        only workers with this label (repeatable)
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
	var dataFiles multiFlag
	fs.Var(&dataFiles, "data", "A feeder/body file to upload with the scenario (repeatable; not with a scenario URL)")
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
	results, err := runViaController(ctx, c, scenario, dataFiles, *cf.labels, *outDir, *poll)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	printTable(os.Stdout, results)
	fmt.Printf("reports in %s\n", *outDir)
	if anyFailed(results) {
		return 1
	}
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

// configCmd is `resonatectl config view` and `resonatectl config use <context>`.
func configCmd(args []string) int {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	file := fs.String("config", "", "Config file (default ~/.config/resonatectl/config.yaml, or $RESONATECTL_CONFIG)")
	// Accepted so `resonatectl --context prod config view` doesn't fail, but a
	// config command only ever reads the file.
	fs.String("context", "", "ignored by config")
	fs.String("controller", "", "ignored by config")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage); fs.PrintDefaults() }

	positionals, flagArgs := splitPositionals(args)
	fs.Parse(flagArgs)

	path := *file
	if path == "" {
		path = os.Getenv("RESONATECTL_CONFIG")
	}
	if path == "" {
		path = defaultConfigPath(os.Getenv)
	}
	switch {
	case len(positionals) == 1 && positionals[0] == "view":
		cfg, exists, err := loadConfig(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		viewConfig(os.Stdout, path, cfg, exists)
		return 0
	case len(positionals) == 2 && positionals[0] == "use":
		if err := useContext(path, positionals[1]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		fmt.Printf("current context is now %q\n", positionals[1])
		return 0
	}
	fs.Usage()
	return 2
}

// splitPositionals separates the non-flag arguments from the flags (and their
// values), in any order. Every flag here takes a value.
func splitPositionals(args []string) (positionals, flagArgs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
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
