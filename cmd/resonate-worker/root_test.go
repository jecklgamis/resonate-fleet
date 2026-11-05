package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRootBanner(t *testing.T) {
	mux := http.NewServeMux()
	(&Server{}).Routes(mux)
	h := withAuth("secret", mux)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 without a token", rec.Code)
	}
	want := `{"app":"resonate-worker","message":"Relax, perf test it"}`
	if got := rec.Body.String(); got != want+"\n" && got != want {
		t.Errorf("body %q, want %q", got, want)
	}
}
