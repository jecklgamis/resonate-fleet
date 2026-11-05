# Configuration

The worker is configured with environment variables only. All are optional.

| Variable | Default | Meaning |
| --- | --- | --- |
| `ADDR` | `:58080` | Listen address |
| `API_TOKEN` | *(unset)* | Bearer token required on every endpoint except `/healthz`. **Unset means no auth at all** — fine on localhost, not for anything reachable over a network |
| `WORKSPACE_DIR` | `workspace` | Where each task's files are written, one subdirectory per task |
| `RESONATE_BIN` | `resonate` | Path to the resonate binary, if it isn't on `$PATH` |
| `SCENARIO_SOURCES` | *(unset)* | Comma-separated `http(s)://host[:port][/path-prefix]` entries the worker may fetch scenarios from. **Unset means submitting by URL is refused.** An invalid entry stops the worker at startup. See [Scenarios by URL](usage.md#scenarios-by-url) |
| `SCENARIO_SOURCE_TOKEN` | *(unset)* | Bearer token sent with every scenario fetch, for sources that need auth. It goes to **every** allowed source, so allow only hosts you trust with it |

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
