package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// A bad `workers` value is the caller's mistake (400); asking for more workers
// than are live is a conflict with the current state (409).
type (
	badSelection  struct{ msg string }
	notEnoughLive struct{ want, live int }
)

func (e badSelection) Error() string { return e.msg }
func (e notEnoughLive) Error() string {
	return fmt.Sprintf("requested %d workers but only %d are live", e.want, e.live)
}

// parseWorkers reads a selection: a count ("4") or a percentage ("25%").
func parseWorkers(spec string) (count int, percent float64, isPercent bool, err error) {
	bad := badSelection{fmt.Sprintf("invalid workers %q: want a count like 4 or a percentage like 25%%", spec)}
	spec = strings.TrimSpace(spec)
	if strings.HasSuffix(spec, "%") {
		p, perr := strconv.ParseFloat(strings.TrimSuffix(spec, "%"), 64)
		if perr != nil || math.IsNaN(p) || math.IsInf(p, 0) || p <= 0 || p > 100 {
			return 0, 0, true, bad
		}
		return 0, p, true, nil
	}
	n, aerr := strconv.Atoi(spec)
	if aerr != nil || n < 1 {
		return 0, 0, false, bad
	}
	return n, 0, false, nil
}

// selectWorkers picks the workers a run goes to from live (already filtered by
// label). An empty spec means all of them. A percentage rounds up, with a
// minimum of one; a count larger than the number of live workers is an error
// rather than a silently smaller (lighter) run. The pick is random: shuffle is
// rand.Shuffle in production and a fixed permutation in tests. The result is
// sorted by name so output order is stable.
func selectWorkers(live []Entry, spec string, shuffle func(n int, swap func(i, j int))) ([]Entry, error) {
	if strings.TrimSpace(spec) == "" {
		return live, nil
	}
	count, percent, isPercent, err := parseWorkers(spec)
	if err != nil {
		return nil, err
	}
	want := count
	if isPercent {
		// The small epsilon keeps float error from rounding an exact result up
		// (25% of 12 is 3, not 4).
		want = int(math.Ceil(float64(len(live))*percent/100 - 1e-9))
		if want < 1 {
			want = 1
		}
	}
	if want > len(live) {
		return nil, notEnoughLive{want: want, live: len(live)}
	}
	picked := append([]Entry(nil), live...)
	shuffle(len(picked), func(i, j int) { picked[i], picked[j] = picked[j], picked[i] })
	picked = picked[:want]
	sort.Slice(picked, func(i, j int) bool { return picked[i].Name < picked[j].Name })
	return picked, nil
}
