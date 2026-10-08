package uicheck_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/uicheck"
)

// healthy is an attempt on which every check passes. Times are on the page's
// clock: Enter at 1000, first text at 1200 (TTFT 200 ms, the agent's own 150,
// so the UI added 50), 230 characters in four steps, finished at 1600.
func healthy() uicheck.Input {
	return uicheck.Input{
		Answer: "42",
		Obs: domain.UIObservations{
			KeydownAt: 1000,
			Updates:   []domain.Update{{At: 1200, Chars: 20}, {At: 1300, Chars: 80}, {At: 1400, Chars: 150}, {At: 1500, Chars: 230}},
			Statuses: []domain.StatusChange{
				{At: 1100, Status: domain.ReplyPending}, {At: 1210, Status: domain.ReplyStreaming}, {At: 1600, Status: domain.ReplyComplete},
			},
			Scrolls: []domain.ScrollSample{{At: 1200, Scrollable: true}, {At: 1300, Scrollable: true}, {At: 1400, FromBottomPx: 10, Scrollable: true}, {At: 1500, FromBottomPx: 20, Scrollable: true}},
			Console: []domain.ConsoleMessage{{Type: "log", Text: "ready"}},
			Requests: []domain.NetworkRequest{
				{Method: "GET", URL: "http://web/", Status: 200, Type: "document"},
				{Method: "GET", URL: "http://web/c/1/stream?x=secret", Status: 200, Type: "eventsource"},
			},
			Message:     domain.MessageInfo{Status: domain.ReplyComplete, TTFTMs: new(int64(150))},
			VisibleText: "The reasoning goes here. FINAL ANSWER: 42",
		},
	}
}

func find(t *testing.T, findings []uicheck.Finding, id domain.CheckID) uicheck.Finding {
	t.Helper()
	for _, f := range findings {
		if f.ID == id {
			return f
		}
	}
	require.Failf(t, "no finding", "%s", id)
	return uicheck.Finding{}
}

func TestEvaluate_HealthyAttemptPassesEverything(t *testing.T) {
	findings := uicheck.Evaluate(healthy())

	require.Len(t, findings, len(domain.PassiveChecks))
	for i, id := range domain.PassiveChecks {
		assert.Equal(t, id, findings[i].ID, "report order")
		assert.Equal(t, domain.Pass, findings[i].Result, "%s: %s", id, findings[i].Detail)
		assert.Empty(t, findings[i].Detail, "a pass needs no explanation")
	}
	assert.Empty(t, uicheck.Failed(findings))
}

func TestChecks_ConsoleTextStaysOutOfNotes(t *testing.T) {
	in := uicheck.Input{Obs: domain.UIObservations{Console: []domain.ConsoleMessage{{Type: "error", Text: "failed to render: THE-QUESTION-CANARY"}}}}

	findings := uicheck.Evaluate(in)

	for _, f := range findings {
		assert.NotContains(t, f.Detail, "CANARY", string(f.ID))
	}
}

