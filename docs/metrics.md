# Metrics

While a task runs, the worker can push live load-test metrics over OTLP/HTTP, so
you can watch a run in Grafana, Datadog or anything that takes OpenTelemetry. It
reads them from resonate's `results.jsonl` as resonate writes it, so resonate is
unchanged. With no endpoint configured, nothing is sent.

## Enabling it

Set the standard OpenTelemetry endpoint variable on the worker:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318 ./resonate-worker
```

`OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS` and the other
`OTEL_EXPORTER_OTLP_*` variables work as the OpenTelemetry SDK defines them. The
export interval is `OTEL_METRIC_EXPORT_INTERVAL` in milliseconds; the worker uses
10 seconds unless you set it (the SDK's own default is 60 seconds). In Helm, set
`otel.endpoint` (and optionally `otel.exportInterval`).

Point it at an [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/),
which can forward to Prometheus (remote write), Datadog and others.

## Trying it locally

`deployment/local/` has a Docker Compose stack: an OpenTelemetry Collector that
takes the worker's OTLP, Prometheus, and Grafana with a ready-made `resonate-fleet`
dashboard (requests per second by worker and status class, p95 latency, errors,
tasks running).

```sh
docker compose -f deployment/local/docker-compose.yaml up -d
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 OTEL_METRIC_EXPORT_INTERVAL=2000 \
  WORKER_LABELS=region=local scripts/run-local-worker.sh
scripts/run-local-scenario.sh      # with scripts/run-local-controller.sh running
```

Open Grafana at <http://127.0.0.1:3000> (anonymous admin) and the dashboard
`resonate-fleet`. Prometheus is at <http://localhost:9090>, and
`curl localhost:8889/metrics` shows what the collector exposes. If something else
already uses port 3000, change the host side of the `grafana` port mapping. Stop
the stack with `docker compose -f deployment/local/docker-compose.yaml down`.

In Prometheus the metrics are named `resonate_worker_requests_total`,
`resonate_worker_errors_total` (present once an error has happened),
`resonate_worker_request_duration_milliseconds_{bucket,sum,count}` and
`resonate_worker_tasks_running`.

## What is sent

| Metric | Type | Attributes |
| --- | --- | --- |
| `resonate.worker.requests` | counter | `status_class` (`2xx`, `4xx`, `5xx`, `error`, `none`), `protocol` |
| `resonate.worker.errors` | counter | same; counts errors and HTTP status 400 and up |
| `resonate.worker.request.duration` | histogram, ms | same |
| `resonate.worker.tasks.running` | up/down counter | |

Every metric carries resource attributes `service.name=resonate-worker`,
`service.instance.id` and `worker` (the worker's name), plus each of the worker's
`WORKER_LABELS` as an attribute (for example `region=sg`). Sum across workers, or
split by `worker` or `region`, in your dashboard. Percentiles come from the
histogram, so they are approximate (fixed buckets from 1 ms to 10 s).

The task ID is not an attribute, to keep cardinality low.

## Limits

- Metrics are cumulative counters, sampled every export interval. A short test
  gives few points; `results.jsonl` and the reports remain the exact record.
- The worker tails the file about twice a second, so metrics trail the load by a
  moment. A final read after resonate exits makes sure the last requests count,
  and the metrics are flushed on shutdown.
- Exporting runs in the background: an unreachable endpoint never slows or fails a
  task.
