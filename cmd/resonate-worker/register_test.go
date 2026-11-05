package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistrarSendsHeartbeat(t *testing.T) {
	got := make(chan map[string]any, 4)
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/workers" || r.Header.Get("Authorization") != "Bearer ct" {
			t.Errorf("bad request: %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		got <- m
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ctrl.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &Registrar{ControllerURL: ctrl.URL + "/", ControllerToken: "ct", Name: "w1", URL: "http://10.0.0.1:58080",
		WorkerToken: "wt", Labels: map[string]string{"region": "sgp1"}, Interval: 20 * time.Millisecond}
	go r.Run(ctx)

	for i := 0; i < 2; i++ { // initial registration, then at least one heartbeat
		select {
		case m := <-got:
			if m["name"] != "w1" || m["url"] != "http://10.0.0.1:58080" || m["token"] != "wt" || m["labels"].(map[string]any)["region"] != "sgp1" {
				t.Fatalf("body = %v", m)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no registration received")
		}
	}
}

func TestParseLabels(t *testing.T) {
	m, err := parseLabels(" region=sgp1, az = a ,")
	if err != nil || m["region"] != "sgp1" || m["az"] != "a" || len(m) != 2 {
		t.Fatalf("labels = %v, %v", m, err)
	}
	if _, err := parseLabels("oops"); err == nil {
		t.Fatal("accepted entry without '='")
	}
}

func TestDetectAdvertiseURL(t *testing.T) {
	u, err := detectAdvertiseURL("http://127.0.0.1:58090", ":58080")
	if err != nil || !strings.HasPrefix(u, "http://127.0.0.1:") || !strings.HasSuffix(u, ":58080") {
		t.Fatalf("url = %q, %v", u, err)
	}
	if _, err := detectAdvertiseURL("http://127.0.0.1:1", "nonsense"); err == nil {
		t.Fatal("accepted bad ADDR")
	}
}

func TestRegistrarDeregister(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	ctrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ctrl.Close()
	r := &Registrar{ControllerURL: ctrl.URL, ControllerToken: "ct", Name: "w 1"}
	if err := r.Deregister(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "DELETE" || gotPath != "/workers/w%201" || gotAuth != "Bearer ct" {
		t.Fatalf("got %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 401) }))
	defer bad.Close()
	if err := (&Registrar{ControllerURL: bad.URL, Name: "w"}).Deregister(context.Background()); err == nil {
		t.Fatal("a 401 was treated as success")
	}
}
