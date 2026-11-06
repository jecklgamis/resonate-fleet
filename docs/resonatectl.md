# resonatectl

`resonatectl` is the command-line client for a [controller](controller.md). It runs
one scenario on every worker registered with the controller (or a labelled subset)
and pulls back each worker's report. It talks to the controller only: you need a
route to the controller, not to the workers.

Each worker runs the scenario on its own and reports on its own slice of traffic.
`run` also prints a **fleet total** over all of them and can fail the run on
[thresholds](#thresholds). Each worker's own report is left untouched.

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

Give a context at most one of `token`, `token_env` and `token_file`. `resonatectl`
warns if a file holding an inline `token` is readable by other users; fix it with
`chmod 600` (a file it creates already is). Unknown fields are an error, so a misspelling is not silently ignored.

```sh
resonatectl workers                          # uses current-context
resonatectl --context prod run scenario.yaml # or: resonatectl run --context prod ...
resonatectl config view                      # the contexts, tokens hidden
resonatectl config use prod                  # switch current-context
```

### Creating and editing contexts

You can write the file by hand, or let `resonatectl` do it:

```sh
# the token comes from standard input, so it never reaches your shell history
printf '%s' "$TOKEN" | resonatectl config set-context prod \
  --controller https://resonate-controller.example.com --token-stdin --use

resonatectl config set-context local --controller http://localhost:58090 --token ctl-secret
resonatectl config set-context staging --controller https://staging.example.com --token-env STAGING_TOKEN
resonatectl config set-context staging --token-file ~/.secrets/resonate-staging   # change one field
resonatectl config delete-context staging
```

| Flag | Meaning |
| --- | --- |
| `--controller URL` | The controller. Required when creating a context |
| `--token-stdin` | Read the token from standard input and store it in the file. **Recommended** for a stored token |
| `--token T` | Store this token in the file. It lands in your shell history and in `ps`, so prefer `--token-stdin` |
| `--token-env VAR` | Read the token from this environment variable each time |
| `--token-file FILE` | Read the token from this file each time |
| `--use` | Also make this the current context |

- Give at most one token option. Choosing one replaces whatever the context had.
  Flags you leave out keep their current value, so you can change just the token or
  just the controller.
- The first context you create becomes the current one. Later ones don't take over
  unless you pass `--use`.
- A new file is created with mode `0600` in a `0700` directory.
- Edits change only what you asked for and keep your comments and layout. If the
  file can't be loaded (for example a misspelled field), `set-context` refuses to
  touch it, and it never replaces a valid file with an invalid one.
- `delete-context` on the current context clears `current-context`; pick another with
  `config use`.

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
resonatectl run --workers 4 scenario.yaml          # 4 random workers
resonatectl run --label region=sgp1 --workers 25% scenario.yaml
```

```
run 0cc792e71eaed4b6 started on 2 worker(s): resonate-worker-…-n8m7x, resonate-worker-…-pc8jd
WORKER                            STATUS  REQUESTS  SUCCESS  P95        DURATION
resonate-worker-66654b7d86-n8m7x  done    219       100.0%   5.12ms     9.95s
resonate-worker-66654b7d86-pc8jd  done    219       100.0%   4.89ms     9.95s

FLEET TOTAL  (2 worker(s))
requests    438
success     100.00%
throughput  43.9 req/s over 9.97s
latency     p50 3.98ms  p90 4.71ms  p95 5.01ms  p99 9.2ms  max 41.7ms

reports in resonatectl-run-1762333200
```

Each worker's `report.html`, `report.json`, `results.jsonl` and `log` land in
`resonatectl-run-<timestamp>/<worker-name>/`.

### Load across the fleet

```sh
resonatectl run --total-rate 500 scenario.yaml     # 500 req/s in all, however many workers
```

Without `--total-rate` each worker runs the scenario as written, so the fleet's load
is the scenario's rate times the number of workers. With it, the rate and user counts
are scaled to the total and split evenly, stages included. For example, 3 workers
and `--total-rate 90` against `rate: 20, workers: 5`:

```
run 2428b0fe7172390c started on 3 of 3 worker(s): w0, w1, w2
all workers start at 06:21:16.285 (in 4s)
WORKER  STATUS  REQUESTS  SUCCESS  P95       DURATION
w0      done    269       100.0%   8.52ms    7.97s
w1      done    269       100.0%   8.12ms    7.97s
w2      done    269       100.0%   7.36ms    7.97s
```

The workers wait for a shared start time (`--start-delay`, default 5s) so their
ramps line up; in that run their first requests landed within 2ms of each other. If
a worker started late, ignored the start time or has a drifting clock, a `warning:`
line says so after the table. An older controller that doesn't know `--total-rate` is
detected and the run is aborted, rather than hit your service at the unsplit rate.

### Fleet total

The total is also written to `resonatectl-run-<timestamp>/aggregate.json`, and drawn
over time in [`report.html`](#fleet-report). Request
and success counts are the sums of each worker's `report.json`, the duration is the
longest worker's, and throughput is total requests over that duration. Percentiles
are **exact**: they are computed over every individual request in every worker's
`results.jsonl`, not averaged from per-worker percentiles (which would be wrong). A
worker with no report is left out, and a `note:` line says so. A run that was
aborted, or a worker whose assertions failed, still contributes what it finished.

### Fleet report

`run` also writes `<out>/report.html`: one self-contained page (no scripts, no external
files, light and dark themes) for the whole run, where each worker's own `report.html`
covers only its slice. It has:

- tiles for the fleet total (requests, success, throughput, duration, p50, p95, p99, max);
- the [thresholds](#thresholds), with PASS or FAIL for each;
- **throughput** (requests and errors per second), **latency** (p50, p95, p99) and a
  **per-worker** requests-per-second chart, all over time, so a ramp, a dip, or one
  worker lagging is visible;
- a workers table with each one's status, requests, success rate, p95, how long after
  the first worker it began, and a link to its own `report.html`.

The curves are built from every request of every worker, placed by its timestamp, so
they are the fleet's own and not a sum of the workers' summaries. The tiles are exact;
the charts read percentiles from 5%-wide buckets, so they are within about 5%. A run
longer than 600 seconds is drawn in steps of several seconds. The time axis is only as
aligned as the workers' clocks (the report shows each worker's start offset). Building
it streams the result files: a million requests take under a second.

No report is written if no worker produced one.

### Thresholds

```sh
resonatectl run scenario.yaml \
  --threshold 'p95<300ms' --threshold 'success_rate>=99%' --threshold 'rps>=100'
```

Each `--threshold` is `metric<value` (or `<=`, `>`, `>=`) checked against the fleet
total:

| Metric | Value |
| --- | --- |
| `min` `mean` `p50` `p75` `p90` `p95` `p99` `max` | a duration: `300ms`, `1.5s` |
| `success_rate` `error_rate` | a fraction (`0.99`) or a percent (`99%`) |
| `rps` `requests` | a number |

```
THRESHOLDS
  PASS  p95<300ms  (got 5.01ms)
  FAIL  success_rate>=99.00%  (got 98.50%)
```

With no usable report, every threshold fails rather than passing on no data. Quote
the expression so your shell doesn't treat `<` and `>` as redirects.

### Watching a run

While `run` waits, it shows the whole fleet on one line, redrawn in place:

```
[13s] 3/3 workers  55.2 req/s  735 req  err 0.00%  p50 2.48ms  p95 3.16ms  p99 3.85ms
```

That is on by default on a terminal and off otherwise, so CI logs stay clean;
`--live on` prints a plain line every 5 seconds anywhere, and `--live off` turns it off.
The line goes to stderr, so the results table on stdout is unaffected.

`resonatectl watch <run-id>` follows a run that is already going, whoever started it,
with a row per worker:

```
run dcc10978ac76766a  running  6s
WORKER  STATUS   REQUESTS  ERRORS  REQ/S  P95     NOTE
w0      running  122       0       20.2   4.24ms
w1      running  122       0       20.2   4.68ms
w2      running  122       0       20.2   4.45ms
FLEET            366       0       60.6   4.68ms  p50 2.48ms  p99 15.83ms  max 16.39ms
```

It ends when the run does (exit `0` if the run finished `done`, `1` if it failed or was
canceled). **Ctrl-C only stops watching**; the run keeps going, unlike Ctrl-C on `run`,
which aborts it. The rate and latency are over the last 10 seconds, and the FLEET row's
percentiles are the whole fleet's; see [Watching a run](controller.md#watching-a-run)
on how they are computed and how precise they are. A controller without a live view
(an older one) is reported as an error rather than waited on.

### Stopping a run

Ctrl-C while `run` is waiting aborts the run on every worker (they stop generating
load), then reports what they finished. `resonatectl abort <run-id>` does the same
from another terminal. A worker's task ends as `canceled`, and so does the run. See
[`DELETE /runs/{id}`](controller.md#api).

| Flag | Default | Meaning |
| --- | --- | --- |
| `--context` | `current-context` | Context to use from the [config file](#config-file) |
| `--controller` | | Controller URL (overrides the context's) |
| `--config` | `~/.config/resonatectl/config.yaml` | Config file |
| `--label k=v` | *(all workers)* | Only workers with this label (repeatable) |
| `--workers N` or `P%` | *(all of them)* | Run on a random `N`, or `P%` (rounded up, at least 1), of the workers left after `--label`. More than are live is an error. See [Running on some of the workers](controller.md#running-on-some-of-the-workers) |
| `--data file` | | A feeder/body file to upload with the scenario (repeatable; not with a scenario URL) |
| `--total-rate R` | | Make `R` req/s the **fleet's** peak rate instead of each worker's: scale the scenario's load to it and split it across the workers. A scenario file only. See [One rate for the whole fleet](controller.md#one-rate-for-the-whole-fleet) |
| `--start-delay D` | `5s` | Have every worker wait `D` and start together; `0` starts each as it gets the scenario. Ignored with one worker. See [Starting together](controller.md#starting-together) |
| `--threshold expr` | | Fail unless the fleet total meets it, like `p95<300ms` (repeatable). See [Thresholds](#thresholds) |
| `--out dir` | `./resonatectl-run-<unix-timestamp>` | Where the reports go |
| `--poll` | `2s` | How often to check progress |

The scenario can go anywhere on the command line, before or after the flags.

### Scenario by URL

Pass a URL instead of a file and each worker fetches it itself. Nothing is uploaded
from your machine, so every worker sees the same bytes. The URL has to be under
each worker's `SCENARIO_SOURCES`; a worker where it isn't shows up as an `error`
row. See [Scenarios by URL](usage.md#scenarios-by-url).

### Exit code

| Code | Meaning |
| --- | --- |
| `0` | Every worker finished `done` and every `--threshold` held |
| `1` | A worker didn't finish `done` (including unreachable or canceled), the controller couldn't be reached, or no live worker matches |
| `2` | Bad usage or flags, including an invalid `--threshold` |
| `3` | Every worker was `done`, but a `--threshold` failed |

So it works as a CI gate the same way `resonate run`'s exit code does.
