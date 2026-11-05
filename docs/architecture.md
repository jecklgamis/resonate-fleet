# Architecture

resonate-fleet is three small Go programs:

- **resonate-worker** is an HTTP server that runs `resonate run` as a subprocess,
  one per submitted task. Everything below up to *Components* is about the worker.
- **resonate-controller** keeps a registry of workers (they register and send
  heartbeats) and fans a scenario out to them over the workers' public API.
- **resonatectl** is a command-line client for the controller.

```
resonatectl ──▶ controller ──POST /task──▶ worker ┐
                    ▲  ▲                  worker ├─▶ resonate run
                    │  └─ heartbeats ──── worker ┘
                    └──── reports proxied back from the workers
```

You can use a worker without a controller. The worker's own flow:

```
client ──POST /task──▶ worker ──▶ workspace/<id>/scenario.yaml
   ▲                      │
   │                      └─ go Runner.Run ─▶ resonate run scenario.yaml
   │                                              │ writes
   └──GET /task/{id}[/report.html|...]◀── report.html, report.json,
                                          results.jsonl, log.txt
```

## Repository Layout

```
cmd/
  resonate-worker/        the worker (this page's request flow and component table)
  resonate-controller/    the controller
  resonatectl/            the CLI
deployment/
  docker/{worker,controller}/Dockerfile
  k8s/helm/{worker,controller}/        one Helm chart each, plus a Makefile for both
scripts/                  run-local-worker.sh, run-local-controller.sh
docs/                     this site
examples/                 a sample scenario
```

The three programs share no code; each is its own `package main`, and they talk
only over HTTP.

## Request Flow

1. `POST /task` takes the scenario from a multipart upload, or fetches it from an
   allowlisted `scenarioUrl`. It makes `WORKSPACE_DIR/<id>/`, saves the bytes as
   `scenario.yaml` (hashing them), records a `queued` task (and its `task.json`),
   and returns the id.
2. A goroutine runs `resonate run scenario.yaml` in that directory with
   stdout/stderr going to `log.txt`, marking the task `running`, then `done` or
   `failed` from the exit code.
3. `GET` endpoints read the in-memory task for status, and serve report files
   straight from the task directory.

## Components

| File | Role |
| --- | --- |
| `cmd/resonate-worker/main.go` | Reads the environment, loads saved tasks, wires the handler chain (request log → auth → routes), registers with a controller, shuts down gracefully |
| `cmd/resonate-worker/server.go` | HTTP handlers, bearer-token auth, request logging |
| `cmd/resonate-worker/source.go` | Fetching scenarios by URL: the allowlist, redirect checks, size cap |
| `cmd/resonate-worker/files.go` | Uploaded data files: name and size limits, and the check that a scenario's file paths stay inside its workspace |
| `cmd/resonate-worker/task.go` | The `Task` and `Store` types, `task.json` persistence, and the `Runner` that executes resonate |
| `cmd/resonate-worker/artifacts.go` | Uploading a finished task's files to S3 |
| `cmd/resonate-worker/register.go` | Registering with a controller: heartbeat and deregistration |
| `cmd/resonate-controller` | The controller: workers register with it and it fans a scenario out to them over the public API |
| `cmd/resonatectl` | The CLI for the controller: run a scenario, list workers |

## Design Notes

- **A subprocess, not a library.** The worker shells out to the `resonate` binary
  rather than importing it, so any resonate scenario works unchanged and the
  worker needs no rebuild when resonate gains features.
- **The filesystem is the database.** Reports are files, and so is task state.
  There is no external store to deploy; the cost is that state is per-worker.
- **A fetched scenario is copied, not referenced.** The task runs the snapshot in its
  own workspace, so a changed URL can't change what a past task ran.
- **Fetching is deny-by-default.** The worker only fetches from `SCENARIO_SOURCES`,
  because an open fetcher is a way to reach internal services.
- **The `Store` is in memory** and rebuilt from `task.json` files at startup.
- **Task state is mutex-guarded.** Handlers read a `snapshot` copy, so a poll never
  races the goroutine finishing the run.
- **Auth wraps everything except `/healthz`**, and request logging sits outside
  auth so rejected requests are logged too.

## What This Isn't

- **No combining of reports.** The controller runs a scenario on every worker and
  hands back each worker's report, but turning five reports into one number is on
  you (sum the counts; never average percentiles). It also doesn't schedule, queue
  or retry, and its registry and runs live in memory only.
- **No database.** Task state is a JSON file per task. Listing is a walk of the
  in-memory map, fine for thousands of tasks, not for millions.
- **No resumption.** A task interrupted by a restart is marked `failed`, not
  re-run.
- **No queue or concurrency cap.** See [Operations](operations.md#concurrency).
- **No notifications.** Reports stay on the worker's disk unless you set
  `S3_BUCKET`, which uploads each finished task's files to S3. There are no
  notifier events.
