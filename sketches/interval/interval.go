// Package interval reports a share with its 95% Wilson score interval, so a small sample is not read as precise.
package interval

import (
	"fmt"
	"math"
)

// z is the normal quantile for a two-sided 95% interval.
const z = 1.959963984540054

// Wilson returns the rate of successes among total trials and the bounds of its interval.
// With no trials there is nothing to say, and ok is false.
func Wilson(successes, total int) (rate, low, high float64, ok bool) {
	if total <= 0 {
		return 0, 0, 0, false
	}
	successes = min(max(successes, 0), total)
	n, p := float64(total), float64(successes)/float64(total)
	centre := (p + z*z/(2*n)) / (1 + z*z/n)
	half := z * math.Sqrt(p*(1-p)/n+z*z/(4*n*n)) / (1 + z*z/n)
	return p, math.Max(0, centre-half), math.Min(1, centre+half), true
}

// Report writes a share as a report should: the rate, its interval and the counts behind it.
func Report(successes, total int) string {
	rate, low, high, ok := Wilson(successes, total)
	if !ok {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%% (95%% CI %.1f to %.1f%%, %d of %d)", rate*100, low*100, high*100, successes, total)
}
