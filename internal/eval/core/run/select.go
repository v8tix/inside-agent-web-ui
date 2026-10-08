package run

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service"
)

// Selected is a problem in the run: asked, or recorded as skipped with the
// reason.
type Selected struct {
	Problem domain.Problem
	// Skip says why the problem is not asked; empty means it is.
	Skip string
}

// Skip reasons.
const (
	SkipAttachment = "attachment"
	SkipUnscored   = "unscored"
)

// Select walks src in its own order and yields what a run covers. Problems
// outside the level or task filter are not part of the run at all. Problems the
// agent cannot be given (an attachment it cannot see, no published answer) are
// yielded as skipped, so coverage shows them instead of hiding them. The first
// Limit others are yielded to be asked.
//
// Asking for a task id that does not exist is an error at the end, which
// catches typos instead of quietly running less than was asked for. An id that
// exists but is outside the level filter is simply not selected.
func Select(ctx context.Context, src resource.ProblemSource, filter service.Filter) iter.Seq2[Selected, error] {
	return func(yield func(Selected, error) bool) {
		wanted := newTaskIDs(filter.TaskIDs)
		asked := 0
		for problem, err := range src.Problems(ctx) {
			if err != nil {
				yield(Selected{}, err)
				return
			}
			if !wanted.admit(problem.TaskID) || (filter.Level != 0 && problem.Level != filter.Level) {
				continue
			}
			sel := Selected{Problem: problem, Skip: skipReason(problem, filter)}
			if sel.Skip == "" {
				if filter.Limit > 0 && asked >= filter.Limit {
					continue
				}
				asked++
			}
			if !yield(sel, nil) {
				return
			}
			if filter.Limit > 0 && asked >= filter.Limit && len(wanted) == 0 {
				return
			}
		}
		if unknown := wanted.unseen(); len(unknown) > 0 {
			yield(Selected{}, fmt.Errorf("no such task id: %s", strings.Join(unknown, ", ")))
		}
	}
}

// skipReason says why a problem is not asked, or "" when it is to be.
func skipReason(p domain.Problem, filter service.Filter) string {
	switch {
	case p.Unscored:
		return SkipUnscored
	case !p.Eligible(filter.Attachments):
		return SkipAttachment
	}
	return ""
}

// taskIDs are the task ids a run was asked for, and whether each has turned up
// in the source. None asked for means every problem is wanted.
type taskIDs map[string]bool

func newTaskIDs(ids []string) taskIDs {
	wanted := taskIDs{}
	for _, id := range ids {
		wanted[id] = false
	}
	return wanted
}

// admit reports whether a problem passes the task filter, and notes that its id
// exists even when the level filter then drops it.
func (t taskIDs) admit(id string) bool {
	if len(t) == 0 {
		return true
	}
	if _, asked := t[id]; !asked {
		return false
	}
	t[id] = true
	return true
}

// unseen lists, in order, the ids that were asked for and never turned up.
func (t taskIDs) unseen() []string {
	var unseen []string
	for id, seen := range t {
		if !seen {
			unseen = append(unseen, id)
		}
	}
	slices.Sort(unseen)
	return unseen
}
