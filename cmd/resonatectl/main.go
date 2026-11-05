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
  resonatectl version                                    print the version

flags (both commands):
  --controller URL   resonate-controller to talk to (default $RESONATE_CONTROLLER)
  --label k=v        only workers with this label (repeatable)
the controller's token is read from $CONTROLLER_TOKEN.`

// version is stamped at release time with -ldflags "-X main.version=<tag>".
var version = "dev"

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(runCmd(os.Args[2:]))
	case "workers":
		os.Exit(workersCmd(os.Args[2:]))
	case "version", "-v", "--version":
		fmt.Println("resonatectl", version)
	case "-h", "--help", "help":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
}

// commonFlags registers --controller and --label on fs.
func commonFlags(fs *flag.FlagSet) (controller *string, labels *multiFlag) {
	labels = &multiFlag{}
	controller = fs.String("controller", os.Getenv("RESONATE_CONTROLLER"), "URL of the resonate-controller (default $RESONATE_CONTROLLER)")
	fs.Var(labels, "label", "Only workers with this label, key=value (repeatable)")
	return controller, labels
}

func newClient(controller string) (*ControllerClient, error) {
	if controller == "" {
		return nil, fmt.Errorf("no controller: pass --controller or set $RESONATE_CONTROLLER")
	}
	return newControllerClient(controller, os.Getenv("CONTROLLER_TOKEN")), nil
}

func runCmd(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	controller, labels := commonFlags(fs)
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
	c, err := newClient(*controller)
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
	results, err := runViaController(ctx, c, scenario, dataFiles, *labels, *outDir, *poll)
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
	controller, labels := commonFlags(fs)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage); fs.PrintDefaults() }
	fs.Parse(args)
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	c, err := newClient(*controller)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ws, err := c.Workers(ctx, *labels)
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
