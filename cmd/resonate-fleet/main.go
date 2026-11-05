// Command resonate-fleet fans one scenario out to several resonate-worker
// instances at once, polls each independently, and pulls back each worker's
// report — a client-side dispatcher over resonate-worker's HTTP API, for the
// "same scenario from several regions/AZs, independent reports" model (see
// the main README's "What this isn't": no merging, by design).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const usage = "usage: resonate-fleet run --workers workers.yaml [--out dir] [--poll 2s] <scenario.yaml|scenario-url>"

func main() {
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	workersPath := fs.String("workers", "", "Path to a workers.yaml listing the resonate-worker instances to fan out to (required)")
	outDir := fs.String("out", "", "Directory to write each worker's reports into, one subdirectory per worker (default: ./fleet-run-<timestamp>)")
	poll := fs.Duration("poll", 2*time.Second, "How often to poll each worker's task status")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, usage); fs.PrintDefaults() }
	// The scenario path can be given anywhere on the command line, not just
	// last — flag.FlagSet otherwise stops parsing at the first non-flag
	// argument, which would silently leave --workers/--out/--poll unparsed if
	// the scenario path came before them.
	scenarioPath, flagArgs := extractPositional(os.Args[2:])
	fs.Parse(flagArgs)

	if scenarioPath == "" || fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	if *workersPath == "" {
		fmt.Fprintln(os.Stderr, "error: --workers is required")
		os.Exit(2)
	}
	if *outDir == "" {
		*outDir = fmt.Sprintf("fleet-run-%d", time.Now().Unix())
	}

	if !isScenarioURL(scenarioPath) {
		if _, err := os.Stat(scenarioPath); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
	cfg, err := LoadFleetConfig(*workersPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cfg.Resolve(ctx, DNSLookup); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("submitting %s to %d worker(s), writing reports under %s\n", scenarioPath, len(cfg.Workers), *outDir)
	results := runFleet(ctx, cfg, scenarioPath, *outDir, *poll)
	printTable(os.Stdout, results)

	if anyFailed(results) {
		os.Exit(1)
	}
}

// extractPositional splits args into the one positional argument (the
// scenario path) and the rest, which are handed to flag.FlagSet.Parse as-is.
// It exists because flag.FlagSet stops parsing at the first argument that
// doesn't start with "-", so with every flag required after the scenario
// path as flag.FlagSet alone would demand, a perfectly reasonable
// `resonate-fleet run scenario.yaml --workers workers.yaml` would silently
// leave --workers unparsed instead of erroring. Every flag this command
// defines takes a value (none are booleans), so "a token starting with -
// consumes the next token as its value, unless joined with =" is unambiguous
// here; a future boolean flag would need this taught about it explicitly.
func extractPositional(args []string) (positional string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 0 && a[0] == '-' {
			rest = append(rest, a)
			if !containsEquals(a) && i+1 < len(args) {
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

func containsEquals(s string) bool {
	for _, c := range s {
		if c == '=' {
			return true
		}
	}
	return false
}
