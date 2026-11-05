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

## Quick start

```sh
docker run -it --rm -p 58080:58080 \
  -e API_TOKEN=some-secret-token jecklgamis/resonate-worker:main
```

Or from source (needs [`resonate`](https://github.com/jecklgamis/resonate) on `$PATH`):

```sh
make build
API_TOKEN=some-secret-token ./resonate-worker   # listens on :58080
```

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

## Documentation

The full user guide lives in [`docs/`](docs), a docsify site you can browse on
GitHub or serve locally with `docsify serve docs`:

| Page | Covers |
| --- | --- |
| [Overview](docs/README.md) | Features and quick start |
| [Install](docs/install.md) | Docker, building from source, running the tests |
| [Usage](docs/usage.md) | Submit, poll, fetch; gating a CI pipeline |
| [API Reference](docs/api.md) | Every endpoint, status codes, auth |
| [Configuration](docs/configuration.md) | Environment variables, the workspace |
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
