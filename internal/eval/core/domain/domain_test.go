package domain_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

func TestParseAttachmentsMode(t *testing.T) {
	tests := []struct {
		in      string
		want    domain.AttachmentsMode
		wantErr bool
	}{
		{"skip", domain.AttachmentsSkip, false},
		{"include", domain.AttachmentsInclude, false},
		{"", "", true},
		{"SKIP", "", true},
		{"attach", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := domain.ParseAttachmentsMode(tc.in)

			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestProblem_Eligible(t *testing.T) {
	tests := []struct {
		name    string
		problem domain.Problem
		mode    domain.AttachmentsMode
		want    bool
	}{
		{"no file, skip", domain.Problem{}, domain.AttachmentsSkip, true},
		{"no file, include", domain.Problem{}, domain.AttachmentsInclude, true},
		{"a file, skip", domain.Problem{FileName: "data.xlsx"}, domain.AttachmentsSkip, false},
		{"a file, include", domain.Problem{FileName: "data.xlsx"}, domain.AttachmentsInclude, true},
		{"no answer to grade against", domain.Problem{Unscored: true}, domain.AttachmentsInclude, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.problem.Eligible(tc.mode))
		})
	}
}

func TestProblem_Valid(t *testing.T) {
	ok := domain.Problem{TaskID: "t1", Level: 2, Question: "q?", Truth: "42"}
	tests := []struct {
		name    string
		mutate  func(p *domain.Problem)
		wantErr string
	}{
		{"complete", func(*domain.Problem) {}, ""},
		{"unscored needs no answer", func(p *domain.Problem) { p.Truth, p.Unscored = "", true }, ""},
		{"no id", func(p *domain.Problem) { p.TaskID = "" }, "no task id"},
		{"no question", func(p *domain.Problem) { p.Question = "" }, "no question"},
		{"level too low", func(p *domain.Problem) { p.Level = 0 }, "level 0"},
		{"level too high", func(p *domain.Problem) { p.Level = 4 }, "level 4"},
		{"an answer is missing", func(p *domain.Problem) { p.Truth = "" }, "no answer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := ok
			tc.mutate(&p)

			err := p.Valid()

			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestAttemptStatus(t *testing.T) {
	tests := []struct {
		status    domain.AttemptStatus
		attempted bool
		answered  bool
		marker    bool
	}{
		{domain.StatusCorrect, true, true, true},
		{domain.StatusIncorrect, true, true, true},
		{domain.StatusNoFinalAnswer, true, true, false},
		{domain.StatusAgentError, true, false, false},
		{domain.StatusTimeout, true, false, false},
		{domain.StatusUIError, true, false, false},
		{domain.StatusSkipped, false, false, false},
		{"mystery", false, false, false},
	}
	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			assert.Equal(t, tc.attempted, tc.status.Attempted(), "attempted")
			assert.Equal(t, tc.answered, tc.status.Answered(), "answered")
			assert.Equal(t, tc.marker, tc.status.HasMarker(), "marker")
		})
	}

	t.Run("the list is complete and valid", func(t *testing.T) {
		assert.Len(t, domain.Statuses, 7)
		seen := map[domain.AttemptStatus]bool{}
		for _, s := range domain.Statuses {
			assert.True(t, s.Valid(), s)
			assert.False(t, seen[s], "%s is listed twice", s)
			seen[s] = true
		}
		assert.False(t, domain.AttemptStatus("").Valid())
	})
}

func TestReplyStatus_Terminal(t *testing.T) {
	for status, want := range map[domain.ReplyStatus]bool{
		domain.ReplyPending: false, domain.ReplyStreaming: false,
		domain.ReplyComplete: true, domain.ReplyStopped: true, domain.ReplyFailed: true,
		"": false,
	} {
		assert.Equal(t, want, status.Terminal(), string(status))
	}
}

func TestChecks(t *testing.T) {
	assert.Len(t, domain.PassiveChecks, 8, "U-01 to U-08")
	assert.Len(t, domain.Probes, 10, "P-01 to P-10")
	assert.Equal(t, "U-01", string(domain.PassiveChecks[0]))
	assert.Equal(t, "P-10", string(domain.Probes[9]))
}

func TestObservations(t *testing.T) {
	// A reply typed at t=1000, first text 250 ms later, finished at t=2500.
	obs := domain.UIObservations{
		KeydownAt: 1000,
		Updates: []domain.Update{
			{At: 1100, Chars: 0}, // the empty bubble
			{At: 1250, Chars: 5},
			{At: 1300, Chars: 5}, // same length: not a new update
			{At: 1500, Chars: 40},
			{At: 2400, Chars: 120},
		},
		Statuses: []domain.StatusChange{
			{At: 1100, Status: domain.ReplyPending},
			{At: 1200, Status: domain.ReplyStreaming},
			{At: 2500, Status: domain.ReplyComplete},
		},
		Console:  []domain.ConsoleMessage{{Type: "log"}, {Type: "error"}, {Type: "exception"}, {Type: "warn"}, {Type: "issue"}},
		Requests: []domain.NetworkRequest{{Status: 200}, {Status: 404}, {Failed: true}, {Status: 500}, {Status: 304}},
		Message:  domain.MessageInfo{TTFTMs: new(int64(180))},
	}

	t.Run("time to first token as the page showed it", func(t *testing.T) {
		got, ok := obs.TTFTUI()
		require.True(t, ok)
		assert.InDelta(t, 250, got, 0.001)
	})
	t.Run("latency until the reply was final", func(t *testing.T) {
		got, ok := obs.Latency()
		require.True(t, ok)
		assert.InDelta(t, 1500, got, 0.001)
	})
	t.Run("what the UI adds to the agent's own time", func(t *testing.T) {
		got, ok := obs.UIOverhead()
		require.True(t, ok)
		assert.InDelta(t, 70, got, 0.001, "250 ms on the page minus the 180 ms the agent reported")
	})
	t.Run("distinct updates ignore the empty one and repeats", func(t *testing.T) {
		assert.Equal(t, 3, obs.DistinctUpdates())
	})
	t.Run("the longest pause between updates", func(t *testing.T) {
		got, ok := obs.MaxGap()
		require.True(t, ok)
		assert.InDelta(t, 900, got, 0.001)
	})
	t.Run("final length", func(t *testing.T) {
		assert.Equal(t, 120, obs.FinalChars())
	})
	t.Run("console errors count errors and exceptions only", func(t *testing.T) {
		assert.Equal(t, 2, obs.ConsoleErrors())
	})
	t.Run("failed requests count failures and statuses of 400 and up", func(t *testing.T) {
		assert.Equal(t, 3, obs.FailedRequests())
	})
}

func TestObservations_Unknown(t *testing.T) {
	// Each measure says "unknown" instead of guessing when its inputs are missing.
	tests := []struct {
		name         string
		obs          domain.UIObservations
		wantTTFT     bool
		wantLatency  bool
		wantOverhead bool
		wantGap      bool
	}{
		{"nothing observed", domain.UIObservations{}, false, false, false, false},
		{
			"no keydown was seen",
			domain.UIObservations{
				Updates:  []domain.Update{{At: 5, Chars: 3}, {At: 9, Chars: 6}},
				Statuses: []domain.StatusChange{{At: 9, Status: domain.ReplyComplete}},
				Message:  domain.MessageInfo{TTFTMs: new(int64(1))},
			},
			false, false, false, true,
		},
		{
			"no text ever arrived",
			domain.UIObservations{KeydownAt: 10, Updates: []domain.Update{{At: 20, Chars: 0}}},
			false, false, false, false,
		},
		{
			"the reply never finished",
			domain.UIObservations{
				KeydownAt: 10,
				Updates:   []domain.Update{{At: 20, Chars: 4}, {At: 30, Chars: 8}},
				Statuses:  []domain.StatusChange{{At: 20, Status: domain.ReplyStreaming}},
			},
			true, false, false, true,
		},
		{
			"the message reported no time to first token",
			domain.UIObservations{KeydownAt: 10, Updates: []domain.Update{{At: 20, Chars: 4}}},
			true, false, false, false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ttft := tc.obs.TTFTUI()
			_, latency := tc.obs.Latency()
			_, overhead := tc.obs.UIOverhead()
			_, gap := tc.obs.MaxGap()

			assert.Equal(t, tc.wantTTFT, ttft, "TTFT")
			assert.Equal(t, tc.wantLatency, latency, "latency")
			assert.Equal(t, tc.wantOverhead, overhead, "overhead")
			assert.Equal(t, tc.wantGap, gap, "gap")
		})
	}

	t.Run("overhead needs both sides", func(t *testing.T) {
		obs := domain.UIObservations{KeydownAt: 1, Updates: []domain.Update{{At: 11, Chars: 1}}}
		obs.Message.TTFTMs = new(int64(4))

		got, ok := obs.UIOverhead()

		require.True(t, ok)
		assert.InDelta(t, 6, got, 0.001)
	})
}

