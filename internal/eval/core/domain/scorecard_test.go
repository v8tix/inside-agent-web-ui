package domain_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

func TestUIScorecard_Clean(t *testing.T) {
	tests := []struct {
		name string
		sc   domain.UIScorecard
		want bool
	}{
		{"nothing to report", domain.UIScorecard{}, true},
		{"a required check failed", domain.UIScorecard{MustFailed: []domain.CheckID{domain.U01}}, false},
		{"a required check could not be decided", domain.UIScorecard{Undecided: []domain.CheckID{domain.U07}}, false},
		{"the page broke an attempt", domain.UIScorecard{UIErrors: map[string]int{"submit": 1}}, false},
		{"all three", domain.UIScorecard{MustFailed: []domain.CheckID{domain.U01}, Undecided: []domain.CheckID{domain.U07}, UIErrors: map[string]int{"submit": 1}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.sc.Clean())
		})
	}
}

func TestUIScorecard_UIErrorTotal(t *testing.T) {
	assert.Zero(t, domain.UIScorecard{}.UIErrorTotal())
	assert.Equal(t, 5, domain.UIScorecard{UIErrors: map[string]int{"start_chat": 2, "submit": 1, "driver_died": 2}}.UIErrorTotal())
}

func TestUIScorecard_UIErrorList(t *testing.T) {
	assert.Empty(t, domain.UIScorecard{}.UIErrorList())
	assert.Equal(t, "driver_died 2 · start_chat 2 · submit 1",
		domain.UIScorecard{UIErrors: map[string]int{"start_chat": 2, "submit": 1, "driver_died": 2}}.UIErrorList(),
		"by class name, so that a report reads the same each time")
}

func TestAttempts(t *testing.T) {
	assert.Equal(t, "0 attempts", domain.Attempts(0))
	assert.Equal(t, "1 attempt", domain.Attempts(1))
	assert.Equal(t, "2 attempts", domain.Attempts(2))
}
