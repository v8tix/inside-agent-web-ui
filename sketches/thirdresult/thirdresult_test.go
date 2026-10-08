package thirdresult

import "testing"

func TestACheckWithNothingToJudgeAbstains(t *testing.T) {
	tests := []struct {
		name               string
		rows               int
		ageHours, maxHours float64
		want               Result
	}{
		{"fresh rows", 120, 2, 24, Pass},
		{"exactly at the limit", 120, 24, 24, Pass},
		{"stale rows", 120, 30, 24, Fail},
		{"an empty table: no row to judge", 0, 0, 24, NA},
	}
	for _, tc := range tests {
		if got := Fresh(tc.rows, tc.ageHours, tc.maxHours); got != tc.want {
			t.Errorf("%s: Fresh = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The trap: a rule such as "nothing failed" is also true of a check that nothing could decide.
func TestNotApplicableIsNeverAPass(t *testing.T) {
	tests := []struct {
		name    string
		results []Result
		want    Result
	}{
		{"every result is a pass", []Result{Pass, Pass}, Pass},
		{"one pass among abstentions", []Result{NA, Pass, NA}, Pass},
		{"one fail fails, whatever else passed", []Result{Pass, Fail, NA}, Fail},
		{"nothing could be decided", []Result{NA, NA, NA}, NA},
		{"no result at all", nil, NA},
	}
	for _, tc := range tests {
		if got := Verdict(tc.results...); got != tc.want {
			t.Errorf("%s: Verdict = %q, want %q", tc.name, got, tc.want)
		}
	}
}
