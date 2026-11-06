package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// threshold is one pass/fail condition on the fleet total, like "p95<300ms".
type threshold struct {
	Expr   string
	Metric string
	Op     string
	Value  float64 // nanoseconds for latency metrics, else a plain number
	Raw    string  // the value as the user wrote it, for display
	isTime bool
}

// metrics a threshold can name. Latency metrics take durations (300ms, 1.5s);
// the rest take numbers, and the two rates also take a percent (99%).
var thresholdMetrics = map[string]bool{ // true: a latency
	"min": true, "mean": true, "p50": true, "p75": true, "p90": true, "p95": true, "p99": true, "max": true,
	"success_rate": false, "error_rate": false, "rps": false, "requests": false,
}

// parseThreshold parses "<metric><op><value>", op being one of <= >= < >.
func parseThreshold(expr string) (threshold, error) {
	s := strings.ReplaceAll(expr, " ", "")
	i := strings.IndexAny(s, "<>")
	if i <= 0 {
		return threshold{}, fmt.Errorf("invalid threshold %q: want metric<value, like p95<300ms or success_rate>=99%%", expr)
	}
	t := threshold{Expr: expr, Metric: s[:i], Op: s[i : i+1]}
	rest := s[i+1:]
	if strings.HasPrefix(rest, "=") {
		t.Op += "="
		rest = rest[1:]
	}
	t.Raw = rest
	isTime, known := thresholdMetrics[t.Metric]
	if !known {
		return threshold{}, fmt.Errorf("invalid threshold %q: unknown metric %q (want min, mean, p50, p75, p90, p95, p99, max, success_rate, error_rate, rps or requests)", expr, t.Metric)
	}
	t.isTime = isTime
	switch {
	case isTime:
		d, err := time.ParseDuration(rest)
		if err != nil {
			return threshold{}, fmt.Errorf("invalid threshold %q: %q is not a duration like 300ms or 1.5s", expr, rest)
		}
		t.Value = float64(d)
	default:
		num, pct := strings.TrimSuffix(rest, "%"), strings.HasSuffix(rest, "%")
		v, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return threshold{}, fmt.Errorf("invalid threshold %q: %q is not a number", expr, rest)
		}
		if pct {
			if t.Metric != "success_rate" && t.Metric != "error_rate" {
				return threshold{}, fmt.Errorf("invalid threshold %q: only success_rate and error_rate take a percent", expr)
			}
			v /= 100
		}
		t.Value = v
	}
	return t, nil
}

func parseThresholds(exprs []string) ([]threshold, error) {
	var out []threshold
	for _, e := range exprs {
		t, err := parseThreshold(e)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// actual reads the metric off the aggregate.
func (t threshold) actual(a *aggregate) float64 {
	l := a.Latencies
	switch t.Metric {
	case "min":
		return float64(l.Min)
	case "mean":
		return float64(l.Mean)
	case "p50":
		return float64(l.P50)
	case "p75":
		return float64(l.P75)
	case "p90":
		return float64(l.P90)
	case "p95":
		return float64(l.P95)
	case "p99":
		return float64(l.P99)
	case "max":
		return float64(l.Max)
	case "success_rate":
		return a.SuccessRate
	case "error_rate":
		if a.Requests == 0 {
			return 0
		}
		return 1 - a.SuccessRate
	case "rps":
		return a.RequestsPerSec
	default: // requests
		return float64(a.Requests)
	}
}

func (t threshold) holds(got float64) bool {
	switch t.Op {
	case "<":
		return got < t.Value
	case "<=":
		return got <= t.Value
	case ">":
		return got > t.Value
	default:
		return got >= t.Value
	}
}

func (t threshold) show(v float64) string {
	switch {
	case t.isTime:
		return time.Duration(v).Round(10 * time.Microsecond).String()
	case t.Metric == "success_rate" || t.Metric == "error_rate":
		return fmt.Sprintf("%.2f%%", v*100)
	default:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
}

// thresholdResult is one evaluated threshold.
type thresholdResult struct {
	Threshold threshold
	Actual    float64
	Pass      bool
}

// evalThresholds checks every threshold against the fleet total. With no
// aggregate (no worker produced a report) every threshold fails.
func evalThresholds(ts []threshold, a *aggregate) []thresholdResult {
	var out []thresholdResult
	for _, t := range ts {
		r := thresholdResult{Threshold: t}
		if a != nil {
			r.Actual = t.actual(a)
			r.Pass = t.holds(r.Actual)
		}
		out = append(out, r)
	}
	return out
}

func printThresholds(w io.Writer, rs []thresholdResult) {
	if len(rs) == 0 {
		return
	}
	fmt.Fprintln(w, "\nTHRESHOLDS")
	for _, r := range rs {
		mark, got := "PASS", r.Threshold.show(r.Actual)
		if !r.Pass {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "  %s  %s%s%s  (got %s)\n", mark, r.Threshold.Metric, r.Threshold.Op, r.Threshold.Raw, got)
	}
}

func thresholdsFailed(rs []thresholdResult) bool {
	for _, r := range rs {
		if !r.Pass {
			return true
		}
	}
	return false
}
