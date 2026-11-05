# Deployment

## Docker

```sh
docker run -d --name resonate-worker -p 58080:58080 \
  -e API_TOKEN=some-secret-token \
  -v resonate-workspace:/app/workspace \
  jecklgamis/resonate-worker:main
```

The mounted volume keeps task state and reports across container restarts; leave
it off for disposable or CI use.

The controller has its own image, `jecklgamis/resonate-controller`:

```sh
docker run -d --name resonate-controller -p 58090:58090 \
  -e CONTROLLER_TOKEN=some-secret jecklgamis/resonate-controller:main
```

To build the images yourself (`make image`, `make image-controller`):

```sh
docker build -t resonate-worker .
docker build --build-arg RESONATE_VERSION=v1.0.0-alpha.2 -t resonate-worker .
```

`RESONATE_VERSION` picks the bundled resonate release. `TARGETOS`/`TARGETARCH`
are set by `docker buildx build`, so a multi-arch build gets the matching binary.

## Kubernetes (Helm)

A chart lives in `deployment/k8s/helm/worker`:

```sh
cd deployment/k8s/helm
helm install resonate-worker ./worker --set apiToken=some-secret-token
```

Key values (see `values.yaml` for the full set, each with its own comment):

| Value | Default | Meaning |
| --- | --- | --- |
| `apiToken` | `"default"` | The bearer token — override it, or set `existingSecretName` to source `API_TOKEN` from a Secret you manage (key `API_TOKEN`) |
| `persistence.enabled` | `false` | Keep task workspaces (scenario, reports and task state) on a PVC across pod restarts |
| `persistence.size` | `10Gi` | PVC size |
| `ingress.enabled` | `false` | Expose it outside the cluster |
| `image.tag` | `main` | A mutable tag, so `pullPolicy` defaults to `Always`; pin a commit SHA for reproducibility |
| `scenarioSources` | `[]` | URL prefixes the worker may fetch scenarios from (`SCENARIO_SOURCES`). Empty refuses URL submissions |
| `scenarioSourceToken` | `""` | Bearer token sent on every scenario fetch (`SCENARIO_SOURCE_TOKEN`), stored in the same Secret as `apiToken` |
| `replicaCount` / `autoscaling` | `1` / off | Without a controller, keep at 1. With one, every pod registers on its own and any number works. See below |
| `controller.url` | `""` | Register with a [controller](controller.md) (`CONTROLLER_URL`). Empty disables it |
| `controller.token` | `""` | The controller's token (`CONTROLLER_TOKEN`), stored in the same Secret as `apiToken` |
| `controller.labels` | `{}` | Labels the worker registers with, e.g. `{region: sgp1}`, for `resonatectl run --label` |
| `controller.advertiseUrl` | `""` | Address the controller reaches the worker at. Unset means the pod IP is detected |
| `s3.bucket` / `s3.prefix` / `s3.endpoint` | `""` | Upload each finished task's files to `s3://<bucket>/<prefix>/<taskId>/`. See [Configuration](configuration.md#artifact-upload) |

If `persistence.enabled` and the volume isn't writable by the container's
non-root `app` user, set `podSecurityContext.fsGroup`.

The `/healthz` endpoint is used for the liveness and readiness probes, which is
why it is exempt from auth.

### Replicas, and why a controller

Task state lives in each pod's own workspace and workspaces aren't shared. A
second replica behind the same Service can't answer a poll for a task that landed
on the first one, so **without a controller, keep one replica**.

With a controller, that stops mattering: each pod registers under its own name
(its pod name) and address, and the controller talks to each pod directly, never
through the Service. Set `controller.url` and `controller.token` and scale
`replicaCount` freely. Pods leave the controller as soon as they receive SIGTERM,
so a rolling restart doesn't leave dead workers listed.

```sh
helm install resonate-worker ./worker --set apiToken=... --set replicaCount=4 \
  --set controller.url=http://resonate-controller --set controller.token=... \
  --set controller.labels.region=sgp1
```

## The Controller

A separate chart lives in `deployment/k8s/helm/controller`:

```sh
helm install resonate-controller ./controller --set controllerToken=some-secret
```

| Value | Default | Meaning |
| --- | --- | --- |
| `controllerToken` | `"default"` | The bearer token for the controller's API, which workers also present when they register. Override it, or set `existingSecretName` (key `CONTROLLER_TOKEN`) |
| `ingress.enabled` | `false` | Expose it so `resonatectl` can reach it from outside the cluster; terminate TLS there, because workers send their API token to it |
| `workerTTL` / `pollInterval` / `runTimeout` | `30s` / `2s` / `1h` | See [Controller](controller.md) |
| `replicaCount` | `1` | **Keep at 1.** The registry and runs are in memory; a second replica would see only some of the workers |

The image is `jecklgamis/resonate-controller`. It holds no persistent state, so a
restart loses past runs, though workers re-register within 10 seconds.

The `Makefile` in `deployment/k8s/helm` wraps both charts, with the component as a
suffix: `make lint`, `make package`, `make install-worker`, `make upgrade-controller`,
`make dry-run-worker`, and so on.

## Putting It on a Network

- **Set `API_TOKEN`.** With it unset, anyone who can reach the port can start
  load tests from your machine, against any target it can reach.
- **Terminate TLS in front of it** (an ingress, a load balancer, a reverse
  proxy). The worker speaks plain HTTP, and the bearer token travels in a header.
- **Mind what it can reach.** A scenario runs from the worker's network position,
  so the worker is a way to send traffic to anything it can see. Treat the token
  accordingly.
