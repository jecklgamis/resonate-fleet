# Install

## Using Docker

```sh
docker run -it --name resonate-worker -p 58080:58080 \
  -e API_TOKEN=some-secret-token jecklgamis/resonate-worker:main
```

The image bundles a pinned `resonate` release (the `RESONATE_VERSION` build arg
in `deployment/docker/worker/Dockerfile`) — there is nothing else to install. It runs as a non-root
user and keeps task workspaces under `/app/workspace`.

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
by default.

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
make test      # go test ./...
make check     # fmt-check + vet + test
```

The tests include integration tests that run the real `resonate` against a
throwaway local HTTP target. They skip themselves when `resonate` isn't found,
so `make test` passes on a bare machine.
