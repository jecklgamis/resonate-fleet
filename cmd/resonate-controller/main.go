// Command resonate-controller keeps a registry of resonate-workers that register
// themselves, and runs a scenario on all of them (or a labelled subset) through
// one API: the caller needs only the controller's URL, not a route to each
// worker. Workers still report independently; nothing is combined.
package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

// version is stamped at release time with -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	addr := envOr("ADDR", ":58090")
	token := os.Getenv("CONTROLLER_TOKEN") // "" disables auth - local use only
	ttl := envDuration(30*time.Second, os.Getenv("WORKER_TTL"))
	poll := envDuration(2*time.Second, os.Getenv("POLL_INTERVAL"))
	timeout := envDuration(time.Hour, os.Getenv("RUN_TIMEOUT"))

	srv := &Server{Registry: NewRegistry(ttl), Runs: NewRuns(poll, timeout)}
	go srv.ReapLoop(max(ttl/3, time.Second), nil)
	mux := http.NewServeMux()
	srv.Routes(mux)

	if token == "" {
		log.Println("warning: CONTROLLER_TOKEN is not set - every endpoint is open to anyone who can reach this address")
	}
	log.Printf("resonate-controller %s listening on %s (worker ttl=%s)", version, addr, ttl)
	log.Fatal(http.ListenAndServe(addr, withAuth(token, mux)))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
