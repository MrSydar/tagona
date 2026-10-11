package models

import (
	"math"
	"testing"
	"time"
)

func TestScoreBucket(t *testing.T) {
	for score, want := range map[float64]int{
		0: 0, -0.1: 0, 1e-9: 1, 0.04999: 1, 0.05: 2, 0.5: 11, 0.7: 15, 0.95: 20, 0.999999: 20, 1: 21, 1.5: 21,
	} {
		if got := ScoreBucket(score); got != want {
			t.Errorf("ScoreBucket(%v) = %d, want %d", score, got, want)
		}
	}
}

func hist(counts map[int]int64) []int64 {
	h := make([]int64, HistogramBuckets)
	for b, n := range counts {
		h[b] = n
	}
	return h
}

func near(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 1e-9 }

func TestNewTagStatPercentiles(t *testing.T) {
	// nothing evaluated: no percentiles, but a histogram of zeros
	st := NewTagStat("a", hist(nil), time.Time{})
	if st.Evaluated != 0 || st.P10 != nil || st.P50 != nil || st.P90 != nil || len(st.Histogram) != HistogramBuckets {
		t.Errorf("empty: %+v", st)
	}

	// only exact answers (grep, forced tags): the percentiles are exactly 0 and 1
	st = NewTagStat("a", hist(map[int]int64{0: 1, 21: 2}), time.Time{})
	if st.Evaluated != 3 || !near(st.P10, 0) || !near(st.P50, 1) || !near(st.P90, 1) {
		t.Errorf("yes/no: %+v p10=%v p50=%v p90=%v", st, *st.P10, *st.P50, *st.P90)
	}

	// ten scores in one bin, [0.9, 0.95): the percentiles walk through the bin
	st = NewTagStat("a", hist(map[int]int64{19: 10}), time.Time{})
	if !near(st.P10, 0.905) || !near(st.P50, 0.925) || !near(st.P90, 0.945) {
		t.Errorf("one bin: p10=%v p50=%v p90=%v", *st.P10, *st.P50, *st.P90)
	}

	// a spread: 5 at exactly 0, 5 in [0.5, 0.55), 10 at exactly 1
	st = NewTagStat("a", hist(map[int]int64{0: 5, 11: 5, 21: 10}), time.Time{})
	if st.Evaluated != 20 || !near(st.P10, 0) || !near(st.P50, 0.55) || !near(st.P90, 1) {
		t.Errorf("spread: p10=%v p50=%v p90=%v", *st.P10, *st.P50, *st.P90)
	}
}
