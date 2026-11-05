# resonate-worker

An HTTP wrapper around [`resonate run`](https://github.com/jecklgamis/resonate):
push a scenario file, it runs resonate against it, you poll for status and pull
back the report. Modeled on
[jecklgamis/gatling-server](https://github.com/jecklgamis/gatling-server)'s
upload/taskId/report shape, for resonate instead of Gatling.

It is one worker. Each instance executes whatever is submitted to it and keeps
no relationship with any other worker — see [What this isn't](#what-this-isnt).

Running the same scenario against several of these (e.g. one per region/AZ)?
See [`cmd/resonate-fleet`](cmd/resonate-fleet), a small dispatcher that
submits to a fixed list of workers in parallel, polls each independently, and
pulls back every report — without merging them.

## Quick start

### Docker

```sh
docker run -it --name resonate-worker -p 58080:58080 \
  -e API_TOKEN=some-secret-token jecklgamis/resonate-worker:main
```

The image bundles a pinned `resonate` release (see `Dockerfile`'s `RESONATE_VERSION`
build arg) — nothing else to install.

### From source

Needs the `resonate` binary on `$PATH` (or point `RESONATE_BIN` at it) — see
[resonate's install docs](https://github.com/jecklgamis/resonate/blob/main/docs/install.md).

```sh
make build
API_TOKEN=some-secret-token ./resonate-worker   # listens on :58080 by default
```

(`make run` builds and starts the worker in one step.) The worker doesn't check
for `resonate` at startup, so a missing binary only shows up as a failed task
on the first upload.

Submit a scenario (a sample is in [`examples/scenario.yaml`](examples/scenario.yaml)):

```sh
curl -H "Authorization: Bearer some-secret-token" \
     -F "file=@scenario.yaml" \
     http://localhost:58080/task
# {"taskId":"c55286ad94a65b72"}
```

Poll it:

```sh
curl -H "Authorization: Bearer some-secret-token" \
     http://localhost:58080/task/c55286ad94a65b72
# {"id":"...","scenario":"scenario.yaml","status":"done","exit_code":0,...}
```

`status` is `queued`, `running`, `done` or `failed` — `failed` covers both a
scenario resonate couldn't even start (bad YAML) and one that ran but whose
assertions didn't pass (resonate's own non-zero exit code either way).

Fetch the report once `status` is `done` or `failed` (a failed run still
writes its report):

```sh
curl -H "Authorization: Bearer some-secret-token" \
     http://localhost:58080/task/c55286ad94a65b72/report.html -o report.html
```

## Endpoints

| Method | Path | Returns |
| --- | --- | --- |
| `POST` | `/task` | Multipart upload, field `file` = the scenario YAML. `{"taskId": "..."}`, `202 Accepted` |
| `GET` | `/task` | Every task this worker has run (in-memory; lost on restart) |
| `GET` | `/task/{id}` | That task's status |
| `GET` | `/task/{id}/report.html` | resonate's HTML report |
| `GET` | `/task/{id}/report.json` | resonate's JSON report |
| `GET` | `/task/{id}/results.jsonl` | resonate's raw per-request results |
| `GET` | `/task/{id}/log` | resonate's stdout+stderr for that run |
| `GET` | `/healthz` | `200 ok` — exempt from auth, for a load balancer health check |

A report/results/log path 404s until resonate has actually written that file
— check `status` first, or just retry.

## Configuration

Environment variables, all optional:

| Variable | Default | Meaning |
| --- | --- | --- |
| `ADDR` | `:58080` | Listen address |
| `API_TOKEN` | *(unset)* | Bearer token required on every endpoint except `/healthz`. **Unset means no auth at all** — fine on localhost, not for anything reachable over a network |
| `WORKSPACE_DIR` | `workspace` | Where each task's scenario file and reports are written, one subdirectory per task |
| `RESONATE_BIN` | `resonate` | Path to the resonate binary, if it isn't on `$PATH` |

## Deploying to Kubernetes

A Helm chart lives in `deployment/k8s/helm/chart`, mirroring gatling-server's own:

```sh
cd deployment/k8s/helm
helm install resonate-worker ./chart --set apiToken=some-secret-token
```

Key values (see `values.yaml` for the full set, each with its own comment):

| Value | Default | Meaning |
| --- | --- | --- |
| `apiToken` | `"default"` | The bearer token — override it, or set `existingSecretName` to source `API_TOKEN` from a Secret you manage |
| `persistence.enabled` | `false` | Keep task workspaces (scenario + reports) on a PVC across pod restarts |
| `ingress.enabled` | `false` | Expose it outside the cluster |
| `replicaCount` / `autoscaling` | `1` / off | **Keep at 1.** Task status is in-memory per pod and workspaces aren't shared, so a second replica behind the same Service can't answer a poll for a task that landed on the first one |

## What this isn't

- **No fleet coordination.** Submitting the same scenario to five workers in
  five regions and combining their reports into one number is on you — this
  is deliberately just the one-node piece. See resonate's own
  `docs/` for the "independent instances, independent reports" pattern this
  is meant to slot into.
- **No persistence.** Task status lives in memory; a worker restart loses the
  task list (the workspace directories on disk, with their reports, survive).
- **No queue or concurrency cap.** Every submission starts a subprocess
  immediately; there's no limit on how many run at once. Fine for occasional
  manual submissions, not yet for something you'd point a scheduler at.
- **No S3/SNS artifact upload or notifier events (yet).** gatling-server has
  both; this doesn't, on purpose, until the core shape has proven itself.
  Docker image and Helm chart are here, though — see above.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
