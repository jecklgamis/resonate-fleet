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
| `CONTROLLER_TOKEN` | *(unset)* | Bearer token required on everything but `/healthz`. Unset means open |
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
| `POST` | `/runs[?label=k=v][&workers=N\|P%]` | Start a run on the matching live workers, or on a random `N` or `P%` of them (see below). Multipart (`file`, plus optional `files`) or JSON `{"scenarioUrl": "..."}`. `202` with `{"runId", "workers", "available"}`; `409` if none match or fewer are live than asked for; `400` for a bad `workers` value |
| `GET` | `/runs/{id}` | Run status with one row per worker |
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
  the aggregate load; the scenario's rate is not divided among the chosen workers.

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
