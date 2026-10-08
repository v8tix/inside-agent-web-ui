package report_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/report"
)

func TestWilson(t *testing.T) {
	// Reference values to four places, computed independently by solving the
	// score-test quadratic (not by the formula the code uses).
	tests := []struct {
		name            string
		successes, n    int
		rate, low, high float64
	}{
		{"8 of 10", 8, 10, 0.8, 0.4902, 0.9433},
		{"5 of 10", 5, 10, 0.5, 0.2366, 0.7634},
		{"1 of 2", 1, 2, 0.5, 0.0945, 0.9055},
		{"50 of 100", 50, 100, 0.5, 0.4038, 0.5962},
		{"0 of 10: the lower bound is 0, the upper is not", 0, 10, 0, 0, 0.2775},
		{"10 of 10: the upper bound is 1, the lower is not", 10, 10, 1, 0.7225, 1},
		{"1 of 1", 1, 1, 1, 0.2065, 1},
		{"0 of 1", 0, 1, 0, 0, 0.7935},
		{"21 of 53 (a GAIA level 1 run)", 21, 53, 21.0 / 53, 0.2759, 0.5306},
		{"7 of 53", 7, 53, 7.0 / 53, 0.0655, 0.2484},
		{"53 of 53", 53, 53, 1, 0.9324, 1},
		{"30 of 165 (all GAIA validation)", 30, 165, 30.0 / 165, 0.1304, 0.2477},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := report.Wilson(tc.successes, tc.n)

			assert.Equal(t, tc.successes, got.Successes)
			assert.Equal(t, tc.n, got.Total)
			assert.InDelta(t, tc.rate, got.Rate, 1e-9)
			assert.InDelta(t, tc.low, got.Low, 5e-4)
			assert.InDelta(t, tc.high, got.High, 5e-4)
			assert.LessOrEqual(t, got.Low, got.Rate)
			assert.GreaterOrEqual(t, got.High, got.Rate)
		})
	}

	t.Run("no trials say nothing", func(t *testing.T) {
		got := report.Wilson(0, 0)

		assert.Zero(t, got.Total)
		assert.Zero(t, got.Rate)
		assert.Zero(t, got.High, "no made-up interval")
	})
	t.Run("out-of-range inputs are clamped", func(t *testing.T) {
		assert.Equal(t, 10, report.Wilson(15, 10).Successes)
		assert.Equal(t, 0, report.Wilson(-3, 10).Successes)
		assert.Zero(t, report.Wilson(3, -1).Total)
	})
	t.Run("more trials make a narrower interval", func(t *testing.T) {
		small, large := report.Wilson(5, 10), report.Wilson(500, 1000)

		assert.Greater(t, small.High-small.Low, large.High-large.Low)
	})
}

func TestPercentile(t *testing.T) {
	sample := []float64{15, 20, 35, 40, 50} // the textbook nearest-rank example
	tests := []struct {
		name   string
		values []float64
		p      float64
		want   float64
	}{
		{"30th", sample, 30, 20},
		{"40th", sample, 40, 20},
		{"50th", sample, 50, 35},
		{"95th", sample, 95, 50},
		{"100th", sample, 100, 50},
		{"0th is the smallest", sample, 0, 15},
		{"one value", []float64{7}, 95, 7},
		{"unsorted input", []float64{50, 15, 40, 20, 35}, 50, 35},
		{"two values: the median is the first", []float64{1, 2}, 50, 1},
		{"twenty values: the 95th is the 19th", seq(20), 95, 19},
		{"a hundred values: the 95th is the 95th", seq(100), 95, 95},
		{"empty", nil, 50, 0},
		{"not-a-number and infinity are ignored", []float64{math.NaN(), 10, math.Inf(1), 20, 30}, 50, 20},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want, report.Percentile(tc.values, tc.p), 1e-9)
		})
	}

	t.Run("it does not reorder the caller's slice", func(t *testing.T) {
		in := []float64{3, 1, 2}
		report.Percentile(in, 50)
		assert.Equal(t, []float64{3, 1, 2}, in)
	})
}

func seq(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = float64(i + 1)
	}
	return out
}

func TestMeanAndSD(t *testing.T) {
	sample := []float64{2, 4, 4, 4, 5, 5, 7, 9}

	assert.InDelta(t, 5, report.Mean(sample), 1e-12)
	assert.InDelta(t, 2.13809, report.SD(sample), 1e-5, "sample SD: sqrt(32/7)")
	assert.Zero(t, report.Mean(nil))
	assert.Zero(t, report.SD(nil))
	assert.Zero(t, report.SD([]float64{5}), "one value has no spread")
	assert.Zero(t, report.SD([]float64{3, 3, 3}))
	assert.InDelta(t, 0.1, report.SD([]float64{0.4, 0.6}), 0.05, "two repeats")
}

func TestSummarize(t *testing.T) {
	t.Run("a sample", func(t *testing.T) {
		got := report.Summarize([]float64{100, 200, 300, 400, 1000})

		assert.Equal(t, 5, got.N)
		assert.InDelta(t, 400, got.Mean, 1e-9)
		assert.InDelta(t, 300, got.P50, 1e-9)
		assert.InDelta(t, 1000, got.P95, 1e-9)
		assert.InDelta(t, 1000, got.Max, 1e-9)
	})
	t.Run("nothing", func(t *testing.T) {
		assert.Zero(t, report.Summarize(nil))
		assert.Zero(t, report.Summarize([]float64{math.NaN()}))
	})
	t.Run("values that are not numbers are left out of N", func(t *testing.T) {
		got := report.Summarize([]float64{math.NaN(), 10, math.Inf(-1), 30})

		assert.Equal(t, 2, got.N)
		assert.InDelta(t, 20, got.Mean, 1e-9)
	})
}

func TestFlips(t *testing.T) {
	tests := []struct {
		name        string
		outcomes    map[string][]bool
		wantFlipped int
		wantTasks   int
	}{
		{"nothing repeated", map[string][]bool{"a": {true}, "b": {false}}, 0, 0},
		{"always right and always wrong are stable", map[string][]bool{"a": {true, true, true}, "b": {false, false}}, 0, 2},
		{"one flip", map[string][]bool{"a": {true, false, true}, "b": {true, true}}, 1, 2},
		{"all flip", map[string][]bool{"a": {true, false}, "b": {false, true}}, 2, 2},
		{"single-attempt tasks do not count as tasks", map[string][]bool{"a": {true, false}, "b": {true}}, 1, 1},
		{"empty", nil, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flipped, tasks := report.Flips(tc.outcomes)

			require.Equal(t, tc.wantTasks, tasks)
			assert.Equal(t, tc.wantFlipped, flipped)
		})
	}
}