func TestChecks(t *testing.T) {
	tests := []struct {
		name   string
		check  domain.CheckID
		mutate func(*uicheck.Input)
		want   domain.CheckResult
		detail string // part of the explanation, when there is one
	}{
		// U-01 console clean
		{"U-01 warnings, logs and issues do not count", domain.U01, func(in *uicheck.Input) {
			in.Obs.Console = []domain.ConsoleMessage{{Type: "warn"}, {Type: "log"}, {Type: "info"}, {Type: "issue"}}
		}, domain.Pass, ""},
		// A console message is the page's own text, and the page may have put
		// anything in it, the question or the reply included. Notes are in words the
		// evaluator wrote, so the message is quoted only when the run keeps text.
		{"U-01 one error", domain.U01, func(in *uicheck.Input) {
			in.IncludeText = true
			in.Obs.Console = []domain.ConsoleMessage{{Type: "log"}, {Type: "error", Text: "Refused to run the script"}}
		}, domain.Fail, "1 console error; the first: Refused to run the script"},
		{"U-01 an uncaught exception counts too", domain.U01, func(in *uicheck.Input) {
			in.IncludeText = true
			in.Obs.Console = []domain.ConsoleMessage{{Type: "exception", Text: "Uncaught TypeError"}, {Type: "error", Text: "second"}}
		}, domain.Fail, "2 console errors; the first: Uncaught TypeError"},
		{"U-01 a long message is cut", domain.U01, func(in *uicheck.Input) {
			in.IncludeText = true
			in.Obs.Console = []domain.ConsoleMessage{{Type: "error", Text: strings.Repeat("x", 500)}}
		}, domain.Fail, "…"},
		{"U-01 without --include-text only the kind and the length are said", domain.U01, func(in *uicheck.Input) {
			in.Obs.Console = []domain.ConsoleMessage{{Type: "log"}, {Type: "error", Text: "Refused to run the script"}}
		}, domain.Fail, "1 console error; the first is an error of 25 characters (its text is kept only with --include-text)"},
		{"U-01 an exception without --include-text", domain.U01, func(in *uicheck.Input) {
			in.Obs.Console = []domain.ConsoleMessage{{Type: "exception", Text: "Uncaught TypeError"}}
		}, domain.Fail, "the first is an exception of 18 characters"},
		{"U-01 nothing in the console is clean", domain.U01, func(in *uicheck.Input) { in.Obs.Console = nil }, domain.Pass, ""},

		// U-02 network clean
		{"U-02 399 is fine", domain.U02, func(in *uicheck.Input) { in.Obs.Requests[0].Status = 399 }, domain.Pass, ""},
		{"U-02 400 is not", domain.U02, func(in *uicheck.Input) { in.Obs.Requests[0].Status = 400 }, domain.Fail, "1 bad request; the first: GET / answered 400"},
		{"U-02 a failed request", domain.U02, func(in *uicheck.Input) { in.Obs.Requests[1].Failed = true }, domain.Fail, "GET /c/1/stream failed"},
		{"U-02 the query stays out of the explanation", domain.U02, func(in *uicheck.Input) { in.Obs.Requests[1].Status = 500 }, domain.Fail, "/c/1/stream answered 500"},
		{"U-02 an address that cannot be read is still reported", domain.U02, func(in *uicheck.Input) {
			in.Obs.Requests[0] = domain.NetworkRequest{Method: "GET", URL: "http://[::1", Failed: true}
		}, domain.Fail, "GET http://[::1 failed"},
		{"U-02 two bad ones", domain.U02, func(in *uicheck.Input) {
			in.Obs.Requests[0].Status, in.Obs.Requests[1].Failed = 404, true
		}, domain.Fail, "2 bad requests"},
		{"U-02 nothing recorded cannot be judged", domain.U02, func(in *uicheck.Input) { in.Obs.Requests = nil }, domain.NA, "no requests"},

		// U-03 terminal state
		{"U-03 complete", domain.U03, func(in *uicheck.Input) {}, domain.Pass, ""},
		{"U-03 failed is a final status too", domain.U03, func(in *uicheck.Input) {
			in.Obs.Statuses[2].Status, in.Obs.Message.Status = domain.ReplyFailed, domain.ReplyFailed
		}, domain.Pass, ""},
		{"U-03 stopped is a final status too", domain.U03, func(in *uicheck.Input) {
			in.Obs.Statuses[2].Status, in.Obs.Message.Status = domain.ReplyStopped, domain.ReplyStopped
		}, domain.Pass, ""},
		{"U-03 final status read from the message alone", domain.U03, func(in *uicheck.Input) { in.Obs.Statuses = nil }, domain.Pass, ""},
		{"U-03 stuck while streaming", domain.U03, func(in *uicheck.Input) {
			in.Obs.Statuses = in.Obs.Statuses[:2]
			in.Obs.Message.Status = domain.ReplyStreaming
		}, domain.Fail, `never reached a final status (last seen: "streaming")`},
		{"U-03 stuck pending", domain.U03, func(in *uicheck.Input) {
			in.Obs.Statuses = in.Obs.Statuses[:1]
			in.Obs.Message.Status = ""
		}, domain.Fail, `last seen: "pending"`},
		{"U-03 stuck, known only from the message", domain.U03, func(in *uicheck.Input) {
			in.Obs.Statuses = nil
			in.Obs.Message.Status = domain.ReplyStreaming
		}, domain.Fail, `last seen: "streaming"`},
		{"U-03 out of time and Stop worked: the agent was slow, not the UI", domain.U03, func(in *uicheck.Input) {
			in.TimedOut = true
			in.Obs.Statuses[2].Status, in.Obs.Message.Status = domain.ReplyStopped, domain.ReplyStopped
		}, domain.NA, "Stop ended it"},
		{"U-03 out of time and still not final after Stop", domain.U03, func(in *uicheck.Input) {
			in.TimedOut = true
			in.Obs.Statuses = in.Obs.Statuses[:2]
			in.Obs.Message.Status = domain.ReplyStreaming
		}, domain.Fail, `still "streaming" after Stop`},

		// U-04 incremental streaming
		{"U-04 a short answer needs no streaming", domain.U04, func(in *uicheck.Input) {
			in.Obs.Updates = []domain.Update{{At: 1200, Chars: 199}}
		}, domain.NA, "199 characters, under the 200"},
		{"U-04 200 characters in one go", domain.U04, func(in *uicheck.Input) {
			in.Obs.Updates = []domain.Update{{At: 1200, Chars: 200}}
		}, domain.Fail, "200 characters arrived in 1 update, expected at least 3"},
		{"U-04 two steps are not enough", domain.U04, func(in *uicheck.Input) {
			in.Obs.Updates = []domain.Update{{At: 1200, Chars: 100}, {At: 1300, Chars: 300}}
		}, domain.Fail, "in 2 updates"},
		{"U-04 three steps are", domain.U04, func(in *uicheck.Input) {
			in.Obs.Updates = []domain.Update{{At: 1200, Chars: 100}, {At: 1300, Chars: 200}, {At: 1400, Chars: 300}}
		}, domain.Pass, ""},
		{"U-04 updates that change nothing do not count", domain.U04, func(in *uicheck.Input) {
			in.Obs.Updates = []domain.Update{{At: 1200, Chars: 50}, {At: 1210, Chars: 50}, {At: 1220, Chars: 50}, {At: 1300, Chars: 400}}
		}, domain.Fail, "in 2 updates"},
		{"U-04 no updates at all", domain.U04, func(in *uicheck.Input) { in.Obs.Updates = nil }, domain.NA, "0 characters"},
		{"U-04 an observer that came late cannot judge", domain.U04, func(in *uicheck.Input) {
			in.Obs.ObserverLate = true
			in.Obs.Updates = []domain.Update{{At: 1500, Chars: 400}}
		}, domain.NA, "observer started after"},

		// U-05 UI overhead
		{"U-05 exactly at the limit", domain.U05, func(in *uicheck.Input) { in.Obs.Message.TTFTMs = new(int64(100)) }, domain.Pass, ""}, // 200 - 100
		{"U-05 a little over", domain.U05, func(in *uicheck.Input) { in.Obs.Message.TTFTMs = new(int64(99)) }, domain.Fail, "the UI added 101 ms, over the 100 ms limit"},
		{"U-05 the UI was faster than the agent's own count", domain.U05, func(in *uicheck.Input) { in.Obs.Message.TTFTMs = new(int64(400)) }, domain.Pass, ""},
		{"U-05 the message reports no TTFT", domain.U05, func(in *uicheck.Input) { in.Obs.Message.TTFTMs = nil }, domain.NA, "no time to first token"},
		{"U-05 no text ever appeared", domain.U05, func(in *uicheck.Input) { in.Obs.Updates = []domain.Update{{At: 1200, Chars: 0}} }, domain.NA, "no first text"},
		{"U-05 no Enter was recorded", domain.U05, func(in *uicheck.Input) { in.Obs.KeydownAt = 0 }, domain.NA, "no first text"},
		{"U-05 an observer that came late cannot judge", domain.U05, func(in *uicheck.Input) { in.Obs.ObserverLate = true }, domain.NA, "observer started after"},

		// U-06 render fidelity
		{"U-06 the answer is on the page", domain.U06, func(in *uicheck.Input) {}, domain.Pass, ""},
		{"U-06 spacing does not matter", domain.U06, func(in *uicheck.Input) {
			in.Answer, in.Obs.VisibleText = "New York City", "…FINAL ANSWER:  New\n York  City\n"
		}, domain.Pass, ""},
		{"U-06 nor does case", domain.U06, func(in *uicheck.Input) { in.Answer, in.Obs.VisibleText = "Paris", "FINAL ANSWER: PARIS" }, domain.Pass, ""},
		{"U-06 emphasis marks the page rendered away", domain.U06, func(in *uicheck.Input) {
			in.Answer, in.Obs.VisibleText = "*italic* and **bold** and `code`", "FINAL ANSWER: italic and bold and code"
		}, domain.Pass, ""},
		{"U-06 underscores in a word", domain.U06, func(in *uicheck.Input) { in.Answer, in.Obs.VisibleText = "snake_case_name", "snakecasename" }, domain.Pass, ""},
		{"U-06 digits and punctuation count", domain.U06, func(in *uicheck.Input) { in.Answer, in.Obs.VisibleText = "$1,000", "FINAL ANSWER: $1000" }, domain.Fail, "is not in the"},
		{"U-06 the page shows something else", domain.U06, func(in *uicheck.Input) { in.Obs.VisibleText = "FINAL ANSWER: 43" }, domain.Fail, "(2 characters) is not in the 16 characters"},
		{"U-06 the page shows nothing", domain.U06, func(in *uicheck.Input) { in.Obs.VisibleText = "" }, domain.Fail, "0 characters"},
		{"U-06 no final answer to look for", domain.U06, func(in *uicheck.Input) { in.Answer = "" }, domain.NA, "no final answer"},
		{"U-06 an answer of only marks has nothing to look for", domain.U06, func(in *uicheck.Input) { in.Answer = " ** _ " }, domain.NA, "no final answer"},
		{"U-06 non-Latin text", domain.U06, func(in *uicheck.Input) { in.Answer, in.Obs.VisibleText = "東京", "答え: 東京" }, domain.Pass, ""},

		// U-07 scroll follow
		{"U-07 exactly 80 px away", domain.U07, func(in *uicheck.Input) { in.Obs.Scrolls[3].FromBottomPx = 80 }, domain.Pass, ""},
		{"U-07 a little further", domain.U07, func(in *uicheck.Input) { in.Obs.Scrolls[3].FromBottomPx = 80.5 }, domain.Fail, "more than 80 px from the bottom after 1 of 4 updates (at worst 80 px)"},
		{"U-07 never followed", domain.U07, func(in *uicheck.Input) {
			in.Obs.Scrolls = []domain.ScrollSample{{At: 1200, FromBottomPx: 120, Scrollable: true}, {At: 1300, FromBottomPx: 300, Scrollable: true}, {At: 1400, FromBottomPx: 500, Scrollable: true}}
		}, domain.Fail, "after 3 of 3 updates (at worst 500 px)"},
		{"U-07 a list that fits in the window has nothing to follow", domain.U07, func(in *uicheck.Input) {
			in.Obs.Scrolls = []domain.ScrollSample{{At: 1200}, {At: 1300}, {At: 1400}}
		}, domain.NA, "nothing to follow"},
		{"U-07 one sample of a list that could scroll is enough", domain.U07, func(in *uicheck.Input) {
			in.Obs.Scrolls = []domain.ScrollSample{{At: 1200}, {At: 1300}, {At: 1400, Scrollable: true}}
		}, domain.Pass, ""},
		{"U-07 far from the bottom fails even if the list could not scroll yet", domain.U07, func(in *uicheck.Input) {
			in.Obs.Scrolls = []domain.ScrollSample{{At: 1200, FromBottomPx: 200}}
		}, domain.Fail, "1 of 1 updates"},
		{"U-07 nothing recorded", domain.U07, func(in *uicheck.Input) { in.Obs.Scrolls = nil }, domain.NA, "no scroll positions"},

		// U-08 render safety
		{"U-08 a script", domain.U08, func(in *uicheck.Input) { in.Obs.Unsafe.Scripts = 1 }, domain.Fail, "1 script(s), 0 event-handler attribute(s) and 0 external image(s)"},
		{"U-08 a handler attribute", domain.U08, func(in *uicheck.Input) { in.Obs.Unsafe.EventHandlerAttrs = 2 }, domain.Fail, "2 event-handler"},
		{"U-08 an external image", domain.U08, func(in *uicheck.Input) { in.Obs.Unsafe.ExternalImages = 3 }, domain.Fail, "3 external image"},
		{"U-08 no body to inspect", domain.U08, func(in *uicheck.Input) { in.Obs.Updates, in.Obs.VisibleText = nil, "" }, domain.NA, "no answer body"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := healthy()
			tc.mutate(&in)

			got := find(t, uicheck.Evaluate(in), tc.check)

			assert.Equal(t, tc.want, got.Result, got.Detail)
			if tc.detail == "" {
				assert.Empty(t, got.Detail)
			} else {
				assert.Contains(t, got.Detail, tc.detail)
			}
		})
	}
}

