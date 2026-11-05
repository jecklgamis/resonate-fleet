# resonatectl

`resonatectl` is the command-line client for a [controller](controller.md). It runs
one scenario on every worker registered with the controller (or a labelled subset)
and pulls back each worker's report. It talks to the controller only: you need a
route to the controller, not to the workers.

**It never combines the reports.** Each worker runs the scenario on its own and
reports on its own slice of traffic. If you need one number across workers, combine
each `report.json` yourself (sum the request counts; do **not** average
percentiles).

## Build

```sh
make build        # builds ./resonatectl, ./resonate-worker and ./resonate-controller
# or
go build -o resonatectl ./cmd/resonatectl
```

The worker image also carries it at `/app/bin/resonatectl`.

## Configure

`resonatectl` needs a controller URL and its token. Give them with a config file,
environment variables, or flags. The token is never a flag, so it stays out of shell
history and `ps`.

### Config file

The usual pattern: a file of named **contexts**, one per controller, like kubectl's.
It lives at `~/.config/resonatectl/config.yaml` (`$XDG_CONFIG_HOME/resonatectl/` if
that is set); override it with `--config FILE` or `$RESONATECTL_CONFIG`.

```yaml
current-context: local
contexts:
  local:
    controller: http://localhost:58090
    token: ctl-secret                    # stored in the file, as kubectl does
  prod:
    controller: https://resonate-controller.example.com
    token_env: PROD_CONTROLLER_TOKEN     # read from this environment variable
  staging:
    controller: https://staging.example.com
    token_file: ~/.secrets/resonate-staging   # read from this file
```

Give a context at most one of `token`, `token_env` and `token_file`. Create the file
with `chmod 600`; `resonatectl` warns if a file holding an inline `token` is readable
by other users. Unknown fields are an error, so a misspelling is not silently ignored.

```sh
resonatectl workers                          # uses current-context
resonatectl --context prod run scenario.yaml # or: resonatectl run --context prod ...
resonatectl config view                      # the contexts, tokens hidden
resonatectl config use prod                  # switch current-context
```

`config use` edits only the `current-context` line, so your comments and layout are
kept.

### Which setting wins

Highest first:

1. **Flags:** `--controller URL` and `--context NAME`.
2. **Environment:** `$RESONATE_CONTROLLER` and `$CONTROLLER_TOKEN`.
3. **The config file's `current-context`.**

An explicit `--context` beats the environment, so a leftover `export` can't send
`--context prod` to the wrong controller. `--controller` still overrides that
context's URL. Without a config file, the environment variables work as before:

```sh
export RESONATE_CONTROLLER=https://resonate-controller.example.com
export CONTROLLER_TOKEN=...
```

## `resonatectl workers`

Lists the workers registered with the controller:

```
NAME                              URL                      LABELS            LAST SEEN
resonate-worker-66654b7d86-n8m7x  http://10.108.0.63:58080 cluster=do-sgp1   3s ago
resonate-worker-66654b7d86-pc8jd  http://10.108.0.64:58080 cluster=do-sgp1   6s ago
```

`--label key=value` (repeatable) shows only workers with that label.

## `resonatectl run`

```sh
resonatectl run scenario.yaml
resonatectl run --label region=sgp1 --data users.csv scenario.yaml
```

```
run 0cc792e71eaed4b6 started on 2 worker(s): resonate-worker-…-n8m7x, resonate-worker-…-pc8jd
WORKER                            STATUS  REQUESTS  SUCCESS  P95        DURATION
resonate-worker-66654b7d86-n8m7x  done    219       100.0%   5.12ms     9.95s
resonate-worker-66654b7d86-pc8jd  done    219       100.0%   4.89ms     9.95s
reports in resonatectl-run-1762333200
```

Each worker's `report.html`, `report.json`, `results.jsonl` and `log` land in
`resonatectl-run-<timestamp>/<worker-name>/`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--context` | `current-context` | Context to use from the [config file](#config-file) |
| `--controller` | | Controller URL (overrides the context's) |
| `--config` | `~/.config/resonatectl/config.yaml` | Config file |
| `--label k=v` | *(all workers)* | Only workers with this label (repeatable) |
| `--data file` | | A feeder/body file to upload with the scenario (repeatable; not with a scenario URL) |
| `--out dir` | `./resonatectl-run-<unix-timestamp>` | Where the reports go |
| `--poll` | `2s` | How often to check progress |

The scenario can go anywhere on the command line, before or after the flags.

### Scenario by URL

Pass a URL instead of a file and each worker fetches it itself. Nothing is uploaded
from your machine, so every worker sees the same bytes. The URL has to be under
each worker's `SCENARIO_SOURCES`; a worker where it isn't shows up as an `error`
row. See [Scenarios by URL](usage.md#scenarios-by-url).

### Exit code

Non-zero if any worker's task didn't finish `done`, including one that was
unreachable, so it works as a CI gate the same way `resonate run`'s exit code does.
It also exits non-zero if the controller can't be reached or no live worker matches.
