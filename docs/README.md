# resonate-fleet

Run [`resonate`](https://jecklgamis.github.io/resonate/) load tests from a fleet of
workers. The pieces:

- **`resonate-worker`**: an HTTP wrapper around `resonate run`. Push a scenario
  file, the worker runs resonate against it, you poll for status and pull back the
  report.
- **`resonate-controller`**: workers register with it, and one API runs a scenario
  on all of them. See [Controller](controller.md).
- **`resonatectl`**: the command-line client for the controller. See
  [resonatectl](resonatectl.md).

> **Alpha**: APIs, flags and behavior may change without notice, like resonate itself.

This site covers **installing and using** resonate-fleet. For what goes
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
| Controller and CLI | Workers register with a controller; `resonatectl` runs one scenario on all of them (or a labelled subset) and collects each report without merging them. See [Controller](controller.md) and [resonatectl](resonatectl.md) |
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

### 4. Many workers: add a controller

With more than one worker, don't submit to each by hand. Run a controller, point
each worker at it, and use `resonatectl`:

```sh
docker run -d -p 58090:58090 -e CONTROLLER_TOKEN=ctl-secret jecklgamis/resonate-controller:main
docker run -d -p 58080:58080 -e API_TOKEN=some-secret-token \
  -e CONTROLLER_URL=http://<controller-host>:58090 -e CONTROLLER_TOKEN=ctl-secret \
  jecklgamis/resonate-worker:main        # repeat for each worker

export RESONATE_CONTROLLER=http://<controller-host>:58090 CONTROLLER_TOKEN=ctl-secret
resonatectl workers                       # who has registered
resonatectl run examples/scenario.yaml    # run on all of them, collect every report
```

See [Controller](controller.md) and [resonatectl](resonatectl.md).

## What Each Component Is, and Isn't

- **A worker** executes whatever is submitted to it, on its own. It has no queue and
  no concurrency cap, and it knows nothing about other workers unless you give it a
  controller to register with.
- **The controller** keeps a registry of live workers and fans a scenario out to
  them. It does not schedule, queue or retry, and it keeps its state in memory
  only. It never combines reports.
- **`resonatectl`** is a client for the controller, nothing more.

See [Architecture](architecture.md#what-this-isnt) for details.
