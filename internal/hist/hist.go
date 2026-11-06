// Package hist is a small log-scale latency histogram that can be summed across
// workers. Bucket i covers latencies up to Upper(i) milliseconds, each 5% wider than
// the last, so a percentile read back from it is within about 5% of the true value.
// Counts are sparse (a map), which keeps them small on the wire.
package hist

import (
	"math"
	"sort"
)

const (
	minMs  = 0.01 // everything at or below this lands in bucket 0
	growth = 1.05
	// MaxIndex caps the buckets: minMs * growth^MaxIndex is about 3 hours.
	MaxIndex = 520
)

var logGrowth = math.Log(growth)

// Index is the bucket a latency in milliseconds belongs to.
func Index(ms float64) int {
	if !(ms > minMs) { // also catches NaN
		return 0
	}
	i := 1 + int(math.Log(ms/minMs)/logGrowth)
	if i > MaxIndex {
		return MaxIndex
	}
	return i
}

// Upper is the largest latency, in milliseconds, that bucket i holds.
func Upper(i int) float64 {
	return minMs * math.Pow(growth, float64(i))
}

// Counts maps a bucket index to how many latencies fell in it.
type Counts map[int]int64

// Clone is a copy of c that can be changed without affecting it.
func (c Counts) Clone() Counts {
	out := make(Counts, len(c))
	for i, n := range c {
		out[i] = n
	}
	return out
}

// Add counts one latency.
func (c Counts) Add(ms float64) { c[Index(ms)]++ }

// Merge adds other's counts into c.
func (c Counts) Merge(other Counts) {
	for i, n := range other {
		c[i] += n
	}
}

// Total is the number of latencies counted.
func (c Counts) Total() int64 {
	var t int64
	for _, n := range c {
		t += n
	}
	return t
}

// Percentile is the latency in milliseconds at or below which p percent (0-100) of
// the counted latencies fall, as the upper edge of its bucket. It is 0 for no data.
func (c Counts) Percentile(p float64) float64 {
	total := c.Total()
	if total == 0 {
		return 0
	}
	idx := make([]int, 0, len(c))
	for i := range c {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	rank := int64(math.Ceil(p / 100 * float64(total)))
	if rank < 1 {
		rank = 1
	}
	var seen int64
	for _, i := range idx {
		seen += c[i]
		if seen >= rank {
			return Upper(i)
		}
	}
	return Upper(idx[len(idx)-1])
}
