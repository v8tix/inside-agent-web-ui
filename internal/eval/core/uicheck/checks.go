// Package uicheck judges one attempt's observations against the passive UI
// checks U-01 to U-08. The checks are pure functions: the same observations
// and thresholds always give the same findings.
package uicheck

import (
	"cmp"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// Input is everything the passive checks look at for one attempt.
type Input struct {
	Obs domain.UIObservations
	// Answer is the final answer taken from the raw reply, empty when the reply
	// had no FINAL ANSWER marker.
	Answer string
	// TimedOut is true when the attempt used up its time and Stop was pressed.
	TimedOut bool
	// Thresholds are the limits to judge by; the zero value means the defaults.
	Thresholds domain.Thresholds
	// IncludeText says the run keeps question and answer text. The page's console
	// messages are the page's own words, and may hold either, so they are quoted
	// in an explanation only then.
	IncludeText bool
}

// Finding is what one check found.
type Finding struct {
	ID     domain.CheckID
	Result domain.CheckResult
	// Detail says why, for the log and the evidence notes. It is empty on a pass
	// and never holds answer text.
	Detail string
}

// Evaluate runs the passive checks, in report order.
func Evaluate(in Input) []Finding {
	th := cmp.Or(in.Thresholds, domain.DefaultThresholds())
	return []Finding{
		consoleClean(in),
		networkClean(in),
		terminalState(in),
		incrementalStreaming(in, th),
		uiOverhead(in, th),
		renderFidelity(in),
		scrollFollow(in, th),
		renderSafety(in),
	}
}

// Results reduces findings to what a result row stores.
func Results(findings []Finding) map[domain.CheckID]domain.CheckResult {
	out := make(map[domain.CheckID]domain.CheckResult, len(findings))
	for _, f := range findings {
		out[f.ID] = f.Result
	}
	return out
}

// Failed returns the findings that failed.
func Failed(findings []Finding) []Finding {
	return slices.DeleteFunc(slices.Clone(findings), func(f Finding) bool { return f.Result != domain.Fail })
}

func pass(id domain.CheckID) Finding { return Finding{ID: id, Result: domain.Pass} }

func fail(id domain.CheckID, format string, args ...any) Finding {
	return Finding{ID: id, Result: domain.Fail, Detail: fmt.Sprintf(format, args...)}
}

func na(id domain.CheckID, why string) Finding {
	return Finding{ID: id, Result: domain.NA, Detail: why}
}

// U-01: no console errors and no uncaught exceptions.
func consoleClean(in Input) Finding {
	n := in.Obs.ConsoleErrors()
	if n == 0 {
		return pass(domain.U01)
	}
	for _, m := range in.Obs.Console {
		if m.Type != "error" && m.Type != "exception" {
			continue
		}
		if in.IncludeText {
			return fail(domain.U01, "%d console %s; the first: %s", n, plural(n, "error", "errors"), shorten(m.Text, 120))
		}
		return fail(domain.U01, "%d console %s; the first is an %s of %d characters (its text is kept only with --include-text)",
			n, plural(n, "error", "errors"), m.Type, utf8.RuneCountInString(m.Text))
	}
	return fail(domain.U01, "%d console %s", n, plural(n, "error", "errors"))
}

// U-02: no failed requests and none answered with 400 or more. Whether a
// stream the page closed itself counts as failed is for the collector to decide
// when it reads the requests; here a failed request is a failed request.
func networkClean(in Input) Finding {
	if len(in.Obs.Requests) == 0 {
		return na(domain.U02, "no requests were recorded")
	}
	n := in.Obs.FailedRequests()
	if n == 0 {
		return pass(domain.U02)
	}
	var first string
	for _, r := range in.Obs.Requests {
		if r.Failed || r.Status >= 400 {
			first = fmt.Sprintf("%s %s", r.Method, path(r.URL))
			if r.Failed {
				first += " failed"
			} else {
				first += fmt.Sprintf(" answered %d", r.Status)
			}
			break
		}
	}
	return fail(domain.U02, "%d bad %s; the first: %s", n, plural(n, "request", "requests"), first)
}

// U-03: the message reaches a final status. A reply that was still going when
// the time ran out is the agent being slow, not the UI being stuck, so it is
// not judged (na) as long as Stop then worked; what fails is a message that
// never reaches a final status.
//
// Pattern: Third Result
func terminalState(in Input) Finding {
	reached := in.Obs.Message.Status.Terminal() || slices.ContainsFunc(in.Obs.Statuses, func(s domain.StatusChange) bool { return s.Status.Terminal() })
	switch {
	case reached && in.TimedOut:
		return na(domain.U03, "the reply was still going when time ran out; Stop ended it")
	case reached:
		return pass(domain.U03)
	case in.TimedOut:
		return fail(domain.U03, "the message was still %q after Stop", lastStatus(in.Obs))
	default:
		return fail(domain.U03, "the message never reached a final status (last seen: %q)", lastStatus(in.Obs))
	}
}

func lastStatus(o domain.UIObservations) domain.ReplyStatus {
	if n := len(o.Statuses); n > 0 {
		return o.Statuses[n-1].Status
	}
	return o.Message.Status
}

// U-04: a long answer arrives in pieces, not all at once.
func incrementalStreaming(in Input, th domain.Thresholds) Finding {
	chars := in.Obs.FinalChars()
	switch {
	case in.Obs.ObserverLate:
		return na(domain.U04, "the observer started after the reply had begun")
	case chars < th.StreamMinChars:
		return na(domain.U04, fmt.Sprintf("the answer has %d characters, under the %d that need streaming", chars, th.StreamMinChars))
	}
	if n := in.Obs.DistinctUpdates(); n < th.StreamMinUpdates {
		return fail(domain.U04, "an answer of %d characters arrived in %d %s, expected at least %d", chars, n, plural(n, "update", "updates"), th.StreamMinUpdates)
	}
	return pass(domain.U04)
}

// U-05: what the UI adds to the agent's time to first token stays within the
// limit. Per attempt this is a plain comparison; the verdict for a run is on
// the 95th percentile (see report.Aggregate).
func uiOverhead(in Input, th domain.Thresholds) Finding {
	if in.Obs.ObserverLate {
		return na(domain.U05, "the observer started after the reply had begun")
	}
	overhead, ok := in.Obs.UIOverhead()
	if !ok {
		return na(domain.U05, "no first text or no time to first token from the message")
	}
	if overhead > th.OverheadP95Ms {
		return fail(domain.U05, "the UI added %.0f ms, over the %.0f ms limit", overhead, th.OverheadP95Ms)
	}
	return pass(domain.U05)
}

// U-06: the final answer the model wrote is in the text the page shows. Both
// sides are compared without spaces, case, or Markdown marks, because the page
// renders Markdown and the answer is taken from the raw text.
func renderFidelity(in Input) Finding {
	want := squash(in.Answer)
	if want == "" {
		return na(domain.U06, "the reply had no final answer to look for")
	}
	if strings.Contains(squash(in.Obs.VisibleText), want) {
		return pass(domain.U06)
	}
	return fail(domain.U06, "the final answer (%d characters) is not in the %d characters of text the page shows", len([]rune(in.Answer)), len([]rune(in.Obs.VisibleText)))
}

// squash keeps what a reader would compare: letters, digits and punctuation in
// lower case, without whitespace and the marks Markdown uses for emphasis.
func squash(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsSpace(r) || strings.ContainsRune("*_~`\\", r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// U-07: while the reply grows, the view stays at the bottom. A reply that never
// outgrew the window has nothing to follow, so the check says nothing then.
func scrollFollow(in Input, th domain.Thresholds) Finding {
	if len(in.Obs.Scrolls) == 0 {
		return na(domain.U07, "no scroll positions were recorded")
	}
	var worst float64
	over, scrollable := 0, 0
	for _, s := range in.Obs.Scrolls {
		worst = max(worst, s.FromBottomPx)
		if s.Scrollable {
			scrollable++
		}
		if s.FromBottomPx > th.ScrollFollowPx {
			over++
		}
	}
	switch {
	case over > 0:
		return fail(domain.U07, "the view was more than %.0f px from the bottom after %d of %d updates (at worst %.0f px)", th.ScrollFollowPx, over, len(in.Obs.Scrolls), worst)
	case scrollable == 0:
		return na(domain.U07, "the reply never grew taller than the window, so there was nothing to follow")
	}
	return pass(domain.U07)
}

// U-08: the rendered answer holds no script, no event-handler attribute and no
// image from another host.
func renderSafety(in Input) Finding {
	f := in.Obs.Unsafe
	if !f.Any() {
		if len(in.Obs.Updates) == 0 && in.Obs.VisibleText == "" {
			return na(domain.U08, "there was no answer body to inspect")
		}
		return pass(domain.U08)
	}
	return fail(domain.U08, "the answer body has %d script(s), %d event-handler attribute(s) and %d external image(s)", f.Scripts, f.EventHandlerAttrs, f.ExternalImages)
}

// --- helpers -----------------------------------------------------------------------

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// shorten cuts text to at most n characters.
func shorten(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > n {
		return string(r[:n]) + "…"
	}
	return text
}

// path is a URL without its query, which may hold values worth keeping out of a log.
func path(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return shorten(raw, 80)
	}
	return shorten(u.Path, 80)
}
