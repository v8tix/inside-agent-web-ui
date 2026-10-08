// Package probe decides what the UI probes found. The procedures that find it
// (clicking, reloading, resizing in a real browser) belong to the browser
// adapter; they hand over what they measured as plain numbers and facts, and the
// verdict is a pure function of those and the thresholds, so that every rule can
// be tested without a browser.
package probe

import (
	"fmt"
	"sort"
	"strings"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

func outcome(id domain.CheckID, result domain.CheckResult, detail string, measures map[string]float64) domain.ProbeOutcome {
	return domain.ProbeOutcome{ID: id, Level: id.Level(), Result: result, Detail: detail, Measures: measures}
}

func pass(id domain.CheckID, detail string, m map[string]float64) domain.ProbeOutcome {
	return outcome(id, domain.Pass, detail, m)
}

func fail(id domain.CheckID, m map[string]float64, format string, args ...any) domain.ProbeOutcome {
	return outcome(id, domain.Fail, fmt.Sprintf(format, args...), m)
}

// NotRun is the outcome of a probe that could not be carried out. It says
// nothing about the UI.
func NotRun(id domain.CheckID, why string) domain.ProbeOutcome {
	return outcome(id, domain.NA, why, nil)
}

func flag(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// step is one thing a probe needs to have gone right: ok says it did, and what
// says what went wrong when it did not.
type step struct {
	ok   bool
	what string
}

// judgeSteps decides a probe whose steps must all hold, and in order: the first
// one that did not is the failure, and when all held, the outcome says passDetail.
func judgeSteps(id domain.CheckID, measures map[string]float64, steps []step, passDetail string) domain.ProbeOutcome {
	for _, s := range steps {
		if !s.ok {
			return fail(id, measures, "%s", s.what)
		}
	}
	return pass(id, passDetail, measures)
}

// --- P-01 Stop ---------------------------------------------------------------------

// StopMeasures is what pressing Stop in the middle of a reply showed.
type StopMeasures struct {
	// Finished is true when the reply was over before Stop could be pressed.
	Finished bool
	// NoButton is true when the page showed no Stop button for a reply that it
	// was still writing.
	NoButton bool
	// Status is the reply's status once it had ended.
	Status domain.ReplyStatus
	// StopMs is the time from the click on Stop to the reply showing as stopped,
	// on the page's clock.
	StopMs float64
	// CharsBeforeStop is how much text the reply had when Stop was pressed, and
	// KeptChars how much the stored reply has.
	CharsBeforeStop, KeptChars int
}

// JudgeStop decides P-01: Stop takes effect within the limit and the text
// written so far is kept. A reply that was over before Stop could be pressed
// says nothing of the page; a reply still being written with no Stop button to
// press is a Stop that does nothing.
func JudgeStop(m StopMeasures, th domain.Thresholds) domain.ProbeOutcome {
	measures := map[string]float64{"stop_ms": m.StopMs, "chars_before_stop": float64(m.CharsBeforeStop), "kept_chars": float64(m.KeptChars)}
	switch {
	case m.Finished:
		return NotRun(domain.P01, "the reply finished before Stop could be pressed: the agent was too fast for this probe")
	case m.NoButton:
		return fail(domain.P01, measures, "there was no Stop button while the reply was being written")
	case m.Status != domain.ReplyStopped:
		return fail(domain.P01, measures, "after Stop the reply was %q, not stopped", m.Status)
	case m.StopMs > th.StopWithinMs:
		return fail(domain.P01, measures, "Stop took %.0f ms to take effect, over the %.0f ms limit", m.StopMs, th.StopWithinMs)
	case m.CharsBeforeStop > 0 && m.KeptChars == 0:
		return fail(domain.P01, measures, "the %d characters written before Stop were not kept", m.CharsBeforeStop)
	case m.KeptChars < m.CharsBeforeStop:
		return fail(domain.P01, measures, "Stop kept %d of the %d characters that were showing", m.KeptChars, m.CharsBeforeStop)
	}
	return pass(domain.P01, fmt.Sprintf("stopped in %.0f ms, %d characters kept", m.StopMs, m.KeptChars), measures)
}

// --- P-02 Regenerate -----------------------------------------------------------------

// RegenerateMeasures is what asking for a new answer to the last message showed,
// in a conversation of two replies.
type RegenerateMeasures struct {
	RepliesBefore, RepliesAfter int
	// FirstUnchanged says the first reply kept its id and text.
	FirstUnchanged bool
	// LastReplaced says the last reply has a new id and ended.
	LastReplaced bool
	// ButtonsOffered is how many Regenerate buttons the page shows afterwards.
	ButtonsOffered int
}

// JudgeRegenerate decides P-02: regenerating replaces the last reply and only that.
func JudgeRegenerate(m RegenerateMeasures) domain.ProbeOutcome {
	measures := map[string]float64{
		"replies_before": float64(m.RepliesBefore), "replies_after": float64(m.RepliesAfter),
		"first_unchanged": flag(m.FirstUnchanged), "last_replaced": flag(m.LastReplaced), "buttons_offered": float64(m.ButtonsOffered),
	}
	switch {
	case m.RepliesAfter != m.RepliesBefore:
		return fail(domain.P02, measures, "the conversation had %d replies and has %d after regenerating: the new reply was added instead of replacing the last", m.RepliesBefore, m.RepliesAfter)
	case !m.FirstUnchanged:
		return fail(domain.P02, measures, "an earlier reply changed when the last one was regenerated")
	case !m.LastReplaced:
		return fail(domain.P02, measures, "the last reply was not replaced by a new one")
	case m.ButtonsOffered != 1:
		return fail(domain.P02, measures, "%d Regenerate buttons are offered, where only the newest reply should have one", m.ButtonsOffered)
	}
	return pass(domain.P02, "only the last reply was replaced, and only it offers Regenerate", measures)
}

// --- P-03 Lighthouse ------------------------------------------------------------------

// Lighthouse names the scores a probe reports, as "<device>_<category>".
const (
	LighthouseDesktopAccessibility = "desktop_accessibility"
	LighthouseDesktopBestPractices = "desktop_best_practices"
	LighthouseMobileAccessibility  = "mobile_accessibility"
	LighthouseMobileBestPractices  = "mobile_best_practices"
)

var lighthouseNames = []string{
	LighthouseDesktopAccessibility, LighthouseDesktopBestPractices, LighthouseMobileAccessibility, LighthouseMobileBestPractices,
}

// JudgeLighthouse decides P-03: accessibility and best practices reach the minimum,
// on desktop and on a phone. A score that is missing is not a pass.
func JudgeLighthouse(scores map[string]float64, th domain.Thresholds) domain.ProbeOutcome {
	var low, missing []string
	for _, name := range lighthouseNames {
		score, ok := scores[name]
		switch {
		case !ok:
			missing = append(missing, name)
		case score < float64(th.LighthouseMin):
			low = append(low, fmt.Sprintf("%s %.0f", strings.ReplaceAll(name, "_", " "), score))
		}
	}
	switch {
	case len(low) > 0:
		return fail(domain.P03, scores, "below %d: %s", th.LighthouseMin, strings.Join(low, ", "))
	case len(missing) > 0:
		return NotRun(domain.P03, "no score for "+strings.Join(missing, ", "))
	}
	var parts []string
	for _, name := range lighthouseNames {
		parts = append(parts, fmt.Sprintf("%s %.0f", strings.ReplaceAll(name, "_", " "), scores[name]))
	}
	return pass(domain.P03, strings.Join(parts, ", "), scores)
}

// --- P-04 Mobile ---------------------------------------------------------------------

// MobileMeasures is what the page showed in a window the size of a phone's.
type MobileMeasures struct {
	// OverflowPx is how much wider the page is than the window.
	OverflowPx float64
	// SidebarReachable says the list of conversations can be opened.
	SidebarReachable bool
	// ComposerVisible says the message field is on screen.
	ComposerVisible bool
}

// JudgeMobile decides P-04: nothing scrolls sideways, the conversations can be
// reached and the message field is on screen.
//
// Pattern: Measure, Then Judge
func JudgeMobile(m MobileMeasures) domain.ProbeOutcome {
	measures := map[string]float64{"overflow_px": m.OverflowPx, "sidebar_reachable": flag(m.SidebarReachable), "composer_visible": flag(m.ComposerVisible)}
	var problems []string
	if m.OverflowPx > 1 {
		problems = append(problems, fmt.Sprintf("the page is %.0f px wider than the window", m.OverflowPx))
	}
	if !m.SidebarReachable {
		problems = append(problems, "the conversations cannot be opened")
	}
	if !m.ComposerVisible {
		problems = append(problems, "the message field is not on screen")
	}
	if len(problems) > 0 {
		return fail(domain.P04, measures, "at 375 px: %s", strings.Join(problems, "; "))
	}
	return pass(domain.P04, "no sideways scrolling at 375 px, conversations reachable", measures)
}

// --- P-05 Rename and delete ------------------------------------------------------------

// RenameDeleteMeasures is what renaming and deleting a conversation showed, at
// once and after the page was loaded again.
type RenameDeleteMeasures struct {
	RenamedShown, RenamedAfterReload bool
	DeletedShown, DeletedAfterReload bool
}

// JudgeRenameDelete decides P-05: both work, and both are still true after a reload.
func JudgeRenameDelete(m RenameDeleteMeasures) domain.ProbeOutcome {
	measures := map[string]float64{
		"renamed_shown": flag(m.RenamedShown), "renamed_after_reload": flag(m.RenamedAfterReload),
		"deleted_shown": flag(m.DeletedShown), "deleted_after_reload": flag(m.DeletedAfterReload),
	}
	return judgeSteps(domain.P05, measures, []step{
		{m.RenamedShown, "the new title did not show after renaming"},
		{m.RenamedAfterReload, "the new title was gone after a reload"},
		{m.DeletedShown, "the conversation was still listed after deleting it"},
		{m.DeletedAfterReload, "the deleted conversation came back after a reload"},
	}, "renaming and deleting work and persist across a reload")
}

// --- P-06 Theme ----------------------------------------------------------------------

// ThemeMeasures is what the theme switch did.
type ThemeMeasures struct {
	// Toggled says the page changed theme when the switch was pressed.
	Toggled bool
	// PersistedAfterReload says the choice survived a reload.
	PersistedAfterReload bool
}

// JudgeTheme decides P-06: the switch works and the choice is kept.
func JudgeTheme(m ThemeMeasures) domain.ProbeOutcome {
	measures := map[string]float64{"toggled": flag(m.Toggled), "persisted": flag(m.PersistedAfterReload)}
	switch {
	case !m.Toggled:
		return fail(domain.P06, measures, "the page did not change theme when the switch was pressed")
	case !m.PersistedAfterReload:
		return fail(domain.P06, measures, "the theme went back to the old one after a reload")
	}
	return pass(domain.P06, "the theme switches and the choice survives a reload", measures)
}

// --- P-07 Reload in the middle of a reply -----------------------------------------------

// ReloadMeasures is what the page showed after being loaded again while a reply
// was being written.
type ReloadMeasures struct {
	UserMessages, AssistantMessages int
	// Ended says the reply reached a final status after the reload.
	Ended bool
	// CharsBefore and CharsAfter are the length of the reply's text just before
	// the reload and once it had ended.
	CharsBefore, CharsAfter int
}

// JudgeReload decides P-07: after a reload there is the partial or the final
// text, once, and the reply ends.
func JudgeReload(m ReloadMeasures) domain.ProbeOutcome {
	measures := map[string]float64{
		"user_messages": float64(m.UserMessages), "assistant_messages": float64(m.AssistantMessages), "ended": flag(m.Ended),
		"chars_before": float64(m.CharsBefore), "chars_after": float64(m.CharsAfter),
	}
	switch {
	case m.UserMessages != 1 || m.AssistantMessages != 1:
		return fail(domain.P07, measures, "after the reload the conversation has %d questions and %d replies, expected one of each", m.UserMessages, m.AssistantMessages)
	case !m.Ended:
		return fail(domain.P07, measures, "after the reload the reply never reached a final status")
	case m.CharsAfter == 0:
		return fail(domain.P07, measures, "after the reload the reply has no text")
	case m.CharsAfter < m.CharsBefore:
		return fail(domain.P07, measures, "after the reload the reply has %d characters, it had %d before", m.CharsAfter, m.CharsBefore)
	}
	return pass(domain.P07, fmt.Sprintf("one reply, %d characters, ended", m.CharsAfter), measures)
}

// --- P-08 Keyboard only -----------------------------------------------------------------

// KeyboardMeasures is what could be done with the keyboard alone.
type KeyboardMeasures struct {
	Sent, Stopped, Navigated bool
}

// JudgeKeyboard decides P-08: a message can be sent, a reply stopped and a
// conversation opened without a mouse.
func JudgeKeyboard(m KeyboardMeasures) domain.ProbeOutcome {
	measures := map[string]float64{"sent": flag(m.Sent), "stopped": flag(m.Stopped), "navigated": flag(m.Navigated)}
	var cannot []string
	if !m.Sent {
		cannot = append(cannot, "send a message")
	}
	if !m.Stopped {
		cannot = append(cannot, "stop a reply")
	}
	if !m.Navigated {
		cannot = append(cannot, "open a conversation")
	}
	if len(cannot) > 0 {
		return fail(domain.P08, measures, "with the keyboard alone it was not possible to %s", strings.Join(cannot, ", "))
	}
	return pass(domain.P08, "send, stop and navigate with the keyboard alone", measures)
}

// --- P-09 Layout shift --------------------------------------------------------------------

// JudgeLayoutShift decides P-09: the cumulative layout shift of a long
// conversation stays within the limit.
func JudgeLayoutShift(cls float64, th domain.Thresholds) domain.ProbeOutcome {
	measures := map[string]float64{"cls": cls}
	if cls > th.CLSMax {
		return fail(domain.P09, measures, "cumulative layout shift %.3f is over the %.3f limit", cls, th.CLSMax)
	}
	return pass(domain.P09, fmt.Sprintf("cumulative layout shift %.3f", cls), measures)
}

// --- P-10 Agent down ------------------------------------------------------------------------

// AgentDownMeasures is what a reply that failed, and a retry of it, showed.
type AgentDownMeasures struct {
	// FailedShown says the reply showed as failed, with a message.
	FailedShown, MessageShown bool
	RetryOffered              bool
	// RetryAnswered says that retrying got an answer once the agent was back.
	RetryAnswered bool
}

// JudgeAgentDown decides P-10: a failed reply says so and offers Retry, and
// Retry works.
func JudgeAgentDown(m AgentDownMeasures) domain.ProbeOutcome {
	measures := map[string]float64{
		"failed_shown": flag(m.FailedShown), "message_shown": flag(m.MessageShown), "retry_offered": flag(m.RetryOffered), "retry_answered": flag(m.RetryAnswered),
	}
	return judgeSteps(domain.P10, measures, []step{
		{m.FailedShown, "a reply the agent failed to give did not show as failed"},
		{m.MessageShown, "the failed reply gave the reader no message"},
		{m.RetryOffered, "the failed reply offered no Retry"},
		{m.RetryAnswered, "Retry did not get an answer once the agent was back"},
	}, "a failed reply says so and Retry gets the answer")
}

// Order is the order the probes are run in: the ones that need a calm page
// first, the audits that reload and resize last.
var Order = []domain.CheckID{
	domain.P01, domain.P02, domain.P07, domain.P05, domain.P10, domain.P06, domain.P08, domain.P09, domain.P04, domain.P03,
}

// Sorted returns outcomes in id order, for the reports.
func Sorted(outcomes []domain.ProbeOutcome) []domain.ProbeOutcome {
	out := append([]domain.ProbeOutcome(nil), outcomes...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
