// Package thirdresult gives a check three results, so that "could not decide" is never counted as a pass.
package thirdresult

import "slices"

// Result is what one check found.
type Result string

const (
	Pass Result = "pass"
	Fail Result = "fail"
	NA   Result = "na" // not applicable, or the data to decide was missing
)

// Fresh passes when the newest row of a table is at most maxHours old. An empty table has no row to judge.
func Fresh(rows int, ageHours, maxHours float64) Result {
	switch {
	case rows == 0:
		return NA
	case ageHours > maxHours:
		return Fail
	}
	return Pass
}

// Verdict joins many results: any fail fails, a pass needs one result that passed, the rest is NA.
func Verdict(results ...Result) Result {
	switch {
	case slices.Contains(results, Fail):
		return Fail
	case slices.Contains(results, Pass):
		return Pass
	}
	return NA
}
