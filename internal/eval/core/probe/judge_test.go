package probe_test

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/probe"
)

var th = domain.DefaultThresholds()

func TestJudgeStop(t *testing.T) {
	good := probe.StopMeasures{Status: domain.ReplyStopped, StopMs: 120, CharsBeforeStop: 40, KeptChars: 55}
	tests := []struct {
		name   string
		mutate func(*probe.StopMeasures)
		want   domain.CheckResult
		detail string
	}{
		{"stopped quickly with the text kept", func(*probe.StopMeasures) {}, domain.Pass, "stopped in 120 ms, 55 characters kept"},
		{"exactly at the limit", func(m *probe.StopMeasures) { m.StopMs = 1000 }, domain.Pass, ""},
		{"a millisecond over", func(m *probe.StopMeasures) { m.StopMs = 1001 }, domain.Fail, "Stop took 1001 ms to take effect, over the 1000 ms limit"},
		{"the reply was not stopped", func(m *probe.StopMeasures) { m.Status = domain.ReplyComplete }, domain.Fail, `the reply was "complete", not stopped`},
		{"the text was lost", func(m *probe.StopMeasures) { m.KeptChars = 0 }, domain.Fail, "the 40 characters written before Stop were not kept"},
		{"only part of the text was kept", func(m *probe.StopMeasures) { m.KeptChars = 30 }, domain.Fail, "Stop kept 30 of the 40 characters that were showing"},
		{"stopped before any text, nothing to keep", func(m *probe.StopMeasures) { m.CharsBeforeStop, m.KeptChars = 0, 0 }, domain.Pass, ""},
		{"the reply had finished first", func(m *probe.StopMeasures) { m.Finished = true }, domain.NA, "too fast for this probe"},
		{"the page had no Stop button for a reply it was still writing", func(m *probe.StopMeasures) { *m = probe.StopMeasures{NoButton: true} }, domain.Fail, "there was no Stop button while the reply was being written"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := good
			tc.mutate(&m)

			got := probe.JudgeStop(m, th)

			assert.Equal(t, domain.P01, got.ID)
			assert.Equal(t, domain.Must, got.Level)
			assert.Equal(t, tc.want, got.Result, got.Detail)
			assert.Contains(t, got.Detail, tc.detail)
		})
	}
	t.Run("a stricter limit from the settings", func(t *testing.T) {
		strict := th
		strict.StopWithinMs = 100

		assert.Equal(t, domain.Fail, probe.JudgeStop(good, strict).Result)
	})
	t.Run("the measures are reported", func(t *testing.T) {
		assert.Equal(t, map[string]float64{"stop_ms": 120, "chars_before_stop": 40, "kept_chars": 55}, probe.JudgeStop(good, th).Measures)
	})
}

func TestJudgeRegenerate(t *testing.T) {
	good := probe.RegenerateMeasures{RepliesBefore: 2, RepliesAfter: 2, FirstUnchanged: true, LastReplaced: true, ButtonsOffered: 1}
	tests := []struct {
		name   string
		mutate func(*probe.RegenerateMeasures)
		want   domain.CheckResult
		detail string
	}{
		{"only the last reply is replaced", func(*probe.RegenerateMeasures) {}, domain.Pass, "only the last reply was replaced"},
		{"a reply was added", func(m *probe.RegenerateMeasures) { m.RepliesAfter = 3 }, domain.Fail, "the new reply was added instead of replacing the last"},
		{"a reply was lost", func(m *probe.RegenerateMeasures) { m.RepliesAfter = 1 }, domain.Fail, "had 2 replies and has 1"},
		{"an earlier reply changed", func(m *probe.RegenerateMeasures) { m.FirstUnchanged = false }, domain.Fail, "an earlier reply changed"},
		{"the last reply stayed the same", func(m *probe.RegenerateMeasures) { m.LastReplaced = false }, domain.Fail, "was not replaced"},
		{"no button", func(m *probe.RegenerateMeasures) { m.ButtonsOffered = 0 }, domain.Fail, "0 Regenerate buttons"},
		{"a button on every reply", func(m *probe.RegenerateMeasures) { m.ButtonsOffered = 2 }, domain.Fail, "2 Regenerate buttons"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := good
			tc.mutate(&m)

			got := probe.JudgeRegenerate(m)

			assert.Equal(t, domain.P02, got.ID)
			assert.Equal(t, domain.Must, got.Level)
			assert.Equal(t, tc.want, got.Result, got.Detail)
			assert.Contains(t, got.Detail, tc.detail)
		})
	}
}

