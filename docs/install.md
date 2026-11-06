# Install

## Using a Release Binary

Download the archive for your platform from the
[releases page](https://github.com/jecklgamis/resonate-fleet/releases) — it holds
`resonate-worker`, `resonate-controller`, `resonatectl` and the `resonate` binary
they run, so there is nothing else to install:

```sh
VERSION=v1.0.0-alpha.6   # a release tag
curl -fsSLO https://github.com/jecklgamis/resonate-fleet/releases/download/$VERSION/resonate-fleet_${VERSION}_darwin_arm64.tar.gz
tar -xzf resonate-fleet_${VERSION}_darwin_arm64.tar.gz && cd resonate-fleet_${VERSION}_darwin_arm64
API_TOKEN=some-secret-token ./resonate-worker   # listens on :58080
```

Platforms: `linux_amd64`, `linux_arm64`, `darwin_amd64`, `darwin_arm64`, `windows_amd64`
(a `.zip`). The worker finds the `resonate` next to it; set `RESONATE_BIN` to use another.
On macOS, a browser-downloaded archive is quarantined as unsigned; clear it with
`xattr -dr com.apple.quarantine .` (not needed when downloaded with `curl`).

## Using Docker

```sh
docker run -it --rm -p 58080:58080 \
  -e API_TOKEN=some-secret-token jecklgamis/resonate-worker:main
```

The image bundles a pinned `resonate` release (the `RESONATE_VERSION` build arg
in `deployment/docker/worker/Dockerfile`) — there is nothing else to install. It runs as a non-root
user and keeps task workspaces under `/app/workspace`.

The controller is a separate image, `jecklgamis/resonate-controller`; see
[Controller](controller.md).

## Building from Source

Requires Go (see [`go.mod`](https://github.com/jecklgamis/resonate-fleet/blob/main/go.mod)
for the pinned version) and the `resonate` binary on `$PATH`, or pointed to by
`RESONATE_BIN`. To get `resonate`, see
[its install page](https://jecklgamis.github.io/resonate/#/install).

```sh
git clone https://github.com/jecklgamis/resonate-fleet.git
cd resonate-fleet
make build            # builds ./resonate-worker, ./resonate-controller and ./resonatectl
API_TOKEN=some-secret-token ./resonate-worker
```

`make run` builds the worker and starts it in one step. It listens on `:58080`
by default. To run a controller and a worker together, see
[Running Locally](#running-locally) below.

> The worker does not check for `resonate` at startup. A missing binary only
> shows up as a `failed` task on the first upload — see
> [Troubleshooting](troubleshooting.md#a-task-fails-immediately).

## Checking It Is Up

```sh
curl http://localhost:58080/healthz
# ok
```

`/healthz` never needs the token.

## Development

```sh
make build     # the three binaries: resonate-worker, resonate-controller, resonatectl
make test      # go test ./...
make check     # fmt-check + vet + test
make images    # both Docker images (image-worker, image-controller for one)
```

The tests include integration tests that run the real `resonate` against a
throwaway local HTTP target. They skip themselves when `resonate` isn't found,
so `make test` passes on a bare machine.

## Running Locally

Two scripts build and run a controller and a worker on your machine, with sensible
defaults, so you can try the whole flow without Docker or Kubernetes:

```sh
scripts/run-local-controller.sh        # terminal 1: controller on :58090
scripts/run-local-worker.sh            # terminal 2: worker on :58080, registered with it
scripts/run-local-scenario.sh          # terminal 3: run a scenario on it via resonatectl
```

`run-local-scenario.sh` runs `examples/local-scenario.yaml`, which loads the local
controller's own `/healthz`, so nothing else needs to be running. Pass your own
scenario and any `resonatectl run` flag straight through:

```sh
scripts/run-local-scenario.sh my-scenario.yaml
scripts/run-local-scenario.sh --label region=sgp1 --data users.csv --out reports my-scenario.yaml
```

Or use `resonatectl` directly, with environment variables or a
[config file](resonatectl.md#config-file) so you don't repeat them:

```sh
export RESONATE_CONTROLLER=http://localhost:58090 CONTROLLER_TOKEN=ctl-secret
./resonatectl workers
./resonatectl run examples/scenario.yaml
```

The worker script needs the `resonate` binary on `PATH` (or `RESONATE_BIN`). Both
scripts take their settings from the environment; the variables and defaults are
listed at the top of each script. Run the worker alone, without a controller, with
`CONTROLLER_URL= scripts/run-local-worker.sh`.
