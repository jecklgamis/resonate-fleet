# resonate-worker

An HTTP wrapper around [`resonate run`](https://jecklgamis.github.io/resonate/): push a
scenario file, the worker runs resonate against it, you poll for status and
pull back the report.

> **Alpha**: APIs, flags and behavior may change without notice, like resonate itself.

This site covers **installing and using** resonate-worker. For what goes
*inside* a scenario file (targets, flows, load profiles, assertions), see the
[resonate docs](https://jecklgamis.github.io/resonate/) — the worker hands your
file to `resonate run` unchanged.

## Features

| Feature | Details |
| --- | --- |
| Upload, poll, fetch | `POST /task` a scenario YAML, get a `taskId`, poll `GET /task/{id}`, fetch the report. See [Usage](usage.md) |
| Scenario by upload or URL | Upload the YAML, or give the worker a URL on an allowlisted config server (or an S3 pre-signed URL) to fetch. The fetched bytes are snapshotted with their `sha256`. See [Scenarios by URL](usage.md#scenarios-by-url) |
| Every resonate report | `report.html`, `report.json`, raw `results.jsonl` and the run's console `log`, all served per task. See [API Reference](api.md) |
| CI-friendly status | A run whose assertions fail ends as `failed` (resonate's non-zero exit), and still serves its report |
| Bearer-token auth | One `API_TOKEN` gates every endpoint except `/healthz`. See [Configuration](configuration.md) |
| Survives restarts | Each task's state is saved to its workspace, so the task list and reports are still there after a restart. See [Operations](operations.md) |
| Request and task logging | One log line per request plus task lifecycle events; bearer tokens are never logged |
| Fleet dispatcher | `resonate-fleet` fans one scenario out to several workers (one per region or AZ) and collects each report without merging them. See [Fleet](fleet.md) |
| Docker and Helm | A public image that bundles a pinned `resonate`, and a Helm chart. See [Deployment](deployment.md) |

## Quick Start

### 1. Run a worker

```sh
docker run -it --name resonate-worker -p 58080:58080 \
  -e API_TOKEN=some-secret-token jecklgamis/resonate-worker:main
```

Or from source — see [Install](install.md).

### 2. Submit a scenario

```sh
curl -H "Authorization: Bearer some-secret-token" \
     -F "file=@examples/scenario.yaml" \
     http://localhost:58080/task
# {"taskId":"c55286ad94a65b72"}
```

### 3. Poll, then fetch the report

```sh
curl -H "Authorization: Bearer some-secret-token" \
     http://localhost:58080/task/c55286ad94a65b72
# {"id":"c55286ad94a65b72","scenario":"scenario.yaml","status":"done",...}

curl -H "Authorization: Bearer some-secret-token" \
     http://localhost:58080/task/c55286ad94a65b72/report.html -o report.html
```

See [Usage](usage.md) for the full walkthrough and [API Reference](api.md) for
every endpoint.

## One Worker, Not a Platform

A worker executes whatever is submitted to it and has no relationship with any
other worker. There is no queue, no concurrency cap and no coordinator. See
[Architecture](architecture.md#what-this-isnt) for what that means in practice.
