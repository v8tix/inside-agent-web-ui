package run_test

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/run"
)

// --- select ------------------------------------------------------------------------

type slice []domain.Problem

func (s slice) Problems(context.Context) iter.Seq2[domain.Problem, error] {
	return func(yield func(domain.Problem, error) bool) {
		for _, p := range s {
			if !yield(p, nil) {
				return
			}
		}
	}
}

type failing struct {
	before []domain.Problem
	err    error
}

func (f failing) Problems(context.Context) iter.Seq2[domain.Problem, error] {
	return func(yield func(domain.Problem, error) bool) {
		for _, p := range f.before {
			if !yield(p, nil) {
				return
			}
		}
		yield(domain.Problem{}, f.err)
	}
}

func newProblem(id string, level int, mods ...func(*domain.Problem)) domain.Problem {
	p := domain.Problem{TaskID: id, Level: level, Question: "Q " + id + "?", Truth: "a"}
	for _, m := range mods {
		m(&p)
	}
	return p
}

func withFile(p *domain.Problem) { p.FileName = "x.png" }
func unscored(p *domain.Problem) { p.Truth, p.Unscored = "", true }

func selection(t *testing.T, src slice, opts service.Filter) (asked, skipped []string, err error) {
	t.Helper()
	for sel, e := range run.Select(t.Context(), src, opts) {
		if e != nil {
			return asked, skipped, e
		}
		if sel.Skip != "" {
			skipped = append(skipped, sel.Problem.TaskID+":"+sel.Skip)
		} else {
			asked = append(asked, sel.Problem.TaskID)
		}
	}
	return asked, skipped, nil
}

func TestSelect(t *testing.T) {
	src := slice{
		newProblem("a", 1), newProblem("b", 1, withFile), newProblem("c", 2), newProblem("d", 3), newProblem("e", 1), newProblem("f", 2, unscored), newProblem("g", 1),
	}
	skip := domain.AttachmentsSkip

	tests := []struct {
		name        string
		opts        service.Filter
		wantAsked   []string
		wantSkipped []string
	}{
		{"everything", service.Filter{Attachments: skip}, []string{"a", "c", "d", "e", "g"}, []string{"b:attachment", "f:unscored"}},
		{"a level", service.Filter{Level: 1, Attachments: skip}, []string{"a", "e", "g"}, []string{"b:attachment"}},
		{"attachments included", service.Filter{Level: 1, Attachments: domain.AttachmentsInclude}, []string{"a", "b", "e", "g"}, nil},
		{"unscored are skipped even when attachments are included", service.Filter{Attachments: domain.AttachmentsInclude}, []string{"a", "b", "c", "d", "e", "g"}, []string{"f:unscored"}},
		{"a limit counts the problems asked, not the skipped", service.Filter{Limit: 2, Attachments: skip}, []string{"a", "c"}, []string{"b:attachment"}},
		{"the limit stops the walk, so later skips are not listed", service.Filter{Limit: 1, Attachments: skip}, []string{"a"}, nil},
		{"a limit larger than the set", service.Filter{Limit: 50, Attachments: skip}, []string{"a", "c", "d", "e", "g"}, []string{"b:attachment", "f:unscored"}},
		{"task ids", service.Filter{TaskIDs: []string{"d", "a"}, Attachments: skip}, []string{"a", "d"}, nil},
		{"a task id with an attachment is skipped", service.Filter{TaskIDs: []string{"b", "c"}, Attachments: skip}, []string{"c"}, []string{"b:attachment"}},
		{"level and ids together", service.Filter{Level: 1, TaskIDs: []string{"a", "c"}, Attachments: skip}, []string{"a"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			asked, skipped, err := selection(t, src, tc.opts)

			require.NoError(t, err)
			assert.Equal(t, tc.wantAsked, asked)
			assert.Equal(t, tc.wantSkipped, skipped)
		})
	}

	t.Run("the order is the source's, every time", func(t *testing.T) {
		first, _, _ := selection(t, src, service.Filter{Attachments: skip})
		for range 3 {
			again, _, _ := selection(t, src, service.Filter{Attachments: skip})
			assert.Equal(t, first, again)
		}
	})
	t.Run("a task id that does not exist is an error", func(t *testing.T) {
		_, _, err := selection(t, src, service.Filter{TaskIDs: []string{"a", "zzz", "yyy"}, Attachments: skip})

		require.ErrorContains(t, err, "no such task id: yyy, zzz")
	})
	t.Run("a typo is still caught when the limit is reached first", func(t *testing.T) {
		// Without ids, the walk ends at the limit. With them, it goes on to the end:
		// stopping at the limit would let a mistyped id pass as "nothing more".
		_, _, err := selection(t, src, service.Filter{TaskIDs: []string{"a", "zzz"}, Limit: 1, Attachments: skip})

		require.ErrorContains(t, err, "no such task id: zzz")
	})
	t.Run("a source error ends the walk", func(t *testing.T) {
		boom := errors.New("disk on fire")
		var asked []string
		var gotErr error
		for sel, err := range run.Select(t.Context(), failing{before: slice{newProblem("a", 1)}, err: boom}, service.Filter{Attachments: skip}) {
			if err != nil {
				gotErr = err
				break
			}
			asked = append(asked, sel.Problem.TaskID)
		}

		require.ErrorIs(t, gotErr, boom)
		assert.Equal(t, []string{"a"}, asked)
	})
	t.Run("the caller can stop early", func(t *testing.T) {
		n := 0
		for _, err := range run.Select(t.Context(), src, service.Filter{Attachments: skip}) {
			require.NoError(t, err)
			if n++; n == 2 {
				break
			}
		}
		assert.Equal(t, 2, n)
	})
}
