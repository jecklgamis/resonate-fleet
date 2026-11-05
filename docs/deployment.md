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

To build the image yourself:

```sh
docker build -t resonate-worker .
docker build --build-arg RESONATE_VERSION=v1.0.0-alpha.2 -t resonate-worker .
```

`RESONATE_VERSION` picks the bundled resonate release. `TARGETOS`/`TARGETARCH`
are set by `docker buildx build`, so a multi-arch build gets the matching binary.

## Kubernetes (Helm)

A chart lives in `deployment/k8s/helm/chart`:

```sh
cd deployment/k8s/helm
helm install resonate-worker ./chart --set apiToken=some-secret-token
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
| `replicaCount` / `autoscaling` | `1` / off | **Keep at 1.** See below |

If `persistence.enabled` and the volume isn't writable by the container's
non-root `app` user, set `podSecurityContext.fsGroup`.

The `/healthz` endpoint is used for the liveness and readiness probes, which is
why it is exempt from auth.

### Why one replica

Task state lives in each pod's own workspace and workspaces aren't shared. A
second replica behind the same Service can't answer a poll for a task that landed
on the first one. If you need more capacity, run **more workers** and address
them individually through a [controller](controller.md) rather than scaling one Service.

## Putting It on a Network

- **Set `API_TOKEN`.** With it unset, anyone who can reach the port can start
  load tests from your machine, against any target it can reach.
- **Terminate TLS in front of it** (an ingress, a load balancer, a reverse
  proxy). The worker speaks plain HTTP, and the bearer token travels in a header.
- **Mind what it can reach.** A scenario runs from the worker's network position,
  so the worker is a way to send traffic to anything it can see. Treat the token
  accordingly.
