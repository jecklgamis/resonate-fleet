package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sampleView() liveView {
	var v liveView
	json.Unmarshal([]byte(`{
	  "id":"r1","status":"running","elapsed_ms":12400,
	  "workers":[
	    {"name":"a","status":"running","requests":600,"errors":3,"rps":50.2,"p95_ms":4.8},
	    {"name":"b","status":"running","requests":584,"errors":0,"rps":48.5,"p95_ms":5.1},
	    {"name":"c","status":"queued","note":"no task yet"}],
	  "reporting":2,"requests":1184,"errors":3,"error_rate":0.0025,"rps":98.7,
	  "latency_ms":{"p50":2.1,"p95":5,"p99":9.2,"max":41}}`), &v)
	return v
}

func TestLiveLine(t *testing.T) {
	got := liveLine(sampleView())
	for _, want := range []string{"[12s]", "2/3 workers", "98.7 req/s", "1184 req", "err 0.25%", "p50 2.1ms", "p95 5ms", "p99 9.2ms"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q lacks %q", got, want)
		}
	}
	v := sampleView()
	v.Reporting = 0
	if got := liveLine(v); !strings.Contains(got, "waiting for the workers to start") {
		t.Errorf("no stats yet: %q", got)
	}
}

func TestLiveTable(t *testing.T) {
	lines := liveTable(sampleView())
	text := strings.Join(lines, "\n")
	for _, want := range []string{"run r1  running  12s", "WORKER", "no task yet", "FLEET", "1184", "98.7"} {
		if !strings.Contains(text, want) {
			t.Errorf("table lacks %q:\n%s", want, text)
		}
	}
	if len(lines) != 2+3+1 { // header, columns, three workers, fleet
		t.Fatalf("%d lines:\n%s", len(lines), text)
	}
}

func TestDisplayRedrawsInPlaceOnATerminalAndAppendsOtherwise(t *testing.T) {
	var buf bytes.Buffer
	d := &liveDisplay{out: &buf, tty: true, table: true}
	d.frame(sampleView())
	first := buf.Len()
	if strings.Contains(buf.String(), "\x1b[") && strings.Contains(buf.String(), "A\x1b[J") {
		t.Fatal("the first frame has nothing to erase")
	}
	d.frame(sampleView())
	if !strings.Contains(buf.String()[first:], fmt.Sprintf("\x1b[%dA\x1b[J", 6)) {
		t.Fatalf("second frame should move up 6 lines and clear:\n%q", buf.String()[first:])
	}
	d.erase()
	if d.drawn != 0 {
		t.Fatal("erase should forget the frame")
	}

	buf.Reset()
	p := &liveDisplay{out: &buf, tty: false}
	p.frame(sampleView())
	p.frame(sampleView())
	if strings.Contains(buf.String(), "\x1b") || strings.Count(buf.String(), "\n") != 2 {
		t.Fatalf("plain output should be two lines with no escapes:\n%q", buf.String())
	}
}

func liveServer(t *testing.T, views ...string) (*ControllerClient, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		if i >= len(views) {
			i = len(views) - 1
		}
		w.Write([]byte(views[i]))
	}))
	t.Cleanup(srv.Close)
	return newControllerClient(srv.URL, ""), &n
}

func TestFollowStopsWhenTheRunEnds(t *testing.T) {
	c, polls := liveServer(t,
		`{"id":"r1","status":"running","reporting":1,"workers":[{"name":"a"}]}`,
		`{"id":"r1","status":"running","reporting":1,"workers":[{"name":"a"}]}`,
		`{"id":"r1","status":"done","reporting":1,"requests":42,"workers":[{"name":"a"}]}`)
	var buf bytes.Buffer
	d := &liveDisplay{out: &buf, every: 5 * time.Millisecond}
	last, err := d.follow(t.Context(), c, "r1")
	if err != nil || last.Status != "done" || last.Requests != 42 {
		t.Fatalf("last=%+v err=%v", last, err)
	}
	if polls.Load() != 3 || strings.Count(buf.String(), "\n") != 3 {
		t.Fatalf("polls=%d output=%q", polls.Load(), buf.String())
	}
}

func TestFollowWithoutLiveSupportIsAnErrorNotAHang(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler()) // an older controller has no such route
	defer srv.Close()
	d := &liveDisplay{out: &bytes.Buffer{}, every: time.Millisecond}
	done := make(chan error, 1)
	cctx, stop := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer stop()
	go func() { _, err := d.follow(cctx, newControllerClient(srv.URL, ""), "r1"); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no live view") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follow hung")
	}
}

func TestLiveUnknownRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"no such run"}`))
	}))
	defer srv.Close()
	_, err := newControllerClient(srv.URL, "").Live(t.Context(), "zzz")
	if err == nil || !strings.Contains(err.Error(), `no such run "zzz"`) {
		t.Fatalf("err = %v", err)
	}
}
