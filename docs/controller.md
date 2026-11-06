# Controller

`resonate-controller` is a small service that workers register with. You give it a
scenario and it runs it on every live worker (or a labelled subset), so a caller
needs one URL instead of a route to each worker. Workers still report
independently; nothing is combined.

It is plain HTTP and knows nothing about Kubernetes: it works the same for
containers on one host, VMs, or pods.

```
worker --POST /workers (heartbeat)--> controller <--POST /runs-- you
                                          |
                                          +--> each worker: POST /task, poll, proxy reports
```

## Run it

```sh
CONTROLLER_TOKEN=secret resonate-controller        # listens on :58090
```

```sh
docker run -p 58090:58090 -e CONTROLLER_TOKEN=secret jecklgamis/resonate-controller:main
```

On Kubernetes, use the chart in `deployment/k8s/helm/controller` (one replica: the
registry is in memory). Point workers at it with the worker chart's
`controller.url`, `controller.token` and `controller.labels` values.

| Variable | Default | Meaning |
| --- | --- | --- |
| `ADDR` | `:58090` | Listen address |
| `CONTROLLER_TOKEN` | *(unset)* | Bearer token required on everything but `/` (a JSON banner) and `/healthz`. Unset means open |
| `WORKER_TTL` | `30s` | A worker not seen for this long is dropped |
| `POLL_INTERVAL` | `2s` | How often the controller polls each worker's task |
| `RUN_TIMEOUT` | `1h` | Cap on one worker's submit and poll |

## Register workers

Set `CONTROLLER_URL` on each worker (see [Configuration](configuration.md)):

```sh
docker run -e API_TOKEN=wt -e CONTROLLER_URL=https://controller.example.com \
  -e CONTROLLER_TOKEN=secret -e WORKER_LABELS=region=sgp1 jecklgamis/resonate-worker:main
```

The worker sends its name, address, labels and its own `API_TOKEN` every
`HEARTBEAT_INTERVAL`. The controller uses that token when it calls the worker, and
never shows it in listings. Because the token travels to the controller, use
`https` (or a private network) for `CONTROLLER_URL`.

The address the worker registers must be reachable **from the controller**. The
worker detects its own address if `ADVERTISE_URL` is unset; set it when workers sit
behind NAT or a proxy.

A worker that stops is dropped after `WORKER_TTL`. The registry is in memory, so a
restarted controller refills within one heartbeat.

## API

All requests need `Authorization: Bearer $CONTROLLER_TOKEN` when it is set.

