package synthetic_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/report"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/run"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/score"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/synthetic"
	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
)

func problems(t *testing.T) []domain.Problem {
	t.Helper()
	var out []domain.Problem
	for p, err := range synthetic.Problems().Problems(t.Context()) {
		require.NoError(t, err)
		out = append(out, p)
	}
	return out
}

func fixture(t *testing.T) *testagent.Fixture {
	t.Helper()
	f, err := testagent.ParseFixture(synthetic.Replies())
	require.NoError(t, err)
	return f
}

func TestProblems(t *testing.T) {
	all := problems(t)

	require.Len(t, all, 13)
	levels := map[int]int{}
	files := 0
	for _, p := range all {
		levels[p.Level]++
		assert.Contains(t, p.Question, "["+p.TaskID+"]", "%s: the id is in the question, which is how the scripted agent knows which problem it is", p.TaskID)
		assert.NotEmpty(t, p.Truth, p.TaskID)
		assert.False(t, p.Unscored)
		if p.FileName != "" {
			files++
		}
	}
	assert.Equal(t, map[int]int{1: 5, 2: 5, 3: 3}, levels)
	assert.Equal(t, 1, files, "one problem comes with a file, and is skipped")
}

func TestReplies(t *testing.T) {
	f := fixture(t)
	all := problems(t)

	for _, p := range all {
		if p.FileName == "" {
			assert.Contains(t, f.Replies, p.TaskID, "every problem that is asked has a scripted answer")
		} else {
			assert.NotContains(t, f.Replies, p.TaskID, "the one that is skipped is never asked")
		}
	}
	for id := range f.Replies {
		known := strings.HasPrefix(id, "probe-")
		for _, p := range all {
			known = known || p.TaskID == id
		}
		assert.True(t, known, "%s belongs to a problem or a probe", id)
	}
	assert.Contains(t, f.Replies, "probe-long", "the probes that need a long reply have one")
	assert.Equal(t, 1, f.Replies["probe-flaky"].FailFirst, "and one that fails once, for the Retry probe")

	t.Run("a copy is returned", func(t *testing.T) {
		first := synthetic.Replies()
		first[0] = 'X'

		assert.Equal(t, byte('{'), synthetic.Replies()[0])
	})
}

// The golden is checked against the data it comes from: each scripted answer is
// run through the real extraction and scorer, and the totals must be what
// selftest_expected.json says. If the data and the golden drift apart, or the
// scorer changes, this fails before a browser is involved.
func TestExpectedFollowsFromTheScriptedAnswers(t *testing.T) {
	want, err := synthetic.Expected()
	require.NoError(t, err)
	f := fixture(t)

	var rows []domain.Result
	for _, p := range problems(t) {
		row := domain.Result{RunID: "r", TaskID: p.TaskID, Level: p.Level, Attempt: 1}
		reply := f.Replies[p.TaskID]
		switch {
		case !p.Eligible(domain.AttachmentsSkip):
			row.Status = domain.StatusSkipped
		case reply.Text == "" && reply.Error != "":
			row.Status = domain.StatusAgentError
		case int64(reply.FirstDelayMs) > run.SelftestTimeout.Milliseconds():
			row.Status = domain.StatusTimeout
		default:
			answer, found := score.Extract(reply.Text)
			switch {
			case !found:
				row.Status = domain.StatusNoFinalAnswer
			case score.Question(answer, p.Truth):
				row.Status = domain.StatusCorrect
			default:
				row.Status = domain.StatusIncorrect
			}
			if reply.Tool != "" {
				row.ToolCalls = []string{reply.Tool}
			}
		}
		rows = append(rows, row)
	}

	summary := report.Aggregate(report.Input{RunID: "r", Rows: rows})

	assert.Empty(t, want.Compare(summary))
	assert.Equal(t, 8, summary.Agent.Correct, "eight correct, among them $1,000, mixed case and a ;-separated list")
}

func TestScriptedAnswersCoverTheScorerQuirks(t *testing.T) {
	f := fixture(t)
	answerOf := func(id string) string {
		a, ok := score.Extract(f.Replies[id].Text)
		require.True(t, ok, id)
		return a
	}

	assert.Equal(t, "$1,000", answerOf("syn-l1-02"), "a number written with a currency sign and a separator")
	assert.Equal(t, "paris", answerOf("syn-l1-03"), "a string in different case")
	assert.Equal(t, "Blue;Green;Red", answerOf("syn-l2-01"), "a list separated by semicolons, without spaces")
	assert.Equal(t, "1, 2, 3", answerOf("syn-l2-02"), "a list of numbers with spaces")
	_, found := score.Extract(f.Replies["syn-l2-03"].Text)
	assert.False(t, found, "a reply without the marker")
	assert.Greater(t, len(f.Replies["syn-l3-02"].Text), 1000, "a reply long enough to outgrow the window, so scrolling is checked")
	assert.Contains(t, f.Replies["syn-l3-03"].Text, "```", "a reply with a code block")
	assert.Equal(t, "lookup", f.Replies["syn-l2-05"].Tool, "a reply that uses a tool")
}

func TestProbePromptsNameScriptedReplies(t *testing.T) {
	f := fixture(t)

	assert.Contains(t, synthetic.LongProbePrompt, "[probe-long]")
	assert.Contains(t, synthetic.FlakyProbePrompt, "[probe-flaky]")
	assert.NotContains(t, synthetic.ShortProbePrompt, "[", "an ordinary prompt, answered the ordinary way")
	assert.Contains(t, f.Replies, "probe-long")
	assert.Contains(t, f.Replies, "probe-flaky")
	for _, id := range []string{"probe-long", "probe-flaky"} {
		for _, p := range problems(t) {
			assert.NotContains(t, p.Question, id, "a probe id never appears in a problem")
		}
	}
}
