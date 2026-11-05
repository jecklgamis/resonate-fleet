# Usage

The loop is always the same: **submit** a scenario, **poll** its task, **fetch**
the results.

In the examples below, `TOKEN` is your `API_TOKEN`. If you started the worker
without one, leave the `Authorization` header out.

```sh
export TOKEN=some-secret-token
export WORKER=http://localhost:58080
```

## 1. Submit a Scenario

Upload the scenario YAML as the multipart field `file`:

```sh
curl -H "Authorization: Bearer $TOKEN" -F "file=@examples/scenario.yaml" $WORKER/task
# {"taskId":"c55286ad94a65b72"}
```

The worker answers `202 Accepted` straight away and runs
`resonate run scenario.yaml` in the background. The scenario is whatever
`resonate run` accepts — here is the sample in
[`examples/scenario.yaml`](https://github.com/jecklgamis/resonate-worker/blob/main/examples/scenario.yaml):

```yaml
protocol: http

load:
  duration: 10s
  rate: 20
  workers: 5

http:
  timeout: 5s
  targets:
    - method: GET
      url: http://localhost:8080/health   # point this at the service you want to load

assertions:
  - metric: success_rate
    min: "0.99"
  - metric: latency_p95
    max: 500ms
```

> The load runs **from the worker**, so the target URL must be reachable from
> wherever the worker runs — `localhost` means the worker's own machine (or
> container), not yours.

## Scenarios by URL

Instead of uploading the file, you can tell the worker where it lives — a config
server, a git host's raw URL, or an S3 pre-signed URL:

```sh
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     -d '{"scenarioUrl":"https://config.example.com/scenarios/checkout.yaml"}' \
     $WORKER/task
# {"taskId":"c55286ad94a65b72"}
```

Everything after that — polling, reports, the log — is identical.

**It is off until you allow sources.** A worker that fetches whatever URL it is
handed can be pointed at internal services, so set `SCENARIO_SOURCES` to the
prefixes it may fetch from:

```sh
SCENARIO_SOURCES=https://config.example.com/scenarios,https://raw.githubusercontent.com/acme/loadtests/main \
  API_TOKEN=some-secret-token ./resonate-worker
```

| Rule | Detail |
| --- | --- |
| Matching | Scheme and host must match exactly, and the path must be the prefix or sit beneath it. `…/scenarios` allows `…/scenarios/a.yaml`, not `…/scenarios-evil/a.yaml` or `…/scenarios/../secret` |
| Redirects | Every hop must also match the allowlist; up to 3 |
| Schemes | `http` and `https` only. `s3://` is not supported — use a pre-signed `https` URL |
| Limits | 10 second fetch timeout, 1 MiB size cap (a longer file is refused, never truncated) |
| Source auth | Set `SCENARIO_SOURCE_TOKEN` and the worker sends it as `Authorization: Bearer …` on every fetch |

**The fetched file is snapshotted.** The worker saves the exact bytes as the task's
`scenario.yaml` and records the `source` URL and a `sha256` in the task's status, so
"what did this run?" has an answer even if the URL's content changes later.

An S3 pre-signed URL works with no AWS support in the worker, because it is just an
HTTPS URL:

```sh
URL=$(aws s3 presign s3://my-bucket/scenarios/checkout.yaml --expires-in 300)
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     -d "{\"scenarioUrl\":\"$URL\"}" $WORKER/task
```

(add `https://my-bucket.s3.amazonaws.com/` to `SCENARIO_SOURCES` first).

## 2. Poll the Task

```sh
curl -H "Authorization: Bearer $TOKEN" $WORKER/task/c55286ad94a65b72
```

```json
{"id":"c55286ad94a65b72","scenario":"scenario.yaml","status":"done","created_at":"2026-11-05T11:46:42+11:00","finished_at":"2026-11-05T11:46:52+11:00"}
```

`status` is one of:

| Status | Meaning |
| --- | --- |
| `queued` | Accepted; the `resonate` subprocess has not started yet |
| `running` | `resonate run` is executing |
| `done` | `resonate` exited 0 |
| `failed` | `resonate` exited non-zero, or could not start |

`failed` covers both a scenario resonate couldn't even start (bad YAML) and one
that ran but whose `assertions:` didn't pass — resonate's own non-zero exit code
either way. The `exit_code` and `error` fields say which; the [log](#4-read-the-log)
has the details.

## 3. Fetch the Results

Once `status` is `done` or `failed`:

```sh
curl -H "Authorization: Bearer $TOKEN" $WORKER/task/c55286ad94a65b72/report.html -o report.html
curl -H "Authorization: Bearer $TOKEN" $WORKER/task/c55286ad94a65b72/report.json -o report.json
curl -H "Authorization: Bearer $TOKEN" $WORKER/task/c55286ad94a65b72/results.jsonl -o results.jsonl
```

A **failed run still writes its report**, so a failed assertion is something
you can open and look at. See resonate's [Reports](https://jecklgamis.github.io/resonate/#/reports)
page for what each file contains.

A report path answers `404` until resonate has actually written that file — check
`status` first, or just retry.

## 4. Read the Log

```sh
curl -H "Authorization: Bearer $TOKEN" $WORKER/task/c55286ad94a65b72/log
```

The log is resonate's stdout and stderr for that run: the text summary on
success, the complaint on a bad scenario, the failed assertions after a run that
missed its thresholds.

## Listing Tasks

```sh
curl -H "Authorization: Bearer $TOKEN" $WORKER/task
```

Returns every task this worker knows about, including ones from before a
restart. See [Operations](operations.md#persistence).

## Gating a CI Pipeline

Because assertion failures surface as `failed`, a pipeline step can submit, wait,
and exit non-zero:

```sh
#!/usr/bin/env bash
set -euo pipefail
auth=(-H "Authorization: Bearer $TOKEN")

id=$(curl -fsS "${auth[@]}" -F "file=@scenario.yaml" "$WORKER/task" | jq -r .taskId)

while :; do
  status=$(curl -fsS "${auth[@]}" "$WORKER/task/$id" | jq -r .status)
  [ "$status" = done ] || [ "$status" = failed ] && break
  sleep 2
done

curl -fsS "${auth[@]}" "$WORKER/task/$id/report.html" -o report.html   # keep as a CI artifact
[ "$status" = done ] || { curl -fsS "${auth[@]}" "$WORKER/task/$id/log"; exit 1; }
```

To run the same scenario against several workers at once, use
[`resonate-fleet`](fleet.md) instead of scripting this loop per worker.
