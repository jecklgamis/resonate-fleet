package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Metrics pushes live load-test metrics over OTLP/HTTP while a task runs. The
// numbers come from tailing resonate's results.jsonl, so resonate itself is
// untouched. A nil *Metrics is valid and does nothing: that is how a worker with
// no OTLP endpoint configured behaves.
type Metrics struct {
	provider *sdkmetric.MeterProvider
	requests metric.Int64Counter
	errors   metric.Int64Counter
	latency  metric.Float64Histogram
	running  metric.Int64UpDownCounter
}

// MetricsFromEnv returns nil unless an OTLP endpoint is configured through the
// standard OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_METRICS_ENDPOINT.
// Everything else (headers, TLS, timeouts, OTEL_METRIC_EXPORT_INTERVAL) is the
// OpenTelemetry SDK's own environment handling. name and labels identify the
// worker: they become resource attributes on every metric.
func MetricsFromEnv(ctx context.Context, name string, labels map[string]string) (*Metrics, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") == "" {
		return nil, nil
	}
	exp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP metrics exporter: %w", err)
	}
	attrs := []attribute.KeyValue{
		attribute.String("service.name", "resonate-worker"),
		attribute.String("service.version", version),
		attribute.String("service.instance.id", name),
		attribute.String("worker", name),
	}
	for k, v := range labels {
		attrs = append(attrs, attribute.String(k, v))
	}
	res, err := resource.New(ctx, resource.WithAttributes(attrs...))
	if err != nil {
		return nil, err
	}
	// The SDK exports every 60s by default, too coarse for a test that lasts a
	// minute or two. Unless the operator chose an interval, use 10s.
	reader := []sdkmetric.PeriodicReaderOption{}
	if os.Getenv("OTEL_METRIC_EXPORT_INTERVAL") == "" {
		reader = append(reader, sdkmetric.WithInterval(10*time.Second))
	}
	return newMetrics(sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, reader...)),
	))
}

func newMetrics(p *sdkmetric.MeterProvider) (*Metrics, error) {
	m := &Metrics{provider: p}
	meter := p.Meter("resonate-worker")
	var err error
	if m.requests, err = meter.Int64Counter("resonate.worker.requests",
		metric.WithDescription("Requests completed by resonate"), metric.WithUnit("{request}")); err != nil {
		return nil, err
	}
	if m.errors, err = meter.Int64Counter("resonate.worker.errors",
		metric.WithDescription("Requests that failed"), metric.WithUnit("{request}")); err != nil {
		return nil, err
	}
	if m.latency, err = meter.Float64Histogram("resonate.worker.request.duration",
		metric.WithDescription("Request latency"), metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000)); err != nil {
		return nil, err
	}
	if m.running, err = meter.Int64UpDownCounter("resonate.worker.tasks.running",
		metric.WithDescription("Tasks currently running"), metric.WithUnit("{task}")); err != nil {
		return nil, err
	}
	return m, nil
}

// Shutdown flushes pending metrics. Call it before the process exits.
func (m *Metrics) Shutdown(ctx context.Context) {
	if m == nil {
		return
	}
	if err := m.provider.Shutdown(ctx); err != nil {
		log.Printf("metrics: flushing on shutdown: %v", err)
	}
}

// resultLine is the part of a results.jsonl line the metrics use.
type resultLine struct {
	LatencyMs  float64 `json:"latency_ms"`
	StatusCode int     `json:"status_code"`
	Protocol   string  `json:"protocol"`
	Error      string  `json:"error"`
}

func (m *Metrics) record(ctx context.Context, r resultLine) {
	attrs := metric.WithAttributes(
		attribute.String("status_class", statusClass(r.StatusCode, r.Error)),
		attribute.String("protocol", r.Protocol),
	)
	m.requests.Add(ctx, 1, attrs)
	m.latency.Record(ctx, r.LatencyMs, attrs)
	if r.Error != "" || r.StatusCode >= 400 {
		m.errors.Add(ctx, 1, attrs)
	}
}

// statusClass is "2xx", "4xx" and so on, or "error" for a request that failed
// without a response. A status code of 0 with no error (a protocol that has no
// HTTP status) is "none".
func statusClass(code int, errMsg string) string {
	switch {
	case code >= 100 && code < 600:
		return strconv.Itoa(code/100) + "xx"
	case errMsg != "":
		return "error"
	}
	return "none"
}

// TrackTask follows the task's results.jsonl until the returned function is
// called, which drains whatever resonate wrote last. It never blocks the run.
func (m *Metrics) TrackTask(path string) (stop func()) {
	if m == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	m.running.Add(ctx, 1)
	go func() {
		defer close(done)
		tailResults(ctx, path, func(r resultLine) { m.record(context.Background(), r) })
	}()
	return func() {
		cancel()
		<-done
		m.running.Add(context.Background(), -1)
	}
}

// tailResults reads complete lines from path as resonate appends them, calling
// fn for each. When ctx is done it reads to the end once more and returns. The
// file may not exist yet, and a half-written last line is held back until its
// newline arrives. Lines that are not JSON are skipped.
func tailResults(ctx context.Context, path string, fn func(resultLine)) {
	var f *os.File
	var pending []byte
	r := bufio.NewReader(nil)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	drain := func() {
		if f == nil {
			var err error
			if f, err = os.Open(path); err != nil {
				f = nil
				return
			}
			r.Reset(f)
		}
		for {
			chunk, err := r.ReadBytes('\n')
			pending = append(pending, chunk...)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					log.Printf("metrics: reading %s: %v", path, err)
				}
				return
			}
			var line resultLine
			if json.Unmarshal(pending, &line) == nil {
				fn(line)
			}
			pending = pending[:0]
		}
	}
	for {
		select {
		case <-ctx.Done():
			drain()
			return
		case <-tick.C:
			drain()
		}
	}
}