func TestJudgeLighthouse(t *testing.T) {
	good := map[string]float64{
		probe.LighthouseDesktopAccessibility: 96, probe.LighthouseDesktopBestPractices: 100,
		probe.LighthouseMobileAccessibility: 96, probe.LighthouseMobileBestPractices: 100,
	}
	with := func(name string, v float64) map[string]float64 {
		m := maps.Clone(good)
		m[name] = v
		return m
	}
	tests := []struct {
		name   string
		scores map[string]float64
		want   domain.CheckResult
		detail string
	}{
		{"all high", good, domain.Pass, "desktop accessibility 96, desktop best practices 100, mobile accessibility 96, mobile best practices 100"},
		{"exactly the minimum", with(probe.LighthouseMobileAccessibility, 90), domain.Pass, ""},
		{"one below", with(probe.LighthouseMobileAccessibility, 89), domain.Fail, "below 90: mobile accessibility 89"},
		{"best practices low on desktop", with(probe.LighthouseDesktopBestPractices, 71), domain.Fail, "desktop best practices 71"},
		{"a score missing", map[string]float64{probe.LighthouseDesktopAccessibility: 96}, domain.NA, "no score for desktop_best_practices, mobile_accessibility, mobile_best_practices"},
		{"no scores at all", nil, domain.NA, "no score for"},
		{"a low score is not hidden by a missing one", map[string]float64{probe.LighthouseDesktopAccessibility: 50}, domain.Fail, "desktop accessibility 50"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := probe.JudgeLighthouse(tc.scores, th)

			assert.Equal(t, domain.P03, got.ID)
			assert.Equal(t, domain.Must, got.Level)
			assert.Equal(t, tc.want, got.Result, got.Detail)
			assert.Contains(t, got.Detail, tc.detail)
		})
	}
	t.Run("the minimum comes from the settings", func(t *testing.T) {
		strict := th
		strict.LighthouseMin = 97

		assert.Equal(t, domain.Fail, probe.JudgeLighthouse(good, strict).Result)
	})
}

func TestJudgeMobile(t *testing.T) {
	good := probe.MobileMeasures{OverflowPx: 0, SidebarReachable: true, ComposerVisible: true}
	tests := []struct {
		name   string
		mutate func(*probe.MobileMeasures)
		want   domain.CheckResult
		detail string
	}{
		{"fits", func(*probe.MobileMeasures) {}, domain.Pass, ""},
		{"a pixel of rounding", func(m *probe.MobileMeasures) { m.OverflowPx = 1 }, domain.Pass, ""},
		{"scrolls sideways", func(m *probe.MobileMeasures) { m.OverflowPx = 24 }, domain.Fail, "24 px wider than the window"},
		{"no way to the conversations", func(m *probe.MobileMeasures) { m.SidebarReachable = false }, domain.Fail, "conversations cannot be opened"},
		{"composer off screen", func(m *probe.MobileMeasures) { m.ComposerVisible = false }, domain.Fail, "message field is not on screen"},
		{"everything wrong is said", func(m *probe.MobileMeasures) { *m = probe.MobileMeasures{OverflowPx: 10} }, domain.Fail, "10 px wider than the window; the conversations cannot be opened; the message field is not on screen"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := good
			tc.mutate(&m)

			got := probe.JudgeMobile(m)

			assert.Equal(t, domain.P04, got.ID)
			assert.Equal(t, domain.Should, got.Level)
			assert.Equal(t, tc.want, got.Result, got.Detail)
			assert.Contains(t, got.Detail, tc.detail)
		})
	}
}

func TestJudgeRenameDelete(t *testing.T) {
	good := probe.RenameDeleteMeasures{RenamedShown: true, RenamedAfterReload: true, DeletedShown: true, DeletedAfterReload: true}
	tests := []struct {
		name   string
		mutate func(*probe.RenameDeleteMeasures)
		want   domain.CheckResult
		detail string
	}{
		{"all four", func(*probe.RenameDeleteMeasures) {}, domain.Pass, "persist across a reload"},
		{"the title did not show", func(m *probe.RenameDeleteMeasures) { m.RenamedShown = false }, domain.Fail, "the new title did not show after renaming"},
		{"the title did not last", func(m *probe.RenameDeleteMeasures) { m.RenamedAfterReload = false }, domain.Fail, "the new title was gone after a reload"},
		{"still listed", func(m *probe.RenameDeleteMeasures) { m.DeletedShown = false }, domain.Fail, "still listed after deleting"},
		{"came back", func(m *probe.RenameDeleteMeasures) { m.DeletedAfterReload = false }, domain.Fail, "came back after a reload"},
		{"the first two went wrong: the first is reported", func(m *probe.RenameDeleteMeasures) {
			m.RenamedShown, m.RenamedAfterReload = false, false
		}, domain.Fail, "the new title did not show after renaming"},
		{"the middle two went wrong: the earlier is reported", func(m *probe.RenameDeleteMeasures) {
			m.RenamedAfterReload, m.DeletedShown = false, false
		}, domain.Fail, "the new title was gone after a reload"},
		{"the last two went wrong: the earlier is reported", func(m *probe.RenameDeleteMeasures) {
			m.DeletedShown, m.DeletedAfterReload = false, false
		}, domain.Fail, "still listed after deleting"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := good
			tc.mutate(&m)

			got := probe.JudgeRenameDelete(m)

			assert.Equal(t, domain.P05, got.ID)
			assert.Equal(t, domain.Should, got.Level)
			assert.Equal(t, tc.want, got.Result)
			assert.Contains(t, got.Detail, tc.detail)
		})
	}
}

