// Package outcometable maps every way a job can end to one stored status and one sentence.
package outcometable

import (
	"context"
	"errors"
)

// ErrQuota is an example of an error that a job can end with.
var ErrQuota = errors.New("quota exceeded")

// Outcome is what is stored: a status, and for a failure the sentence the reader sees.
type Outcome struct{ Status, Reason string }

// Decide reads the rows from the top, and the first row that matches decides.
func Decide(stopped bool, err error) Outcome {
	switch {
	case stopped || errors.Is(err, context.Canceled):
		return Outcome{"stopped", ""}
	case err == nil:
		return Outcome{"complete", ""}
	case errors.Is(err, context.DeadlineExceeded):
		return Outcome{"failed", "The job took too long."}
	case errors.Is(err, ErrQuota):
		return Outcome{"failed", "The service is busy. Try again later."}
	}
	return Outcome{"failed", "Something went wrong."}
}
