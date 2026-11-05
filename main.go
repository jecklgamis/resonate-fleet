// Command resonate-worker is an HTTP wrapper around `resonate run`, modeled on
// jecklgamis/gatling-server's "push a scenario, poll a taskId, fetch the
// report" shape. Push a scenario file via HTTP; it runs resonate against it as
// a subprocess and serves back the report, the raw results, and the console
// log. It is a single worker: it executes whatever is submitted to it and
// keeps no relationship with the other workers (see README's
// "What this isn't").
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	addr := envOr("ADDR", ":58080")
	workspace := envOr("WORKSPACE_DIR", "workspace")
	resonateBin := envOr("RESONATE_BIN", "resonate")
	token := os.Getenv("API_TOKEN") // "" disables auth — fine for local use, not for anything reachable over a network

	if err := os.MkdirAll(workspace, 0o755); err != nil {
		log.Fatalf("creating workspace dir %q: %v", workspace, err)
	}

	store, err := LoadStore(workspace)
	if err != nil {
		log.Fatalf("loading tasks from %q: %v", workspace, err)
	}
	log.Printf("loaded %d task(s) from %s", len(store.List()), workspace)

	var allow []string
	if v := os.Getenv("SCENARIO_SOURCES"); v != "" {
		allow = strings.Split(v, ",")
	}
	fetcher, err := NewSourceFetcher(allow, os.Getenv("SCENARIO_SOURCE_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	if len(fetcher.Allow) > 0 {
		log.Printf("scenario URLs enabled for: %s", strings.Join(fetcher.Allow, ", "))
	}

	runner := &Runner{ResonateBin: resonateBin}
	if bucket := os.Getenv("S3_BUCKET"); bucket != "" {
		s3store, err := NewS3Store(context.Background(), bucket, os.Getenv("S3_PREFIX"), os.Getenv("S3_ENDPOINT"))
		if err != nil {
			log.Fatal(err)
		}
		runner.Artifacts = s3store
		log.Printf("uploading task artifacts to s3://%s/%s", bucket, s3store.Prefix)
	}

	srv := &Server{
		Fetcher:   fetcher,
		Store:     store,
		Runner:    runner,
		Workspace: workspace,
	}

	// SIGTERM is how Kubernetes (and docker stop) ask a process to leave.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reg, err := registrarFromEnv(addr, token)
	if err != nil {
		log.Fatal(err)
	}
	heartbeatDone := make(chan struct{})
	if reg != nil {
		log.Printf("registering with controller %s as %q at %s", reg.ControllerURL, reg.Name, reg.URL)
		go func() { reg.Run(ctx); close(heartbeatDone) }()
	} else {
		close(heartbeatDone)
	}

	mux := http.NewServeMux()
	srv.Routes(mux)

	handler := withRequestLog(withAuth(token, mux))

	if token == "" {
		log.Println("warning: API_TOKEN is not set — every endpoint is open to anyone who can reach this address")
	}
	log.Printf("resonate-worker listening on %s (workspace=%s, resonate=%s)", addr, workspace, resonateBin)

	server := &http.Server{Addr: addr, Handler: handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()

	select {
	case err := <-serveErr:
		log.Fatal(err)
	case <-ctx.Done():
	}
	log.Printf("shutting down")

	// Leave the controller first so no new run is sent here, then stop serving.
	// A task still running when the process exits is marked failed on the next
	// start (see LoadStore).
	<-heartbeatDone
	if reg != nil {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := reg.Deregister(dctx); err != nil {
			log.Printf("deregistering from controller: %v", err)
		} else {
			log.Printf("deregistered from controller")
		}
		cancel()
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(sctx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
