# resonate-fleet

Fans one scenario out to several [`resonate-worker`](../..) instances at
once — for example, one worker per region or AZ — polls each independently,
and pulls back each worker's report.

**It never combines the reports.** Each worker runs the scenario on its own
and reports on its own slice of traffic; this just automates submitting to
all of them and collecting what comes back, in place of a shell loop of
`curl` calls. See the main README's "What this isn't" for why: there's no
coordinator and no shared state, so one worker running slow or failing never
affects any other worker's run.

## Quick start

```sh
go build -o resonate-fleet ./cmd/resonate-fleet
```

List the workers to fan out to:

```yaml
# workers.yaml
workers:
  - name: us-east
    url: https://worker-us-east.example.com
    token_env: US_EAST_TOKEN   # read from this env var; never put a real token in the file
  - name: eu-west
    url: https://worker-eu-west.example.com
    token_env: EU_WEST_TOKEN
```

Run it:

```sh
US_EAST_TOKEN=... EU_WEST_TOKEN=... \
  resonate-fleet run --workers workers.yaml scenario.yaml
```

```
submitting scenario.yaml to 2 worker(s), writing reports under fleet-run-1762333200
WORKER    STATUS  REQUESTS  SUCCESS  P95      DURATION  ERROR
eu-west   done    1500      99.8%    142ms    30.01s
us-east   done    1500      100.0%   118ms    30.01s
```

Each worker's `report.html`/`report.json`/`results.jsonl`/`log` land under
`./fleet-run-<timestamp>/<worker-name>/` (override with `--out`).

Exit code is non-zero if any worker's task didn't finish `done` — including
one that was unreachable — so it's usable as a CI gate the same way
`resonate run`'s own exit code is.

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--workers` | *(required)* | Path to the `workers.yaml` listing which instances to fan out to |
| `--out` | `./fleet-run-<unix-timestamp>` | Where to write each worker's reports, one subdirectory per worker |
| `--poll` | `2s` | How often to check each worker's task status |

The scenario can also be a URL — `resonate-fleet run --workers workers.yaml
https://config.example.com/scenarios/checkout.yaml`. Each worker then fetches it
itself, so the URL has to be under that worker's `SCENARIO_SOURCES`; nothing is
uploaded from your machine.

The scenario path can go anywhere on the command line — before, after, or
between the flags.

## workers.yaml

| Field | Meaning |
| --- | --- |
| `name` | Used as the output subdirectory and the table's row label; must be unique |
| `url` | The worker's base URL, no trailing slash |
| `token_env` | Env var to read the bearer token from (preferred) |
| `token` | A literal token in the file instead — only for a quick local trial; don't commit a real one this way |

Setting both `token` and `token_env` on the same worker, or naming a
`token_env` variable that isn't set, is an error at startup, before anything
is submitted.
