// Command resonate-worker is an HTTP wrapper around `resonate run`, modeled on
// jecklgamis/gatling-server's "push a scenario, poll a taskId, fetch the
// report" shape. Push a scenario file via HTTP; it runs resonate against it as
// a subprocess and serves back the report, the raw results, and the console
// log. It is a single worker: it executes whatever is submitted to it and
// keeps no relationship with the other workers in a fleet (see README's
// "What this isn't").
package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	addr := envOr("ADDR", ":58080")
	workspace := envOr("WORKSPACE_DIR", "workspace")
	resonateBin := envOr("RESONATE_BIN", "resonate")
	token := os.Getenv("API_TOKEN") // "" disables auth — fine for local use, not for anything reachable over a network

	if err := os.MkdirAll(workspace, 0o755); err != nil {
		log.Fatalf("creating workspace dir %q: %v", workspace, err)
	}

	srv := &Server{
		Store:     NewStore(),
		Runner:    &Runner{ResonateBin: resonateBin},
		Workspace: workspace,
	}

	mux := http.NewServeMux()
	srv.Routes(mux)

	handler := withAuth(token, mux)

	if token == "" {
		log.Println("warning: API_TOKEN is not set — every endpoint is open to anyone who can reach this address")
	}
	log.Printf("resonate-worker listening on %s (workspace=%s, resonate=%s)", addr, workspace, resonateBin)
	log.Fatal(http.ListenAndServe(addr, handler))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