func TestEvaluate_Thresholds(t *testing.T) {
	t.Run("the defaults when none are given", func(t *testing.T) {
		in := healthy()
		in.Obs.Message.TTFTMs = new(int64(99)) // 101 ms added
		assert.Equal(t, domain.Fail, find(t, uicheck.Evaluate(in), domain.U05).Result)

		in.Thresholds = domain.DefaultThresholds()
		assert.Equal(t, domain.Fail, find(t, uicheck.Evaluate(in), domain.U05).Result, "the same as writing them out")
	})
	t.Run("settings change what is judged", func(t *testing.T) {
		in := healthy()
		in.Obs.Message.TTFTMs = new(int64(99))
		in.Obs.Scrolls[3].FromBottomPx = 200
		in.Obs.Updates = []domain.Update{{At: 1200, Chars: 100}, {At: 1300, Chars: 130}}
		in.Thresholds = domain.Thresholds{OverheadP95Ms: 150, ScrollFollowPx: 250, StreamMinChars: 100, StreamMinUpdates: 2, StopWithinMs: 1000, LighthouseMin: 90, CLSMax: 0.1}

		findings := uicheck.Evaluate(in)

		assert.Equal(t, domain.Pass, find(t, findings, domain.U05).Result, "101 ms is within 150")
		assert.Equal(t, domain.Pass, find(t, findings, domain.U07).Result, "200 px is within 250")
		assert.Equal(t, domain.Pass, find(t, findings, domain.U04).Result, "130 characters in two steps meets 100 and 2")
	})
	t.Run("stricter settings fail what the defaults pass", func(t *testing.T) {
		in := healthy()
		in.Thresholds = domain.DefaultThresholds()
		in.Thresholds.OverheadP95Ms = 10 // the healthy attempt added 50

		assert.Equal(t, domain.Fail, find(t, uicheck.Evaluate(in), domain.U05).Result)
	})
}

