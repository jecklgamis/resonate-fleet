package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestStatusClass(t *testing.T) {
	for _, c := range []struct {
		code int
		err  string
		want string
	}{{200, "", "2xx"}, {404, "", "4xx"}, {503, "x", "5xx"}, {0, "timeout", "error"}, {0, "", "none"}} {
		if got := statusClass(c.code, c.err); got != c.want {
			t.Errorf("statusClass(%d,%q) = %q, want %q", c.code, c.err, got, c.want)
		}
	}
}

func TestNilMetricsIsNoop(t *testing.T) {
	var m *Metrics
	m.TrackTask("nonexistent")()
	m.Shutdown(context.Background())
}

func TestMetricsFromEnvUnset(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	m, err := MetricsFromEnv(context.Background(), "w", nil)
	if m != nil || err != nil {
		t.Fatalf("got %v, %v; want nil, nil without an endpoint", m, err)
	}
}

// A task's results.jsonl is tailed into counters and a histogram, including a
// line written after tracking began and one with no trailing newline yet.
func TestTrackTask(t *testing.T) {
	reader := metric.NewManualReader()
	m, err := newMetrics(metric.NewMeterProvider(metric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "results.jsonl")
	if err := os.WriteFile(path, []byte(`{"latency_ms":10,"status_code":200,"protocol":"http"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stop := m.TrackTask(path)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"latency_ms":20,"status_code":500,"protocol":"http"}` + "\n")
	f.WriteString("not json\n")
	f.WriteString(`{"latency_ms":30,"error":"timeout","protocol":"http"}`) // no newline yet
	time.Sleep(700 * time.Millisecond)
	f.WriteString("\n")
	f.Close()
	stop()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var requests, errs, latencyCount int64
	var running int64 = -1
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			switch d := mt.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					switch mt.Name {
					case "resonate.worker.requests":
						requests += p.Value
					case "resonate.worker.errors":
						errs += p.Value
					case "resonate.worker.tasks.running":
						running = p.Value
					}
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					latencyCount += int64(p.Count)
				}
			}
		}
	}
	if requests != 3 || errs != 2 || latencyCount != 3 || running != 0 {
		t.Errorf("requests=%d errors=%d latency samples=%d running=%d; want 3, 2, 3, 0", requests, errs, latencyCount, running)
	}
}
