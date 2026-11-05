# resonate-fleet

Run [`resonate`](https://github.com/jecklgamis/resonate) load tests from a fleet
of workers. Three components:

| Component | What it is |
| --- | --- |
| `resonate-worker` | An HTTP wrapper around `resonate run`: push a scenario file, the worker runs resonate against it, you poll for status and pull back the report |
| `resonate-controller` | Workers register with it; one API runs a scenario on all of them (or a labelled subset) |
| `resonatectl` | The command-line client for the controller: `run` a scenario, list `workers` |

You can use a worker on its own, which is the quick start below, and add the
controller when you have more than one.

> **Alpha**: APIs and behavior may change without notice.

**📖 Full user guide: https://jecklgamis.github.io/resonate-fleet/**

## Features

**resonate-worker**
- Submit a scenario by multipart upload or by `scenarioUrl` (allowlisted), with feeder and other data files alongside it
- Poll task status; fetch the report and logs
- Refuses absolute paths and `..` in scenario file references
- Optional upload of task files and results to S3
- Registers with a controller by heartbeat (with labels) and deregisters on shutdown
- Live metrics pushed over OTLP while a run is going ([Metrics](docs/metrics.md))
- Bearer-token auth; the Docker image bundles `resonate`

**resonate-controller**
- Workers register and heartbeat; entries expire after a TTL (in-memory registry)
- `GET /workers`, filterable by label (`?label=k=v`)
- `POST /runs` fans a scenario out to the matching workers, polls them, and proxies each report and log
- Targeting: label filter first, then a random subset with `workers=N` or `workers=P%`
- Bearer-token auth

**resonatectl**
- `run` a scenario with data files and see a per-worker results table; `workers` lists the fleet
- `--label`, `--workers N|P%`, `--out`, `--poll`
- kubectl-style config file with contexts (`config view|use|set-context|delete-context`)
- Settings resolve flags, then environment, then the current context; warns on a world-readable config holding a token

**Packaging**
- Docker Hub images (Ubuntu 24.04, amd64), Helm charts for the worker and controller
- Tagged releases with binaries for five platforms and checksums
- Scripts to run a controller, a worker and a scenario locally

## Quick start

Download the archive for your platform from the
[releases page](https://github.com/jecklgamis/resonate-fleet/releases) — it holds
`resonate-worker`, `resonate-controller`, `resonatectl` and the `resonate` binary
they run, so there is nothing else to install:

```sh
VERSION=v1.0.0-alpha.4   # a release tag
curl -fsSLO https://github.com/jecklgamis/resonate-fleet/releases/download/$VERSION/resonate-fleet_${VERSION}_darwin_arm64.tar.gz
tar -xzf resonate-fleet_${VERSION}_darwin_arm64.tar.gz && cd resonate-fleet_${VERSION}_darwin_arm64
API_TOKEN=some-secret-token ./resonate-worker   # listens on :58080
```

Platforms: `linux_amd64`, `linux_arm64`, `darwin_amd64`, `darwin_arm64`, `windows_amd64`
(a `.zip`). The worker finds the `resonate` next to it; set `RESONATE_BIN` to use another.
On macOS, a browser-downloaded archive is quarantined as unsigned; clear it with
`xattr -dr com.apple.quarantine .` (not needed when downloaded with `curl`).

Or with Docker — better for servers, CI and Kubernetes (the image bundles `resonate`;
it is `linux/amd64` only, so it runs under emulation on Apple Silicon):

```sh
docker run -it --rm -p 58080:58080 \
  -e API_TOKEN=some-secret-token jecklgamis/resonate-worker:main
```

Or from source: `make build`, with [`resonate`](https://github.com/jecklgamis/resonate)
on `$PATH`.

To try a controller and a worker together on your machine, run
`scripts/run-local-controller.sh`, `scripts/run-local-worker.sh` and then
`scripts/run-local-scenario.sh` (see [Running Locally](docs/install.md#running-locally)).

Submit a scenario, poll it, fetch the report:

```sh
curl -H "Authorization: Bearer some-secret-token" \
     -F "file=@examples/scenario.yaml" http://localhost:58080/task
# {"taskId":"c55286ad94a65b72"}

curl -H "Authorization: Bearer some-secret-token" \
     http://localhost:58080/task/c55286ad94a65b72
# {"id":"c55286ad94a65b72","status":"done",...}

curl -H "Authorization: Bearer some-secret-token" \
     http://localhost:58080/task/c55286ad94a65b72/report.html -o report.html
```

Rather than uploading, you can have the worker fetch the scenario from an
allowlisted URL (`SCENARIO_SOURCES`) — see the
[Usage guide](docs/usage.md#scenarios-by-url).

Running the same scenario against several workers (one per region or AZ)?
[`resonatectl`](docs/resonatectl.md) runs it on all of them through the
[controller](docs/controller.md) and collects each report, without merging them.

`resonatectl` talks to a controller, not to a worker directly. Even with a single
worker, the easiest way to use it is to run a controller beside it: it is one small
binary, and `scripts/run-local-controller.sh` starts one in a single command. With no
controller, use the worker's HTTP API with `curl`, as above.

## Documentation

The full user guide is published at
**[jecklgamis.github.io/resonate-fleet](https://jecklgamis.github.io/resonate-fleet/)**.
Its source is [`docs/`](docs), a docsify site you can also browse on GitHub or serve
locally with `docsify serve docs`:

| Page | Covers |
| --- | --- |
| [Overview](docs/README.md) | Features and quick start |
| [Install](docs/install.md) | Docker, building from source, running the tests |
| [Usage](docs/usage.md) | Submit, poll, fetch; gating a CI pipeline |
| [API Reference](docs/api.md) | Every endpoint, status codes, auth |
| [Configuration](docs/configuration.md) | Environment variables, the workspace |
| [Metrics](docs/metrics.md) | Push live request metrics over OpenTelemetry (OTLP) |
| [Controller](docs/controller.md) | Workers register; one API runs a scenario on all of them |
| [resonatectl](docs/resonatectl.md) | The CLI for the controller |
| [Deployment](docs/deployment.md) | Docker, Helm, running on a network |
| [Operations](docs/operations.md) | Logging, persistence, disk, concurrency, security |
| [Architecture](docs/architecture.md) | How it works, and what it isn't |
| [Troubleshooting](docs/troubleshooting.md) | Common failures |

## Development

```sh
make check   # fmt-check + vet + test
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).
