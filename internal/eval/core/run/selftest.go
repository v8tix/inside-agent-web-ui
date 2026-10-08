package run

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// ExpectedVersion is the version of the golden file format.
const ExpectedVersion = 1

// SelftestTimeout is the time the self-test gives each attempt. The scripted
// agent's slow reply is slower than this, so the attempt ends in a timeout.
const SelftestTimeout = 20 * time.Second

// Expected is what the self-test must find: the part of a summary that does not
// depend on timing. The self-test runs a set of invented problems against an
// agent whose every answer is scripted, so the statuses, the counts and the
// scores are known beforehand. If a run disagrees, the evaluator's own pipeline
// (the prompt, the answer extraction, the scorer, the driver) is wrong, not the
// agent.
//
// Pattern: Golden Self-Test
type Expected struct {
	Version   int                          `json:"version"`
	Statuses  map[domain.AttemptStatus]int `json:"statuses"`
	Eligible  int                          `json:"eligible"`
	Attempted int                          `json:"attempted"`
	Correct   int                          `json:"correct"`
	Levels    []ExpectedLevel              `json:"levels"`
	// FormatCompliance counts the replies that had a FINAL ANSWER marker, out of
	// those that were answered.
	FormatCompliance ExpectedShare `json:"format_compliance"`
	// ToolCalls is the number of tool calls the agent reports in all.
	ToolCalls int `json:"tool_calls"`
}

// ExpectedLevel is the counts for one level.
type ExpectedLevel struct {
	Level     int `json:"level"`
	Eligible  int `json:"eligible"`
	Attempted int `json:"attempted"`
	Correct   int `json:"correct"`
}

// ExpectedShare is a count out of a total.
type ExpectedShare struct {
	Successes int `json:"successes"`
	Total     int `json:"total"`
}

// ParseExpected reads a golden file. A member it does not know is an error.
func ParseExpected(data []byte) (Expected, error) {
	var e Expected
	if err := json.Unmarshal(data, &e, json.RejectUnknownMembers(true)); err != nil {
		return Expected{}, fmt.Errorf("reading the expected results: %w", err)
	}
	if e.Version != ExpectedVersion {
		return Expected{}, fmt.Errorf("expected results are version %d, this evaluator reads version %d", e.Version, ExpectedVersion)
	}
	return e, nil
}

// Compare says how a summary differs from what was expected. An empty result
// means it matches.
func (e Expected) Compare(s domain.Summary) []string {
	var diffs []string
	check := func(what string, got, want int) {
		if got != want {
			diffs = append(diffs, fmt.Sprintf("%s: got %d, want %d", what, got, want))
		}
	}
	a := s.Agent

	statuses := slices.Sorted(maps.Keys(mergedStatuses(e.Statuses, a.Statuses)))
	for _, st := range statuses {
		check("status "+string(st), a.Statuses[st], e.Statuses[st])
	}
	check("eligible", a.Eligible, e.Eligible)
	check("attempted", a.Attempted, e.Attempted)
	check("correct", a.Correct, e.Correct)

	got := map[int]domain.LevelScore{}
	for _, l := range a.Levels {
		got[l.Level] = l
	}
	want := map[int]ExpectedLevel{}
	for _, l := range e.Levels {
		want[l.Level] = l
	}
	for _, level := range slices.Sorted(maps.Keys(mergedLevels(got, want))) {
		g, w := got[level], want[level]
		prefix := fmt.Sprintf("level %d ", level)
		check(prefix+"eligible", g.Eligible, w.Eligible)
		check(prefix+"attempted", g.Attempted, w.Attempted)
		check(prefix+"correct", g.Correct, w.Correct)
	}

	check("replies with a FINAL ANSWER marker", a.FormatCompliance.Successes, e.FormatCompliance.Successes)
	check("replies that were answered", a.FormatCompliance.Total, e.FormatCompliance.Total)
	check("tool calls", a.Tokens.ToolCallsTotal, e.ToolCalls)
	return diffs
}

func mergedStatuses(a, b map[domain.AttemptStatus]int) map[domain.AttemptStatus]struct{} {
	out := map[domain.AttemptStatus]struct{}{}
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}

func mergedLevels(a map[int]domain.LevelScore, b map[int]ExpectedLevel) map[int]struct{} {
	out := map[int]struct{}{}
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}
