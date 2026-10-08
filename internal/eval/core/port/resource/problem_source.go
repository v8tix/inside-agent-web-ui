// Package resource declares what the evaluator needs from the outside world:
// where problems come from, the browser, and where results are kept.
package resource

import (
	"context"
	"iter"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// ProblemSource provides the problems to ask.
//
//go:generate mockery --name ProblemSource --filename problem_source.go
type ProblemSource interface {
	// Problems yields the problems in a stable order. A read error ends the
	// sequence with that error.
	Problems(ctx context.Context) iter.Seq2[domain.Problem, error]
}