func TestJudgeRenameDelete_ReportsWhatWasMeasured(t *testing.T) {
	tests := []struct {
		name string
		m    probe.RenameDeleteMeasures
		want map[string]float64
	}{
		{
			"on a pass",
			probe.RenameDeleteMeasures{RenamedShown: true, RenamedAfterReload: true, DeletedShown: true, DeletedAfterReload: true},
			map[string]float64{"renamed_shown": 1, "renamed_after_reload": 1, "deleted_shown": 1, "deleted_after_reload": 1},
		},
		{
			"on a failure",
			probe.RenameDeleteMeasures{RenamedShown: true, DeletedShown: true},
			map[string]float64{"renamed_shown": 1, "renamed_after_reload": 0, "deleted_shown": 1, "deleted_after_reload": 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, probe.JudgeRenameDelete(tc.m).Measures)
		})
	}
}

func TestJudgeTheme(t *testing.T) {
	assert.Equal(t, domain.Pass, probe.JudgeTheme(probe.ThemeMeasures{Toggled: true, PersistedAfterReload: true}).Result)
	assert.Contains(t, probe.JudgeTheme(probe.ThemeMeasures{}).Detail, "did not change theme")
	assert.Contains(t, probe.JudgeTheme(probe.ThemeMeasures{Toggled: true}).Detail, "went back to the old one")
	assert.Equal(t, domain.P06, probe.JudgeTheme(probe.ThemeMeasures{}).ID)
	assert.Equal(t, domain.Should, probe.JudgeTheme(probe.ThemeMeasures{}).Level)
}

func TestJudgeReload(t *testing.T) {
	good := probe.ReloadMeasures{UserMessages: 1, AssistantMessages: 1, Ended: true, CharsBefore: 100, CharsAfter: 140}
	tests := []struct {
		name   string
		mutate func(*probe.ReloadMeasures)
		want   domain.CheckResult
		detail string
	}{
		{"one reply that grew and ended", func(*probe.ReloadMeasures) {}, domain.Pass, "one reply, 140 characters, ended"},
		{"the same text", func(m *probe.ReloadMeasures) { m.CharsAfter = 100 }, domain.Pass, ""},
		{"a duplicate reply", func(m *probe.ReloadMeasures) { m.AssistantMessages = 2 }, domain.Fail, "1 questions and 2 replies"},
		{"the reply is gone", func(m *probe.ReloadMeasures) { m.AssistantMessages = 0 }, domain.Fail, "1 questions and 0 replies"},
		{"a duplicate question", func(m *probe.ReloadMeasures) { m.UserMessages = 2 }, domain.Fail, "2 questions and 1 replies"},
		{"never ended", func(m *probe.ReloadMeasures) { m.Ended = false }, domain.Fail, "never reached a final status"},
		{"no text", func(m *probe.ReloadMeasures) { m.CharsAfter = 0 }, domain.Fail, "has no text"},
		{"less text than before", func(m *probe.ReloadMeasures) { m.CharsAfter = 60 }, domain.Fail, "60 characters, it had 100 before"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := good
			tc.mutate(&m)

			got := probe.JudgeReload(m)

			assert.Equal(t, domain.P07, got.ID)
			assert.Equal(t, domain.Should, got.Level)
			assert.Equal(t, tc.want, got.Result, got.Detail)
			assert.Contains(t, got.Detail, tc.detail)
		})
	}
}

func TestJudgeKeyboard(t *testing.T) {
	assert.Equal(t, domain.Pass, probe.JudgeKeyboard(probe.KeyboardMeasures{Sent: true, Stopped: true, Navigated: true}).Result)
	got := probe.JudgeKeyboard(probe.KeyboardMeasures{Sent: true})
	assert.Equal(t, domain.Fail, got.Result)
	assert.Contains(t, got.Detail, "stop a reply, open a conversation")
	assert.NotContains(t, got.Detail, "send a message")
	assert.Equal(t, domain.P08, got.ID)
}

func TestJudgeLayoutShift(t *testing.T) {
	assert.Equal(t, domain.Pass, probe.JudgeLayoutShift(0.1, th).Result, "at the limit")
	assert.Equal(t, domain.Pass, probe.JudgeLayoutShift(0, th).Result)
	got := probe.JudgeLayoutShift(0.1001, th)
	assert.Equal(t, domain.Fail, got.Result)
	assert.Contains(t, got.Detail, "0.100 is over the 0.100 limit")
	assert.Equal(t, 0.1001, got.Measures["cls"])
	assert.Equal(t, domain.P09, got.ID)
	assert.Equal(t, domain.Should, got.Level)
}

