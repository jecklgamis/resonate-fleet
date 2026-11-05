# Fleet

`resonate-fleet` fans one scenario out to several workers at once — for example,
one worker per region or AZ — polls each independently, and pulls back each
worker's report.

**It never combines the reports.** Each worker runs the scenario on its own and
reports on its own slice of traffic. The fleet tool just replaces a shell loop of
`curl` calls. There is no coordinator and no shared state, so one worker running
slow or failing never affects another worker's run.

## Build

```sh
make build        # also builds ./resonate-worker
# or
go build -o resonate-fleet ./cmd/resonate-fleet
```

## workers.yaml

List the workers to fan out to. A sample is in
[`examples/workers.yaml`](https://github.com/jecklgamis/resonate-worker/blob/main/examples/workers.yaml):

```yaml
workers:
  - name: us-east
    url: https://worker-us-east.example.com
    token_env: US_EAST_TOKEN   # read from this env var; never put a real token in the file
  - name: eu-west
    url: https://worker-eu-west.example.com
    token_env: EU_WEST_TOKEN
```

| Field | Meaning |
| --- | --- |
| `name` | Used as the output subdirectory and the table's row label; must be unique |
| `url` | The worker's base URL, no trailing slash |
| `token_env` | Env var to read the bearer token from (preferred) |
| `token` | A literal token in the file instead — only for a quick local trial; don't commit a real one this way |

Setting both `token` and `token_env` on one worker, or naming a `token_env`
variable that isn't set, is an error at startup — before anything is submitted.

For a single local worker:

```yaml
workers:
  - name: local
    url: http://localhost:58080
    token_env: API_TOKEN
```

## Run

```sh
US_EAST_TOKEN=... EU_WEST_TOKEN=... \
  ./resonate-fleet run --workers workers.yaml scenario.yaml
```

```
submitting scenario.yaml to 2 worker(s), writing reports under fleet-run-1762333200
WORKER    STATUS  REQUESTS  SUCCESS  P95      DURATION  ERROR
eu-west   done    1500      99.8%    142ms    30.01s
us-east   done    1500      100.0%   118ms    30.01s
```

Each worker's `report.html`, `report.json`, `results.jsonl` and `log` land under
`./fleet-run-<timestamp>/<worker-name>/`.

## Scenario by URL

Instead of a local file, pass a URL and each worker fetches it itself:

```sh
./resonate-fleet run --workers workers.yaml \
  https://config.example.com/scenarios/checkout.yaml
```

Nothing is uploaded from your machine, so every worker sees exactly the same bytes
(each records the `sha256` in its task status). The URL has to be under every
worker's `SCENARIO_SOURCES`; a worker where it isn't answers `403`, shows up as an
`error` row in the table, and doesn't affect the others. See
[Scenarios by URL](usage.md#scenarios-by-url).

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--workers` | *(required)* | Path to the `workers.yaml` listing which instances to fan out to |
| `--out` | `./fleet-run-<unix-timestamp>` | Where to write each worker's reports, one subdirectory per worker |
| `--poll` | `2s` | How often to check each worker's task status |

The scenario path can go anywhere on the command line — before, after, or
between the flags.

## Exit Code

Non-zero if any worker's task didn't finish `done` — including one that was
unreachable — so it works as a CI gate the same way `resonate run`'s exit code
does.

## Combining Results

The fleet tool deliberately stops at "one report per worker". If you need one
number across regions, take each worker's `report.json` and combine them yourself
(sum the request counts; do **not** average percentiles). See resonate's own docs
for the "independent instances, independent reports" pattern.
