// Package report turns attempts into scorecards and summaries.
package report

import (
	"math"
	"slices"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// z95 is the normal quantile for a two-sided 95% interval.
const z95 = 1.959963984540054

// Wilson returns successes out of total with its 95% Wilson score interval. The
// Wilson interval behaves at the edges (all pass, all fail, few trials), where
// the textbook normal interval gives nonsense; GAIA samples are small.
//
// With no trials there is nothing to say: the zero Proportion comes back, and
// callers check Total.
//
// Pattern: Interval, Not Percentage
func Wilson(successes, total int) domain.Proportion {
	if total <= 0 {
		return domain.Proportion{}
	}
	successes = min(max(successes, 0), total)
	n, p := float64(total), float64(successes)/float64(total)
	z2 := z95 * z95
	denominator := 1 + z2/n
	centre := (p + z2/(2*n)) / denominator
	half := z95 * math.Sqrt(p*(1-p)/n+z2/(4*n*n)) / denominator
	return domain.Proportion{
		Successes: successes,
		Total:     total,
		Rate:      p,
		Low:       max(0, centre-half),
		High:      min(1, centre+half),
	}
}

// Percentile is the nearest-rank percentile: the smallest value that at least
// p percent of the sample is at or below. It is always a value from the sample.
// p is between 0 and 100; an empty sample gives 0.
func Percentile(values []float64, p float64) float64 {
	sorted := finite(values)
	if len(sorted) == 0 {
		return 0
	}
	slices.Sort(sorted)
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[min(max(rank, 1), len(sorted))-1]
}

// Summarize describes a sample of values (times in milliseconds, usually).
// Values that are not finite numbers are left out.
func Summarize(values []float64) domain.Quantiles {
	sample := finite(values)
	if len(sample) == 0 {
		return domain.Quantiles{}
	}
	return domain.Quantiles{
		N:    len(sample),
		Mean: Mean(sample),
		P50:  Percentile(sample, 50),
		P95:  Percentile(sample, 95),
		Max:  slices.Max(sample),
	}
}

// Mean is the arithmetic mean; an empty sample gives 0.
func Mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// SD is the sample standard deviation (dividing by n-1), the right one for a
// handful of repeats of an experiment. Fewer than two values give 0.
func SD(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	mean := Mean(values)
	sum := 0.0
	for _, v := range values {
		sum += (v - mean) * (v - mean)
	}
	return math.Sqrt(sum / float64(len(values)-1))
}

// Flips counts the tasks whose outcome changed between repeats. outcomes holds,
// for each task, whether each repeat got it right. Only tasks with at least two
// repeats can flip, and only they are counted in tasks.
func Flips(outcomes map[string][]bool) (flipped, tasks int) {
	for _, repeats := range outcomes {
		if len(repeats) < 2 {
			continue
		}
		tasks++
		if slices.Contains(repeats, true) && slices.Contains(repeats, false) {
			flipped++
		}
	}
	return flipped, tasks
}

func finite(values []float64) []float64 {
	out := make([]float64, 0, len(values))
	for _, v := range values {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			out = append(out, v)
		}
	}
	return out
}