| Method | Path | Meaning |
| --- | --- | --- |
| `POST` | `/workers` | Register or refresh a worker: `{name, url, token, labels}`. `204` |
| `DELETE` | `/workers/{name}` | Remove a worker. `204` |
| `GET` | `/workers[?label=k=v]` | Live workers (repeat `label` to require several) |
| `POST` | `/runs[?label=k=v][&workers=N\|P%][&total_rate=R][&start_delay=D]` | Start a run on the matching live workers, or on a random `N` or `P%` of them (see below). Multipart (`file`, plus optional `files`) or JSON `{"scenarioUrl": "..."}`. `total_rate` splits one fleet-wide rate across the workers, and `start_delay` makes them all start together (see below). `202` with `{"runId", "workers", "available"}`, plus `totalRate` and `warnings` with `total_rate` and `startAt` with `start_delay`; `409` if none match or fewer are live than asked for; `400` for a bad `workers`, `total_rate` or `start_delay` |
| `GET` | `/runs/{id}` | Run status with one row per worker |
| `GET` | `/runs/{id}/live` | What the run is doing right now, across all its workers (see [Watching a run](#watching-a-run)) |
| `DELETE` | `/runs/{id}` | Abort a run: each worker still running is told to cancel its task. `202` with `{"runId", "aborted": [workers]}`; `200` with none if the run had already finished. The run ends as `canceled` |
| `GET` | `/runs/{id}/{worker}/{file}` | A worker's `report.html`, `report.json`, `results.jsonl` or `log`, proxied |

```sh
id=$(curl -fsS -H "Authorization: Bearer $TOKEN" -F file=@scenario.yaml \
  "$CTRL/runs?label=region=sgp1" | jq -r .runId)
curl -fsS -H "Authorization: Bearer $TOKEN" $CTRL/runs/$id
curl -fsS -H "Authorization: Bearer $TOKEN" $CTRL/runs/$id/10.0.1.5/report.html -o report.html
```

### Running on some of the workers

`workers` narrows a run to a random subset of the live workers that match the
labels, so the labels are applied first:

| `workers=` | Runs on |
| --- | --- |
| *(absent)* | Every matching live worker |
| `4` | Exactly 4 of them, chosen at random |
| `25%` | A quarter of them, rounded **up**, and never fewer than 1 (`25%` of 10 is 3; `1%` is 1) |

- A count larger than the number of matching live workers is a `409` ("requested 8
  workers but only 4 are live"), not a smaller run. Silently running on fewer would
  change the total load without telling you.
- `0`, a negative number, anything above `100%`, or a non-number is a `400`.
- The choice is purely random. The controller doesn't look at how busy a worker is, and
  the same workers can be picked twice in a row. The response lists the chosen
  workers in `workers` and how many were eligible in `available`.
- **Load is per worker.** A scenario at `rate: 100` generates 400 requests per second
  on 4 workers, and the same 400 on 25% of 16. Choosing a count or percentage changes
  the aggregate load; the scenario's rate is not divided among the chosen workers
  (unless you also pass `total_rate`, below).

### One rate for the whole fleet

By default every worker runs the scenario as written, so `rate: 100` on 4 workers is
400 req/s. `?total_rate=R` makes **R the fleet's peak rate** instead: the controller
rewrites the scenario's `load:` block, one copy per worker, and the copies add up to R.

- Every rate and user count in the load is scaled by `R / peak`, where the peak is
  `load.rate` or the highest stage `rate`, so a staged ramp keeps its shape and only
  changes height. Then each is divided across the workers.
- Rates divide exactly. User counts (`workers`, `max_workers`) are whole numbers: the
  remainder goes to the first workers, and a count smaller than the number of workers
  gives each worker 1 (the fleet then runs a little more than asked, and the response
  `warnings` say so).
- `requests` is a fleet-wide cap, so it is divided, not scaled. `iterations` is per
  virtual user and is left alone. Durations never change.
- A rate of `0` stays `0` (in a stage, that is a ramp to zero).
- Only the `load:` block is edited. Comments elsewhere survive, but the rewritten file
  is re-serialized, so blank lines go and flow-style braces are re-spaced.
- It needs an uploaded scenario (`400` with `scenarioUrl`: the workers fetch that file
  themselves) and a load with a rate (`400` otherwise).
- Each worker's recorded `sha256` is of its own slice, not of the file you sent.

`resonate` itself runs a few percent over the nominal rate, mostly in its first
second, so expect the measured fleet rate to land slightly above R.

### Watching a run

`GET /runs/{id}/live` polls every worker's [`/task/{id}/live`](api.md#get-taskidlive)
at once and combines them:

```json
{
  "id": "dcc10978ac76766a", "status": "running", "elapsed_ms": 11000,
  "reporting": 3, "requests": 642, "errors": 0, "error_rate": 0, "rps": 55.2,
  "latency_ms": {"p50": 2.36, "p95": 3.16, "p99": 3.49, "max": 3.96},
  "workers": [{"name": "w0", "status": "running", "requests": 214, "errors": 0, "rps": 18.4, "p95_ms": 3.16}]
}
```

`requests` and `errors` are the whole run so far. While the run is going, `rps` and
the latency percentiles cover **the last 10 seconds**, so a problem shows as it starts
instead of being averaged away by the run so far; once it is over they cover the whole
run. The percentiles are the fleet's own: each worker sends a small latency histogram
and the controller adds them, so they are not an average of the workers' percentiles.
They are read from buckets 5% wide, so they are within about 5% of the exact value
(the [fleet total](resonatectl.md#fleet-total) after the run is exact).

A worker that is unreachable, has no task yet, or predates live stats gets a `note`
instead of numbers and is left out of the totals (`reporting` counts the rest). Each
call polls the workers, so a few watchers at a second apiece is fine; it isn't meant
for a crowd.

### Starting together

`?start_delay=D` (like `5s`; at most `10m`) has the controller pick one start time,
now plus `D`, and pass it to every worker (`POST /task?startAt=...`). Each worker
holds the task `queued` until then and starts `resonate` at that moment, so staged
ramps line up. With a single worker it is ignored. Without it, each worker starts as
soon as it has the scenario.

It uses the workers' own clocks, so they need to agree (NTP). Each worker echoes its
clock when it accepts the task, and a worker's `warning` in the run status is set
when it:

- **predates `startAt`** (an older worker) and started at once;
- **received the scenario after the start time** (a large upload, or a delay shorter
  than the delivery), so it started late: raise `D`;
- **has a clock more than 500ms** from the controller's, so its start is off by about
  that much.

Warnings don't fail the run. A queued task can be canceled while it waits.

A run's status is `running` until every worker finishes, then `done` if all did
and `failed` if any worker failed or could not be reached (its row shows
`error`). Runs are kept in memory only.

## From the command line

[`resonatectl`](resonatectl.md) is the CLI for the controller:

```sh
export RESONATE_CONTROLLER=https://controller.example.com CONTROLLER_TOKEN=secret
resonatectl workers
resonatectl run --label region=sgp1 --data users.csv scenario.yaml
```

## Limits of this version

- No pre-flight check that the scenario is valid; each worker rejects it itself.
- Runs and the registry are lost when the controller restarts.
