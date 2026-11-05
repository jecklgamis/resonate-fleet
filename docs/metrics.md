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