func TestBodyFindings_Any(t *testing.T) {
	assert.False(t, domain.BodyFindings{}.Any())
	assert.True(t, domain.BodyFindings{Scripts: 1}.Any())
	assert.True(t, domain.BodyFindings{EventHandlerAttrs: 2}.Any())
	assert.True(t, domain.BodyFindings{ExternalImages: 1}.Any())
}

func sampleResult() domain.Result {
	return domain.Result{
		RunID: "20261002-101500", TaskID: "t-01", Level: 1, Attempt: 1,
		Status: domain.StatusCorrect, Score: 1,
		LatencyMs: new(int64(1500)), TTFTUIMs: new(int64(250)), TTFTAgentMs: new(int64(180)), UIOverheadMs: new(int64(70)),
		Updates: 3, MaxGapMs: new(int64(900)),
		Tokens:    domain.Tokens{Prompt: 12, Completion: 34},
		ToolCalls: []string{"search"},
		UIChecks:  map[domain.CheckID]domain.CheckResult{domain.U01: domain.Pass, domain.U05: domain.NA},
		Question:  "What is six times seven?", Truth: "42", Answer: "42",
		ConversationID: "c-1",
	}
}

func TestResult_JSON(t *testing.T) {
	t.Run("a row without text has none of it, and nothing that is unknown", func(t *testing.T) {
		row := domain.Result{
			RunID: "r", TaskID: "t-02", Level: 2, Attempt: 1, Status: domain.StatusTimeout,
			UIChecks: map[domain.CheckID]domain.CheckResult{domain.U03: domain.Fail},
		}

		out, err := json.Marshal(row, json.Deterministic(true))

		require.NoError(t, err)
		assert.JSONEq(t, `{
			"run_id":"r","task_id":"t-02","level":2,"attempt":1,"status":"timeout","score":0,
			"ui_checks":{"U-03":"fail"},"console_errors":0,"failed_requests":0
		}`, string(out))
	})

	t.Run("a full row", func(t *testing.T) {
		out, err := json.Marshal(sampleResult().WithoutText(), json.Deterministic(true))

		require.NoError(t, err)
		assert.JSONEq(t, `{
			"run_id":"20261002-101500","task_id":"t-01","level":1,"attempt":1,"status":"correct","score":1,
			"latency_ms":1500,"ttft_ui_ms":250,"ttft_agent_ms":180,"ui_overhead_ms":70,"updates":3,"max_gap_ms":900,
			"tokens":{"prompt":12,"completion":34},"tool_calls":["search"],
			"ui_checks":{"U-01":"pass","U-05":"na"},"console_errors":0,"failed_requests":0,"conversation_id":"c-1"
		}`, string(out))
	})

	t.Run("text appears only when it was asked for", func(t *testing.T) {
		with, err := json.Marshal(sampleResult())
		require.NoError(t, err)
		without, err := json.Marshal(sampleResult().WithoutText())
		require.NoError(t, err)

		assert.Contains(t, string(with), "six times seven")
		assert.NotContains(t, string(without), "six times seven")
		assert.NotContains(t, string(without), `"truth"`)
		assert.NotContains(t, string(without), `"answer"`)
	})

	t.Run("a row survives a round trip", func(t *testing.T) {
		out, err := json.Marshal(sampleResult())
		require.NoError(t, err)

		var back domain.Result
		require.NoError(t, json.Unmarshal(out, &back))

		assert.Equal(t, sampleResult(), back)
	})

	t.Run("its key", func(t *testing.T) {
		assert.Equal(t, domain.AttemptKey{TaskID: "t-01", Attempt: 1}, sampleResult().Key())
		assert.Equal(t, "t-01#1", sampleResult().Key().String())
	})
}

