# Architecture

resonate-worker is a small Go HTTP server that runs `resonate run` as a
subprocess, one per submitted task.

```
client ──POST /task──▶ worker ──▶ workspace/<id>/scenario.yaml
   ▲                      │
   │                      └─ go Runner.Run ─▶ resonate run scenario.yaml
   │                                              │ writes
   └──GET /task/{id}[/report.html|...]◀── report.html, report.json,
                                          results.jsonl, log.txt
```

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
| `main.go` | Reads the environment, loads saved tasks, wires the handler chain (request log → auth → routes) |
| `server.go` | HTTP handlers, bearer-token auth, request logging |
| `source.go` | Fetching scenarios by URL: the allowlist, redirect checks, size cap |
| `task.go` | The `Task` and `Store` types, `task.json` persistence, and the `Runner` that executes resonate |
| `cmd/resonate-fleet` | The separate fan-out CLI; it talks to workers only over the public API |

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

- **No fleet coordination.** Submitting the same scenario to five workers in five
  regions and combining their reports into one number is on you. A worker is
  deliberately just the one-node piece; [`resonate-fleet`](fleet.md) automates the
  submitting and collecting, not the combining.
- **No database.** Task state is a JSON file per task. Listing is a walk of the
  in-memory map, fine for thousands of tasks, not for millions.
- **No resumption.** A task interrupted by a restart is marked `failed`, not
  re-run.
- **No queue or concurrency cap.** See [Operations](operations.md#concurrency).
- **No artifact upload or notifications.** Reports stay on the worker's disk;
  there is no S3/SNS upload or notifier events, on purpose, until the core shape
  has proven itself.