func TestEvaluate_NeverRepeatsAnswerTextInADetail(t *testing.T) {
	in := healthy()
	in.Answer = "ANSWER-CANARY"
	in.Obs.VisibleText = "VISIBLE-CANARY"
	in.Obs.Unsafe.Scripts = 1
	in.Obs.Console = []domain.ConsoleMessage{{Type: "error", Text: "boom"}}

	for _, f := range uicheck.Evaluate(in) {
		assert.NotContains(t, f.Detail, "CANARY", "%s", f.ID)
	}
	assert.Equal(t, domain.Fail, find(t, uicheck.Evaluate(in), domain.U06).Result, "and the canary case did fail U-06")
}

func TestEvaluate_DoesNotChangeItsInput(t *testing.T) {
	in := healthy()
	in.Obs.Console = []domain.ConsoleMessage{{Type: "error", Text: strings.Repeat("y", 300)}}
	before := healthy()
	before.Obs.Console = []domain.ConsoleMessage{{Type: "error", Text: strings.Repeat("y", 300)}}

	uicheck.Evaluate(in)

	assert.Equal(t, before, in)
}

func TestResultsAndFailed(t *testing.T) {
	in := healthy()
	in.Obs.Console = []domain.ConsoleMessage{{Type: "error", Text: "boom"}}
	in.Obs.Requests = nil
	findings := uicheck.Evaluate(in)

	results := uicheck.Results(findings)

	assert.Len(t, results, len(domain.PassiveChecks))
	assert.Equal(t, domain.Fail, results[domain.U01])
	assert.Equal(t, domain.NA, results[domain.U02])
	assert.Equal(t, domain.Pass, results[domain.U03])
	failed := uicheck.Failed(findings)
	require.Len(t, failed, 1)
	assert.Equal(t, domain.U01, failed[0].ID)
	assert.Len(t, findings, len(domain.PassiveChecks), "Failed leaves the findings alone")
}
