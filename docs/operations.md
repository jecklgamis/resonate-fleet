# Operations

## Logging

The worker logs to stderr with timestamps: one line per request, and one per task
lifecycle event.

```
2026/11/05 11:46:42 POST /task -> 202 (1ms) from [::1]:58198
2026/11/05 11:46:42 task 9e8c725173ce4da2: submitted (scenario="scenario.yaml", from=[::1]:58198)
2026/11/05 11:46:42 task 9e8c725173ce4da2: running resonate run scenario.yaml
2026/11/05 11:46:52 task 9e8c725173ce4da2: done in 10.017s
2026/11/05 11:46:54 GET /task/nope -> 404 (0s) from [::1]:58213
2026/11/05 11:46:54 GET /task -> 401 (0s) from [::1]:58214
```

A failed task logs `task <id>: failed (exit 1, err=exit status 1) after 10.1s; see
<workspace>/<id>/log.txt`.

Request lines include rejected (`401`) requests. **Headers are never logged**, so
the bearer token stays out of the log. Polling adds one line per poll; filter on
`/task/` if that is noisy.

This is the worker's own log. A task's resonate output is separate — it is in
`log.txt`, served at `GET /task/{id}/log`.

## Persistence

Each task's state is saved as `task.json` in its workspace directory when it is
submitted, starts running and finishes. At startup the worker scans the workspace
and rebuilds its task list from those files, so after a restart:

- `GET /task` and `GET /task/{id}` still know about finished tasks
- their reports and logs are still served
- the startup line says how many tasks were loaded

State is written to a temp file and renamed into place, so a crash mid-write never
leaves a truncated `task.json`. A failed write is logged but not fatal; the
in-memory state stays correct.

Subdirectories without a readable `task.json` (for example, workspaces from before
persistence existed) are skipped, not loaded.

### Tasks that were running when the worker stopped

A `queued` or `running` task has a `resonate` subprocess that dies with the worker,
and nothing resumes it. On the next start the worker marks such tasks `failed`:

```json
{"status":"failed","exit_code":1,"error":"worker restarted before the task finished", ...}
```

Resubmit the scenario if you still want the run. Any partial report resonate may
have written is whatever is on disk.

### Keeping state across container restarts

`WORKSPACE_DIR` has to live on storage that outlives the container: a Docker
volume, or `persistence.enabled=true` in the Helm chart. See
[Deployment](deployment.md).

## Disk Usage

The worker never deletes a workspace. Each holds the scenario, the three reports,
the log, and `task.json`; `results.jsonl` is one line per request, so it grows with
`rate × duration`. Clear out old ones on whatever schedule suits you:

```sh
find "$WORKSPACE_DIR" -mindepth 1 -maxdepth 1 -type d -mtime +7 -exec rm -rf {} +
```

A deleted workspace is simply not loaded after the next restart. Don't delete one
while its task is still running.

## Concurrency

Every submission starts a `resonate` subprocess immediately. There is **no queue
and no concurrency cap**, so ten submissions are ten simultaneous load tests
sharing the worker's CPU, memory and network. That skews results (the load
generator becomes the bottleneck), so:

- run one scenario at a time per worker, or
- size the worker for the sum of everything you submit at once, or
- use more workers — see [Controller](controller.md).

## Security

| Concern | Guidance |
| --- | --- |
| Authentication | One shared bearer token. Set `API_TOKEN`; rotate it by restarting the worker with a new one |
| Transport | Plain HTTP. Put TLS in front of it on any shared network |
| Reach | A scenario runs from the worker's network position, so anyone with the token can aim traffic at anything the worker can reach |
| Scenario URLs | Off unless `SCENARIO_SOURCES` is set. Keep entries narrow (a host and path prefix, not a whole domain); every redirect hop is re-checked. The allowlist matches names, not addresses, so an allowed hostname that resolves to an internal address is still fetched |
| Fetch token | `SCENARIO_SOURCE_TOKEN` is sent to every allowed source. Don't allowlist a host you wouldn't give that token to |
| File access | Report endpoints serve fixed file names from a task's own directory; there is no caller-supplied path |
| Token comparison | Plain string equality, not constant-time. This guards a load-test trigger, not a high-value secret |

Scenarios can reference data files (feeders, body files). Upload them with the
scenario (the `files` field, or `--data` with `resonatectl`); each is saved in the
task's own directory and the scenario names it as a plain file name. Absolute paths
and `..` in `feeder.file`, `body_file` and `raw_body_file` are refused, so a
submitted scenario can't read other files on the worker. See
[Data files](usage.md#data-files-feeders).