func TestObservations_NeverWriteAnswerText(t *testing.T) {
	obs := domain.UIObservations{VisibleText: "the secret answer", KeydownAt: 1}

	out, err := json.Marshal(obs)

	require.NoError(t, err)
	assert.NotContains(t, string(out), "secret answer")
}

func TestCheckID_Level(t *testing.T) {
	must := []domain.CheckID{domain.U01, domain.U02, domain.U03, domain.U04, domain.U05, domain.U06, domain.U07, domain.U08, domain.P01, domain.P02, domain.P03}
	should := []domain.CheckID{domain.P04, domain.P05, domain.P06, domain.P07, domain.P08, domain.P09, domain.P10}
	for _, id := range must {
		assert.Equal(t, domain.Must, id.Level(), string(id))
	}
	for _, id := range should {
		assert.Equal(t, domain.Should, id.Level(), string(id))
	}

	t.Run("every check and probe is covered", func(t *testing.T) {
		assert.ElementsMatch(t, append(domain.PassiveChecks, domain.Probes...), append(must, should...),
			"a check added without a level would silently become a Should")
	})
	t.Run("an unknown id cannot fail a run", func(t *testing.T) {
		assert.Equal(t, domain.Should, domain.CheckID("X-1").Level())
	})
}

func TestCheckID_Name(t *testing.T) {
	for _, id := range append(append([]domain.CheckID{}, domain.PassiveChecks...), domain.Probes...) {
		assert.NotEqual(t, string(id), id.Name(), "%s has a title", id)
	}
	assert.Equal(t, "Console clean", domain.U01.Name())
	assert.Equal(t, "Agent down", domain.P10.Name())
	assert.Equal(t, "X-1", domain.CheckID("X-1").Name(), "an unknown id is its own title")
}

// The defaults are the numbers of the spec. If one of them changes, it should be
// a decision, made here and in the spec, not a way to make a run pass.
func TestDefaultThresholds_AreTheSpecs(t *testing.T) {
	assert.Equal(t, domain.Thresholds{
		OverheadP95Ms:    100,
		StreamMinChars:   200,
		StreamMinUpdates: 3,
		ScrollFollowPx:   80,
		StopWithinMs:     1000,
		LighthouseMin:    90,
		CLSMax:           0.1,
	}, domain.DefaultThresholds())
}

func TestUIObservations_FinalChars(t *testing.T) {
	assert.Zero(t, domain.UIObservations{}.FinalChars())
	assert.Equal(t, 7, domain.UIObservations{Updates: []domain.Update{{At: 1, Chars: 3}, {At: 2, Chars: 7}}}.FinalChars())
}

func TestJoinChecks(t *testing.T) {
	assert.Empty(t, domain.JoinChecks(nil))
	assert.Equal(t, "U-07", domain.JoinChecks([]domain.CheckID{domain.U07}))
	assert.Equal(t, "U-01, P-02, U-07", domain.JoinChecks([]domain.CheckID{domain.U01, domain.P02, domain.U07}), "in the order given")
}
