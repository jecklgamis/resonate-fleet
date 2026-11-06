package hist

import (
	"math"
	"math/rand"
	"sort"
	"testing"
)

func TestIndexAndUpperAgree(t *testing.T) {
	for _, ms := range []float64{0, 0.005, 0.01, 0.02, 1, 2.5, 100, 4999, 60000, 1e9} {
		i := Index(ms)
		if i < 0 || i > MaxIndex {
			t.Fatalf("Index(%v) = %d out of range", ms, i)
		}
		if ms > minMs && Upper(i) < ms && i != MaxIndex {
			t.Errorf("Upper(Index(%v)) = %v is below the value", ms, Upper(i))
		}
		if i > 1 && ms > minMs && i != MaxIndex && Upper(i-1) >= ms {
			t.Errorf("%v belongs in an earlier bucket than %d", ms, i)
		}
	}
	if Index(math.NaN()) != 0 || Index(-5) != 0 {
		t.Fatal("NaN and negatives go to bucket 0")
	}
}

func TestPercentileWithinFivePercentOfExact(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	c := Counts{}
	var all []float64
	for i := 0; i < 20000; i++ {
		v := math.Exp(rng.NormFloat64()*0.8 + 2) // log-normal around 7ms
		c.Add(v)
		all = append(all, v)
	}
	sort.Float64s(all)
	for _, p := range []float64{50, 90, 95, 99} {
		exact := all[int(math.Ceil(p/100*float64(len(all))))-1]
		got := c.Percentile(p)
		if got < exact*0.999 || got > exact*1.06 {
			t.Errorf("p%v = %v, exact %v: off by more than the bucket width", p, got, exact)
		}
	}
}

func TestMergeEqualsCountingTogether(t *testing.T) {
	a, b, both := Counts{}, Counts{}, Counts{}
	for i := 0; i < 500; i++ {
		a.Add(float64(i%40) + 1)
		both.Add(float64(i%40) + 1)
		b.Add(float64(i%900) + 100)
		both.Add(float64(i%900) + 100)
	}
	a.Merge(b)
	if a.Total() != both.Total() {
		t.Fatalf("totals %d vs %d", a.Total(), both.Total())
	}
	for _, p := range []float64{50, 95, 99} {
		if a.Percentile(p) != both.Percentile(p) {
			t.Errorf("p%v differs after merge", p)
		}
	}
}

func TestEmptyPercentileIsZero(t *testing.T) {
	if (Counts{}).Percentile(99) != 0 {
		t.Fatal("empty histogram should give 0")
	}
}
