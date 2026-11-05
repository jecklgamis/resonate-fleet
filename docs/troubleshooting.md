# Troubleshooting

## `docker run` says the container name is already in use

```
docker: Error response from daemon: Conflict. The container name "/resonate-worker"
is already in use by container "...".
```

A container with that `--name` already exists, running or stopped, from an earlier
`docker run`. Remove it, then run again:

```sh
docker rm -f resonate-worker
```

The quick-start commands avoid this: they use `--rm` (the container is removed when
it exits) and no `--name`, so you can paste them as often as you like. It happens with
the long-lived `docker run -d --name ...` examples in [Deployment](deployment.md),
or after a named container failed to start (for example because port 58080 was
already in use, which leaves it in the `Created` state). Check what is using the
port with `lsof -nP -iTCP:58080 -sTCP:LISTEN`, or map a different host port:
`-p 58081:58080`.

## A task fails immediately

`GET /task/{id}/log` first. Common causes:

| What the log (or `error`) says | Cause |
| --- | --- |
| `exec: "resonate": executable file not found in $PATH` | The worker can't find `resonate`. Install it, or set `RESONATE_BIN` to its path. The worker doesn't check at startup, so this first appears on the first upload |
| A YAML or schema error from resonate | The scenario is invalid. The worker accepts any upload; resonate rejects it. Test the file with `resonate run scenario.yaml` locally first |
| `connection refused` or timeouts in the report | The target isn't reachable **from the worker**. `localhost` in a scenario is the worker's own machine or container |
| Assertion failures at the end of the log | The run worked; the thresholds weren't met. The report is still available |

## Submitting by URL fails

| Response | Cause |
| --- | --- |
| `403 scenario URLs are disabled` | The worker was started without `SCENARIO_SOURCES` |
| `403 … not under an allowed source` | The URL (or a redirect it followed) is outside the allowlist. Compare scheme, host and port exactly, and check the path prefix |
| `400 invalid scenario URL` | Malformed, no host, or not `http(s)` — `s3://` is unsupported, use a pre-signed `https` URL |
| `502 fetching scenario` | The source was unreachable, timed out, or returned non-2xx. A pre-signed URL that has expired shows up here |
| `413` | The scenario is over 1 MiB |

The fetch happens from the **worker**, so the source has to be reachable from there.

## `401 missing or bad Authorization header`

The header must be exactly `Authorization: Bearer <token>` with the token the worker
was started with. `bearer` in lowercase and `Basic` are rejected. Running the worker
without `API_TOKEN` disables auth entirely.

## A report URL returns `404`

Either the task id is wrong, or resonate hasn't written that file yet (the message
includes the task's current status). Check `status`, wait for `done` or `failed`, and
retry.

## `status` is stuck on `running`

The worker can't tell a hung `resonate` from a long one. Compare the elapsed time
with the scenario's `load.duration`, and check the worker's CPU. There is no cancel
endpoint; restarting the worker stops the run and marks it `failed` on the next
start.

## Tasks vanished after a restart

Task state lives in `WORKSPACE_DIR`. If the worker runs in a container without a
volume, or the pod has no PVC, that directory is new on every start. Mount storage
there — see [Deployment](deployment.md). Workspaces from before persistence
existed (no `task.json`) are also not loaded.

## A task I started shows `failed` with "worker restarted"

The worker stopped while it was `queued` or `running`. See
[Operations](operations.md#tasks-that-were-running-when-the-worker-stopped);
resubmit it.

## Results look slower than the target really is

Several tasks running at once compete for the worker's resources, and the load
generator becomes the bottleneck. See [Operations](operations.md#concurrency).

## `resonatectl run` exits non-zero

At least one worker's task didn't finish `done`, or a worker was unreachable. The
table's `ERROR` column says which; each reachable worker's log is saved under
`resonatectl-run-<timestamp>/<worker-name>/log`.

## `resonatectl` says there is no controller, or gets `401`

Pass `--context` or `--controller URL`, export `RESONATE_CONTROLLER` and
`CONTROLLER_TOKEN`, or set `current-context` in the
[config file](resonatectl.md#config-file) (`resonatectl config view` shows what it
sees). A `401` means the token is missing or
wrong; `409 no live workers match` means no worker has registered within the last
30 seconds (or none has the `--label` you asked for) - check
`resonatectl workers`.
