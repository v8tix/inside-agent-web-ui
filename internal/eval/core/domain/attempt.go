package domain

import (
	"fmt"
	"slices"
	"strings"
)

// AttemptStatus says how one attempt at a problem ended.
type AttemptStatus string

// Attempt statuses.
const (
	StatusCorrect       AttemptStatus = "correct"
	StatusIncorrect     AttemptStatus = "incorrect"
	StatusNoFinalAnswer AttemptStatus = "no_final_answer" // the reply had no FINAL ANSWER marker
	StatusAgentError    AttemptStatus = "agent_error"     // the page showed an error bubble
	StatusTimeout       AttemptStatus = "timeout"         // no end before the task timeout
	StatusUIError       AttemptStatus = "ui_error"        // the page broke before an answer
	StatusSkipped       AttemptStatus = "skipped"         // not asked: attachment, filter
)

// Statuses lists every status, in the order reports show them.
var Statuses = []AttemptStatus{
	StatusCorrect, StatusIncorrect, StatusNoFinalAnswer, StatusAgentError, StatusTimeout, StatusUIError, StatusSkipped,
}

// Valid reports whether s is a known status.
func (s AttemptStatus) Valid() bool { return slices.Contains(Statuses, s) }

// Attempted is true for every status but skipped: the problem was asked.
func (s AttemptStatus) Attempted() bool { return s.Valid() && s != StatusSkipped }

// Answered is true when the agent produced a reply to grade: correct, incorrect
// or no final answer.
func (s AttemptStatus) Answered() bool {
	return s == StatusCorrect || s == StatusIncorrect || s == StatusNoFinalAnswer
}

// HasMarker is true when the reply carried the FINAL ANSWER marker.
func (s AttemptStatus) HasMarker() bool { return s == StatusCorrect || s == StatusIncorrect }

// AttemptKey identifies one attempt: a problem and the n-th try at it.
type AttemptKey struct {
	TaskID  string
	Attempt int
}

func (k AttemptKey) String() string { return fmt.Sprintf("%s#%d", k.TaskID, k.Attempt) }

// ReplyStatus is the status the Web UI shows on an assistant message.
type ReplyStatus string

// Reply statuses, as the page's data-status attribute spells them.
const (
	ReplyPending   ReplyStatus = "pending"
	ReplyStreaming ReplyStatus = "streaming"
	ReplyComplete  ReplyStatus = "complete"
	ReplyStopped   ReplyStatus = "stopped"
	ReplyFailed    ReplyStatus = "failed"
)

// Terminal reports whether the reply has stopped changing.
func (s ReplyStatus) Terminal() bool {
	return s == ReplyComplete || s == ReplyStopped || s == ReplyFailed
}

// Reply is the assistant message an attempt is waiting for.
type Reply struct {
	ConversationID string
	MessageID      string
	Status         ReplyStatus
	// Error is the text of the error bubble, when the reply failed.
	Error string
}

// CheckID names a UI check (U-01 to U-08, passive) or probe (P-01 to P-10).
type CheckID string

// UI checks and probes.
const (
	U01 CheckID = "U-01" // console clean
	U02 CheckID = "U-02" // network clean
	U03 CheckID = "U-03" // terminal state
	U04 CheckID = "U-04" // incremental streaming
	U05 CheckID = "U-05" // UI overhead
	U06 CheckID = "U-06" // render fidelity
	U07 CheckID = "U-07" // scroll follow
	U08 CheckID = "U-08" // render safety

	P01 CheckID = "P-01" // stop
	P02 CheckID = "P-02" // regenerate
	P03 CheckID = "P-03" // Lighthouse
	P04 CheckID = "P-04" // mobile
	P05 CheckID = "P-05" // rename and delete
	P06 CheckID = "P-06" // theme
	P07 CheckID = "P-07" // reload mid-stream
	P08 CheckID = "P-08" // keyboard only
	P09 CheckID = "P-09" // layout shift
	P10 CheckID = "P-10" // agent down
)

// JoinChecks lists check ids as a sentence has them: "U-01, P-02".
func JoinChecks(ids []CheckID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = string(id)
	}
	return strings.Join(parts, ", ")
}

// PassiveChecks run on every attempt, in report order.
var PassiveChecks = []CheckID{U01, U02, U03, U04, U05, U06, U07, U08}

// Probes run once per run.
var Probes = []CheckID{P01, P02, P03, P04, P05, P06, P07, P08, P09, P10}

// CheckResult is what one check found.
type CheckResult string

// Check results.
const (
	Pass CheckResult = "pass"
	Fail CheckResult = "fail"
	NA   CheckResult = "na" // not applicable, or the data to decide was missing
)

// Level says how much a failing check matters.
type Level string

// Check levels. A failing Must check makes the run exit with status 2.
const (
	Must   Level = "must"
	Should Level = "should"
)

// checkInfo is what the spec says about each check and probe.
var checkInfo = map[CheckID]struct {
	Name  string
	Level Level
}{
	U01: {"Console clean", Must}, U02: {"Network clean", Must}, U03: {"Terminal state", Must},
	U04: {"Incremental streaming", Must}, U05: {"UI overhead", Must}, U06: {"Render fidelity", Must},
	U07: {"Scroll follow", Must}, U08: {"Render safety", Must},
	P01: {"Stop", Must}, P02: {"Regenerate", Must}, P03: {"Lighthouse", Must},
	P04: {"Mobile", Should}, P05: {"Rename and delete", Should}, P06: {"Theme", Should},
	P07: {"Reload mid-stream", Should}, P08: {"Keyboard only", Should}, P09: {"Layout shift", Should},
	P10: {"Agent down", Should},
}

// Level is how much a failure of the check matters. An id the spec does not
// know is a Should, so it can report but never fail a run.
func (c CheckID) Level() Level {
	if info, ok := checkInfo[c]; ok {
		return info.Level
	}
	return Should
}

// Name is a short title for reports; an unknown id is its own title.
func (c CheckID) Name() string {
	if info, ok := checkInfo[c]; ok {
		return info.Name
	}
	return string(c)
}