func TestJudgeAgentDown(t *testing.T) {
	good := probe.AgentDownMeasures{FailedShown: true, MessageShown: true, RetryOffered: true, RetryAnswered: true}
	tests := []struct {
		name   string
		mutate func(*probe.AgentDownMeasures)
		want   domain.CheckResult
		detail string
	}{
		{"fails, says so, retries", func(*probe.AgentDownMeasures) {}, domain.Pass, "Retry gets the answer"},
		{"not shown as failed", func(m *probe.AgentDownMeasures) { m.FailedShown = false }, domain.Fail, "did not show as failed"},
		{"no message", func(m *probe.AgentDownMeasures) { m.MessageShown = false }, domain.Fail, "gave the reader no message"},
		{"no Retry", func(m *probe.AgentDownMeasures) { m.RetryOffered = false }, domain.Fail, "offered no Retry"},
		{"Retry does not work", func(m *probe.AgentDownMeasures) { m.RetryAnswered = false }, domain.Fail, "Retry did not get an answer"},
		{"the first two went wrong: the first is reported", func(m *probe.AgentDownMeasures) {
			m.FailedShown, m.MessageShown = false, false
		}, domain.Fail, "did not show as failed"},
		{"the middle two went wrong: the earlier is reported", func(m *probe.AgentDownMeasures) {
			m.MessageShown, m.RetryOffered = false, false
		}, domain.Fail, "gave the reader no message"},
		{"the last two went wrong: the earlier is reported", func(m *probe.AgentDownMeasures) {
			m.RetryOffered, m.RetryAnswered = false, false
		}, domain.Fail, "offered no Retry"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := good
			tc.mutate(&m)

			got := probe.JudgeAgentDown(m)

			assert.Equal(t, domain.P10, got.ID)
			assert.Equal(t, tc.want, got.Result)
			assert.Contains(t, got.Detail, tc.detail)
		})
	}
}

func TestJudgeAgentDown_ReportsWhatWasMeasured(t *testing.T) {
	tests := []struct {
		name string
		m    probe.AgentDownMeasures
		want map[string]float64
	}{
		{
			"on a pass",
			probe.AgentDownMeasures{FailedShown: true, MessageShown: true, RetryOffered: true, RetryAnswered: true},
			map[string]float64{"failed_shown": 1, "message_shown": 1, "retry_offered": 1, "retry_answered": 1},
		},
		{
			"on a failure",
			probe.AgentDownMeasures{FailedShown: true, MessageShown: true},
			map[string]float64{"failed_shown": 1, "message_shown": 1, "retry_offered": 0, "retry_answered": 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, probe.JudgeAgentDown(tc.m).Measures)
		})
	}
}

func TestNotRun(t *testing.T) {
	got := probe.NotRun(domain.P03, "Lighthouse is not available")

	assert.Equal(t, domain.ProbeOutcome{ID: domain.P03, Level: domain.Must, Result: domain.NA, Detail: "Lighthouse is not available"}, got)
}

func TestOrderAndSorted(t *testing.T) {
	assert.ElementsMatch(t, domain.Probes, probe.Order, "every probe has its place in the order")
	require.Len(t, probe.Order, 10)

	sorted := probe.Sorted([]domain.ProbeOutcome{{ID: domain.P09}, {ID: domain.P01}, {ID: domain.P10}})

	assert.Equal(t, []domain.CheckID{domain.P01, domain.P09, domain.P10}, []domain.CheckID{sorted[0].ID, sorted[1].ID, sorted[2].ID})
}

func TestEveryVerdictCarriesItsLevelFromTheSpec(t *testing.T) {
	// The level is not chosen by the code that judges: it is the spec's, kept in
	// one place (domain.CheckID.Level).
	for _, o := range []domain.ProbeOutcome{
		probe.JudgeStop(probe.StopMeasures{}, th), probe.JudgeRegenerate(probe.RegenerateMeasures{}), probe.JudgeLighthouse(nil, th),
		probe.JudgeMobile(probe.MobileMeasures{}), probe.JudgeRenameDelete(probe.RenameDeleteMeasures{}), probe.JudgeTheme(probe.ThemeMeasures{}),
		probe.JudgeReload(probe.ReloadMeasures{}), probe.JudgeKeyboard(probe.KeyboardMeasures{}), probe.JudgeLayoutShift(0, th), probe.JudgeAgentDown(probe.AgentDownMeasures{}),
	} {
		assert.Equal(t, o.ID.Level(), o.Level, o.ID)
	}
}
