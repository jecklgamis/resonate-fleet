# API Reference

All endpoints except `/` and `/healthz` need `Authorization: Bearer <API_TOKEN>` when the
worker was started with an `API_TOKEN`. Errors are JSON: `{"error": "<message>"}`.

| Method | Path | Returns |
| --- | --- | --- |
| `POST` | `/task[?startAt=T]` | Start a task. Either a multipart upload (field `file` = the scenario YAML, plus optional `files` fields for data files such as feeders) or JSON `{"scenarioUrl": "..."}`. `202 Accepted` with `{"taskId": "..."}`. With `startAt` (an RFC 3339 time, at most 10 minutes ahead) the task waits `queued` until then; the response then also has `startAt` and `now` (the worker's clock). `400` for a bad `startAt` |
| `GET` | `/task` | Every task this worker knows about |
| `GET` | `/task/{id}` | That task's status |
| `DELETE` | `/task/{id}` | Cancel a running task: resonate is interrupted (and killed after 10s), and the task ends as `canceled`. `202` if it was running, `200` with the final state if it was not, `404` if unknown |
| `GET` | `/task/{id}/report.html` | resonate's HTML report |
| `GET` | `/task/{id}/report.json` | resonate's JSON report |
| `GET` | `/task/{id}/results.jsonl` | resonate's raw per-request results |
| `GET` | `/task/{id}/log` | resonate's stdout and stderr for that run |
| `GET` | `/` | `200` `{"app":"resonate-worker","message":"Relax, perf test it"}` — exempt from auth |
| `GET` | `/healthz` | `200 ok` — exempt from auth, for a load balancer health check |

## `POST /task`

```sh
curl -H "Authorization: Bearer $TOKEN" -F "file=@scenario.yaml" $WORKER/task
```

```json
{"taskId":"c55286ad94a65b72"}
```

`?startAt=2026-11-07T10:00:00Z` holds the task until that time, so several workers can
start together (the controller's `start_delay` uses it). A time already past starts at
once. A task can be canceled with `DELETE /task/{id}` while it waits.

To have the worker fetch the scenario instead of uploading it, send JSON:

```sh
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     -d '{"scenarioUrl":"https://config.example.com/scenarios/checkout.yaml"}' $WORKER/task
```

This only works for URLs under the worker's `SCENARIO_SOURCES` allowlist — see
[Scenarios by URL](usage.md#scenarios-by-url). The worker fetches the file
*before* answering, so a bad or unreachable URL is an error here, not a failed
task later.

| Status | When |
| --- | --- |
| `202` | Accepted; the run starts in the background |
| `400` | Not multipart and not valid JSON, no `file` field or `scenarioUrl`, or the URL is malformed or not `http(s)` |
| `401` | Missing or wrong bearer token |
| `403` | `scenarioUrl` is not under an allowed source, or URL submissions are disabled (`SCENARIO_SOURCES` unset) |
| `413` | The fetched scenario is over the 1 MiB size limit |
| `500` | The worker couldn't create the task workspace or save the scenario |
| `502` | The source was unreachable, timed out, or answered with a non-2xx status |

The worker saves the scenario as `scenario.yaml` in the task's workspace and
remembers its name for display (the uploaded filename, or the last path segment of
the URL). It does **not** validate the YAML —
a bad scenario is accepted, then ends as a `failed` task with resonate's
complaint in the [log](#get-taskidlog).

## `GET /task/{id}`

```json
{
  "id": "c55286ad94a65b72",
  "scenario": "checkout.yaml",
  "source": "https://config.example.com/scenarios/checkout.yaml",
  "sha256": "f958493c68511aa1bc9209e3f89c7056dca6c3ca419c43d88d31db8846194e14",
  "status": "failed",
  "exit_code": 1,
  "error": "exit status 1",
  "created_at": "2026-11-05T11:46:42.762131+11:00",
  "finished_at": "2026-11-05T11:46:52.778565+11:00"
}
```

| Field | Meaning |
| --- | --- |
| `id` | The task id (16 hex characters) |
| `start_at` | When the task was asked to start, if it was submitted with `startAt` |
| `scenario` | The uploaded file's original name (or the URL's last path segment), for display only |
| `source` | The URL the scenario was fetched from. Omitted for uploads |
| `sha256` | Hash of the exact scenario bytes the worker saved and ran |
| `status` | `queued`, `running`, `done` or `failed` — see [Usage](usage.md#2-poll-the-task) |
| `exit_code` | resonate's exit code. Omitted when `0` |
| `error` | Why a run failed (e.g. `exit status 1`, or `worker restarted before the task finished`). Omitted when empty |
| `created_at` | When the task was submitted |
| `finished_at` | When the run ended. Until then it is the zero timestamp `0001-01-01T00:00:00Z` |

Unknown ids return `404` with `{"error":"no such task"}`.

## `GET /task`

A JSON array of the same objects `GET /task/{id}` returns, in no particular
order.

## `GET /task/{id}/report.html`, `report.json`, `results.jsonl`, `log`

Each serves one file from the task's workspace as-is:

| Path | Content-Type |
| --- | --- |
| `report.html` | `text/html; charset=utf-8` |
| `report.json` | `application/json` |
| `results.jsonl` | `application/x-ndjson` |
| `log` | `text/plain; charset=utf-8` |

A `404` means either the task doesn't exist or resonate hasn't written that file
yet (the error message includes the task's current status). Reports are only
complete once `status` is `done` or `failed`.

The file names are fixed — there is no caller-supplied path, so nothing here can
be used to read other files from the host.

## `GET /healthz`

```sh
curl http://localhost:58080/healthz
# ok
```

Always `200`, always open. Use it for liveness and readiness probes; the Helm
chart does.

## Authentication

```sh
curl -H "Authorization: Bearer some-secret-token" $WORKER/task
```

The header must be exactly `Bearer <token>` — the scheme is case-sensitive.
Anything else is `401` with `{"error":"missing or bad Authorization header"}`.

With no `API_TOKEN` set, **there is no auth at all**. That is fine on localhost
and not for anything reachable over a network.
