package interval

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 5e-4 }

func TestWilson(t *testing.T) {
	tests := []struct {
		name            string
		successes, n    int
		rate, low, high float64
	}{
		{"8 of 10", 8, 10, 0.8, 0.4902, 0.9433},
		{"all right: the upper bound is 1, the lower is not", 10, 10, 1, 0.7225, 1},
		{"all wrong: the lower bound is 0, the upper is not", 0, 10, 0, 0, 0.2775},
		{"one trial says little", 1, 1, 1, 0.2065, 1},
		{"the same rate from more trials is narrower", 800, 1000, 0.8, 0.7741, 0.8236},
	}
	for _, tc := range tests {
		rate, low, high, ok := Wilson(tc.successes, tc.n)
		if !ok || !near(rate, tc.rate) || !near(low, tc.low) || !near(high, tc.high) {
			t.Errorf("%s: Wilson = %.4f [%.4f, %.4f] ok=%v, want %.4f [%.4f, %.4f]", tc.name, rate, low, high, ok, tc.rate, tc.low, tc.high)
		}
	}
}

// Each bound of a Wilson interval is a rate whose own standard error puts the observed rate z errors away.
func TestTheBoundsSolveTheScoreEquation(t *testing.T) {
	rate, low, high, _ := Wilson(8, 10)
	for _, bound := range []float64{low, high} {
		if distance, errors := math.Abs(rate-bound), z*math.Sqrt(bound*(1-bound)/10); math.Abs(distance-errors) > 1e-9 {
			t.Errorf("bound %.6f: the observed rate is %.6f away, and z standard errors are %.6f", bound, distance, errors)
		}
	}
}

func TestNoTrialsGiveNoInterval(t *testing.T) {
	if _, _, high, ok := Wilson(0, 0); ok || high != 0 {
		t.Fatalf("Wilson(0, 0) made an interval up: high=%v ok=%v", high, ok)
	}
	if got := Report(0, 0); got != "n/a" {
		t.Fatalf("Report(0, 0) = %q, want n/a", got)
	}
}

func TestReportShowsTheIntervalAndTheCounts(t *testing.T) {
	if got, want := Report(8, 10), "80.0% (95% CI 49.0 to 94.3%, 8 of 10)"; got != want {
		t.Fatalf("Report(8, 10) = %q, want %q", got, want)
	}
}
