package run_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/run"
)

const expectedJSON = `{
  "version": 1,
  "statuses": {"correct": 2, "incorrect": 1, "timeout": 1, "skipped": 1},
  "eligible": 5, "attempted": 4, "correct": 2,
  "levels": [
    {"level": 1, "eligible": 3, "attempted": 2, "correct": 2},
    {"level": 2, "eligible": 2, "attempted": 2, "correct": 0}
  ],
  "format_compliance": {"successes": 3, "total": 3},
  "tool_calls": 1
}`

func matchingSummary() domain.Summary {
	return domain.Summary{Agent: domain.AgentScorecard{
		Eligible: 5, Attempted: 4, Correct: 2,
		Statuses: map[domain.AttemptStatus]int{domain.StatusCorrect: 2, domain.StatusIncorrect: 1, domain.StatusTimeout: 1, domain.StatusSkipped: 1, domain.StatusUIError: 0},
		Levels: []domain.LevelScore{
			{Level: 1, Eligible: 3, Attempted: 2, Correct: 2},
			{Level: 2, Eligible: 2, Attempted: 2, Correct: 0},
		},
		FormatCompliance: domain.Proportion{Successes: 3, Total: 3},
		Tokens:           domain.TokenStats{ToolCallsTotal: 1},
	}}
}

func TestParseExpected(t *testing.T) {
	t.Run("a good file", func(t *testing.T) {
		e, err := run.ParseExpected([]byte(expectedJSON))

		require.NoError(t, err)
		assert.Equal(t, 5, e.Eligible)
		assert.Equal(t, 1, e.Statuses[domain.StatusTimeout])
		assert.Len(t, e.Levels, 2)
	})
	for name, data := range map[string]string{
		"not JSON":          `{nope`,
		"another version":   `{"version":2}`,
		"no version":        `{"eligible":1}`,
		"an unknown member": `{"version":1,"statuses":{},"elegible":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := run.ParseExpected([]byte(data))

			require.Error(t, err)
		})
	}
}

func TestExpected_Compare(t *testing.T) {
	want, err := run.ParseExpected([]byte(expectedJSON))
	require.NoError(t, err)

	t.Run("a matching summary", func(t *testing.T) {
		assert.Empty(t, want.Compare(matchingSummary()))
	})
	tests := []struct {
		name   string
		mutate func(*domain.Summary)
		want   []string
	}{
		{"a status count", func(s *domain.Summary) {
			s.Agent.Statuses[domain.StatusTimeout] = 0
			s.Agent.Statuses[domain.StatusUIError] = 1
		},
			[]string{"status timeout: got 0, want 1", "status ui_error: got 1, want 0"}},
		{"a status that is expected but missing", func(s *domain.Summary) { delete(s.Agent.Statuses, domain.StatusSkipped) },
			[]string{"status skipped: got 0, want 1"}},
		{"a status nobody expected", func(s *domain.Summary) { s.Agent.Statuses["weird"] = 2 }, []string{"status weird: got 2, want 0"}},
		{"counts", func(s *domain.Summary) { s.Agent.Eligible, s.Agent.Attempted, s.Agent.Correct = 6, 5, 3 },
			[]string{"eligible: got 6, want 5", "attempted: got 5, want 4", "correct: got 3, want 2"}},
		{"a level", func(s *domain.Summary) { s.Agent.Levels[1].Correct = 1 }, []string{"level 2 correct: got 1, want 0"}},
		{"a level that is missing", func(s *domain.Summary) { s.Agent.Levels = s.Agent.Levels[:1] },
			[]string{"level 2 eligible: got 0, want 2", "level 2 attempted: got 0, want 2"}},
		{"a level nobody expected", func(s *domain.Summary) {
			s.Agent.Levels = append(s.Agent.Levels, domain.LevelScore{Level: 3, Eligible: 1})
		},
			[]string{"level 3 eligible: got 1, want 0"}},
		{"format compliance", func(s *domain.Summary) { s.Agent.FormatCompliance = domain.Proportion{Successes: 2, Total: 4} },
			[]string{"replies with a FINAL ANSWER marker: got 2, want 3", "replies that were answered: got 4, want 3"}},
		{"tool calls", func(s *domain.Summary) { s.Agent.Tokens.ToolCallsTotal = 0 }, []string{"tool calls: got 0, want 1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := matchingSummary()
			tc.mutate(&s)

			assert.ElementsMatch(t, tc.want, want.Compare(s))
		})
	}
	t.Run("timing and accuracy intervals are not compared", func(t *testing.T) {
		s := matchingSummary()
		s.Agent.LatencyMs = domain.Quantiles{N: 3, P95: 12345}
		s.Agent.AccuracyAttempted = domain.Proportion{Successes: 2, Total: 4, Rate: 0.5, Low: 0.1, High: 0.9}

		assert.Empty(t, want.Compare(s))
	})
}
