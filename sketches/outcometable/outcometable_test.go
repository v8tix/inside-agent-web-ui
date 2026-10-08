package outcometable

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestEachEndHasOneOutcome(t *testing.T) {
	tests := []struct {
		name    string
		stopped bool
		err     error
		want    Outcome
	}{
		{"no error", false, nil, Outcome{"complete", ""}},
		{"the reader pressed Stop", true, nil, Outcome{"stopped", ""}},
		{"too slow", false, fmt.Errorf("call: %w", context.DeadlineExceeded), Outcome{"failed", "The job took too long."}},
		{"quota", false, ErrQuota, Outcome{"failed", "The service is busy. Try again later."}},
		{"anything else", false, errors.New("disk on fire"), Outcome{"failed", "Something went wrong."}},
	}
	for _, tc := range tests {
		if got := Decide(tc.stopped, tc.err); got != tc.want {
			t.Errorf("%s: Decide = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// The order is the logic: the error that a Stop causes must not be read as a failure.
func TestAStopComesBeforeTheErrorRows(t *testing.T) {
	err := fmt.Errorf("upstream: %w: %w", context.Canceled, ErrQuota)
	if got := Decide(false, err); got.Status != "stopped" {
		t.Fatalf("a cancelled job was stored as %+v", got)
	}
}
