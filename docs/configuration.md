# Configuration

The worker is configured with environment variables only. All are optional.

| Variable | Default | Meaning |
| --- | --- | --- |
| `ADDR` | `:58080` | Listen address |
| `API_TOKEN` | *(unset)* | Bearer token required on every endpoint except `/healthz`. **Unset means no auth at all** — fine on localhost, not for anything reachable over a network |
| `WORKSPACE_DIR` | `workspace` | Where each task's files are written, one subdirectory per task |
| `RESONATE_BIN` | `resonate` next to the worker, else on `$PATH` | Path to the resonate binary |
| `SCENARIO_SOURCES` | *(unset)* | Comma-separated `http(s)://host[:port][/path-prefix]` entries the worker may fetch scenarios from. **Unset means submitting by URL is refused.** An invalid entry stops the worker at startup. See [Scenarios by URL](usage.md#scenarios-by-url) |
| `CONTROLLER_URL` | *(unset)* | When set, the worker registers with this [controller](controller.md) and re-registers as a heartbeat |
| `CONTROLLER_TOKEN` | *(unset)* | Bearer token for the controller |
| `WORKER_NAME` | hostname | Name the worker registers under |
| `ADVERTISE_URL` | detected | Address the controller reaches this worker at. If unset, the worker uses its own address on the interface that routes to the controller, plus the `ADDR` port |
| `WORKER_LABELS` | *(unset)* | `key=value,key=value`, e.g. `region=sgp1,az=a`, for selecting workers in a run |
| `HEARTBEAT_INTERVAL` | `10s` | How often to re-register |
| `S3_BUCKET` | *(unset)* | When set, each finished task's files (scenario, uploaded data files, `report.*`, `results.jsonl`, `log.txt`) are uploaded to `s3://<bucket>/<S3_PREFIX>/<taskId>/`. Credentials come from the standard AWS chain (env vars, shared config, IRSA, instance role) |
| `S3_PREFIX` | *(unset)* | Key prefix inside the bucket |
| `S3_ENDPOINT` | *(unset)* | Endpoint URL for an S3-compatible service such as MinIO (uses path-style addressing) |
| `SCENARIO_SOURCE_TOKEN` | *(unset)* | Bearer token sent with every scenario fetch, for sources that need auth. It goes to **every** allowed source, so allow only hosts you trust with it |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *(unset)* | When set, the worker pushes live request metrics over OTLP/HTTP. Also `OTEL_METRIC_EXPORT_INTERVAL` (ms, default 10000 here). See [Metrics](metrics.md) |

The Docker image sets `WORKSPACE_DIR=/app/workspace` and
`RESONATE_BIN=/usr/local/bin/resonate`.

```sh
ADDR=:9000 API_TOKEN=s3cret WORKSPACE_DIR=/var/lib/resonate-worker \
  RESONATE_BIN=/opt/resonate/bin/resonate ./resonate-worker
```

## Startup Output

```
2026/11/05 11:46:42 scenario URLs enabled for: https://config.example.com/scenarios
2026/11/05 11:46:42 loaded 3 task(s) from /var/lib/resonate-worker
2026/11/05 11:46:42 resonate-worker listening on :9000 (workspace=/var/lib/resonate-worker, resonate=/opt/resonate/bin/resonate)
```

With no `API_TOKEN` you also get:

```
warning: API_TOKEN is not set — every endpoint is open to anyone who can reach this address
```

## The Workspace

Each task gets `WORKSPACE_DIR/<taskId>/`:

| File | What it is |
| --- | --- |
| `scenario.yaml` | The uploaded scenario |
| `task.json` | The task's saved state — see [Operations](operations.md#persistence) |
| `log.txt` | resonate's stdout and stderr |
| `report.html`, `report.json`, `results.jsonl` | resonate's reports |

The worker never deletes workspaces. Clear old ones out yourself — see
[Operations](operations.md#disk-usage).

## How `resonate` Is Invoked

For every task the worker runs, with the task directory as the working directory:

```sh
resonate run scenario.yaml --quiet \
  --html-report report.html --json-report report.json --results-file results.jsonl
```

There is no way to pass other flags through. Everything about the load (duration,
rate, assertions, targets) belongs in the scenario file — see resonate's
[Scenarios](https://jecklgamis.github.io/resonate/#/scenarios) page.

## Artifact upload

With `S3_BUCKET` set, the worker uploads a task's files after `resonate` exits and
before the task is marked `done` or `failed`. The task status then carries
`artifacts_url` (the `s3://` prefix). If the upload fails, the task keeps its run
status and gets `upload_error` instead. The local copy stays on the worker, so the
report endpoints keep working.
