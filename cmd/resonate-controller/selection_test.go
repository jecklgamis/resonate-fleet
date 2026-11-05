package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"
)

func entries(n int) []Entry {
	out := make([]Entry, n)
	for i := range out {
		out[i] = Entry{Name: fmt.Sprintf("w%02d", i)}
	}
	return out
}

// reverse is a fixed "shuffle", so a test knows which workers get picked.
func reverse(n int, swap func(i, j int)) {
	for i := 0; i < n/2; i++ {
		swap(i, n-1-i)
	}
}

func names(es []Entry) string {
	var ns []string
	for _, e := range es {
		ns = append(ns, e.Name)
	}
	return strings.Join(ns, ",")
}

func TestSelectWorkersCountsAndPercentages(t *testing.T) {
	cases := []struct {
		live int
		spec string
		want int
	}{
		{10, "", 10},
		{10, "4", 4},
		{10, "10", 10},
		{12, "25%", 3},   // exact: not rounded up to 4
		{10, "25%", 3},   // 2.5 rounds up
		{10, "1%", 1},    // minimum of one
		{3, "0.5%", 1},   // fractional percent, still at least one
		{10, "100%", 10}, // all
		{7, "50%", 4},    // 3.5 rounds up
		{30, "10%", 3},   // float error must not push this to 4
		{1, "50%", 1},
		{10, " 40% ", 4}, // surrounding spaces are fine
	}
	for _, c := range cases {
		got, err := selectWorkers(entries(c.live), c.spec, reverse)
		if err != nil || len(got) != c.want {
			t.Errorf("%d live, %q: got %d, err %v; want %d", c.live, c.spec, len(got), err, c.want)
		}
	}
}

func TestSelectWorkersPicksRandomlyButReturnsSortedByName(t *testing.T) {
	got, err := selectWorkers(entries(6), "3", reverse)
	if err != nil {
		t.Fatal(err)
	}
	// reverse puts w05,w04,w03 first; the result is then sorted for stable output.
	if names(got) != "w03,w04,w05" {
		t.Fatalf("picked %s", names(got))
	}
	// With the real shuffle, repeated picks of 1 out of 20 must not always be the
	// same worker (the chance of that is 20^-49, i.e. never).
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		one, _ := selectWorkers(entries(20), "1", rand.Shuffle)
		seen[one[0].Name] = true
	}
	if len(seen) < 2 {
		t.Fatalf("50 random picks always chose the same worker: %v", seen)
	}
}

func TestSelectWorkersDoesNotModifyItsInput(t *testing.T) {
	in := entries(5)
	before := names(in)
	selectWorkers(in, "2", reverse)
	if names(in) != before {
		t.Fatalf("input reordered: %s -> %s", before, names(in))
	}
}

func TestSelectWorkersRejections(t *testing.T) {
	for _, spec := range []string{"0", "-3", "abc", "1.5", "0%", "-5%", "101%", "%", "x%", "NaN%", "Inf%", "1e3%"} {
		_, err := selectWorkers(entries(10), spec, reverse)
		var bad badSelection
		if !errors.As(err, &bad) {
			t.Errorf("%q: err = %v, want a badSelection", spec, err)
		}
	}
	_, err := selectWorkers(entries(4), "8", reverse)
	var few notEnoughLive
	if !errors.As(err, &few) || few.want != 8 || few.live != 4 || !strings.Contains(err.Error(), "requested 8 workers but only 4 are live") {
		t.Fatalf("too many: %v", err)
	}
}

func TestRunsEndpointAppliesWorkersAfterTheLabelFilter(t *testing.T) {
	srv, h := newController(t)
	srv.Shuffle = reverse
	for i, region := range []string{"sgp1", "sgp1", "sgp1", "sgp1", "nyc1", "nyc1"} {
		w, _ := fakeWorker(t, "tok", "done")
		register(t, h, fmt.Sprintf("w%d", i), w.URL, "tok", map[string]string{"region": region})
	}
	post := func(query string) (int, map[string]any) {
		rec := call(h, "POST", "/runs"+query, "application/json", strings.NewReader(`{"scenarioUrl":"http://x/s.yaml"}`))
		var body map[string]any
		json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	// 50% of the 4 sgp1 workers is 2 (not 3, which 50% of all 6 would give).
	code, body := post("?label=region=sgp1&workers=50%25")
	if code != http.StatusAccepted || len(body["workers"].([]any)) != 2 || body["available"].(float64) != 4 {
		t.Fatalf("50%% of sgp1: %d %v", code, body)
	}
	// A count over the label-filtered pool is a 409, even though 6 workers exist.
	if code, body := post("?label=region=nyc1&workers=3"); code != http.StatusConflict || !strings.Contains(fmt.Sprint(body["error"]), "only 2 are live") {
		t.Fatalf("3 of nyc1: %d %v", code, body)
	}
	if code, _ := post("?workers=banana"); code != http.StatusBadRequest {
		t.Fatalf("invalid spec = %d, want 400", code)
	}
	// No parameter still means every matching worker.
	if code, body := post(""); code != http.StatusAccepted || len(body["workers"].([]any)) != 6 {
		t.Fatalf("all: %d %v", code, body)
	}
}
