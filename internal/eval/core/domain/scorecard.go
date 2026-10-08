package domain

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Proportion is a share together with its 95% Wilson confidence interval.
type Proportion struct {
	Successes int     `json:"successes"`
	Total     int     `json:"total"`
	Rate      float64 `json:"rate"`
	Low       float64 `json:"low"`
	High      float64 `json:"high"`
}

// Quantiles summarize a sample of values (milliseconds, usually).
type Quantiles struct {
	N    int     `json:"n"`
	Mean float64 `json:"mean,omitzero"`
	P50  float64 `json:"p50,omitzero"`
	P95  float64 `json:"p95,omitzero"`
	Max  float64 `json:"max,omitzero"`
}

// LevelScore is the agent's result on one GAIA level.
type LevelScore struct {
	Level             int        `json:"level"`
	Eligible          int        `json:"eligible"`
	Attempted         int        `json:"attempted"`
	Correct           int        `json:"correct"`
	AccuracyAttempted Proportion `json:"accuracy_attempted"`
	AccuracyEligible  Proportion `json:"accuracy_eligible"`
}

// Repeatability describes runs with --repeats above 1.
type Repeatability struct {
	Repeats      int     `json:"repeats"`
	Tasks        int     `json:"tasks"`
	MeanAccuracy float64 `json:"mean_accuracy"`
	SDAccuracy   float64 `json:"sd_accuracy"`
	// Flipped counts the tasks that were right in some repeats and wrong in
	// others; FlipRate is that share of all tasks.
	Flipped  int     `json:"flipped"`
	FlipRate float64 `json:"flip_rate"`
}

// TokenStats summarize what the agent reported using.
type TokenStats struct {
	Reported         int     `json:"reported"` // attempts that reported tokens
	PromptTotal      int     `json:"prompt_total"`
	CompletionTotal  int     `json:"completion_total"`
	PromptMean       float64 `json:"prompt_mean,omitzero"`
	CompletionMean   float64 `json:"completion_mean,omitzero"`
	ToolCallsTotal   int     `json:"tool_calls_total"`
	ToolCallsMean    float64 `json:"tool_calls_mean,omitzero"`
	AttemptsWithTool int     `json:"attempts_with_tools"`
}

// AgentScorecard is how the agent did. Its counts are attempts: every
// (task, attempt) slot of the run has a row, so with --repeats 3 a problem
// adds three to Eligible.
type AgentScorecard struct {
	// Eligible counts every slot in the run's scope, the skipped ones included,
	// so a run that skipped a fifth of its problems says so. Attempted counts
	// those that were asked.
	Eligible  int `json:"eligible"`
	Attempted int `json:"attempted"`
	Correct   int `json:"correct"`

	AccuracyAttempted Proportion `json:"accuracy_attempted"`
	AccuracyEligible  Proportion `json:"accuracy_eligible"`
	// Coverage is attempted over eligible, reported beside accuracy so skipped
	// problems cannot hide.
	Coverage float64 `json:"coverage"`

	Levels   []LevelScore          `json:"levels,omitempty"`
	Statuses map[AttemptStatus]int `json:"statuses"`

	FormatCompliance Proportion `json:"format_compliance"`
	LatencyMs        Quantiles  `json:"latency_ms"`
	Tokens           TokenStats `json:"tokens"`

	Repeatability *Repeatability `json:"repeatability,omitempty"`
}

// CheckTally counts how often a check passed.
type CheckTally struct {
	Level Level `json:"level"`
	Pass  int   `json:"pass"`
	Fail  int   `json:"fail"`
	NA    int   `json:"na"`
}

// UIScorecard is how the Web UI did.
type UIScorecard struct {
	// Checks tallies each passive check over the attempted slots. A slot where a
	// check was not recorded counts as na, so pass + fail + na is the number
	// attempted.
	Checks map[CheckID]CheckTally `json:"checks"`
	// Verdicts is the run's verdict on each passive check: fail when any attempt
	// failed it, pass when some passed and none failed, na otherwise. U-05 is
	// judged on the overhead of the whole run instead: on its 95th percentile, or
	// on its median when there are too few attempts for that, and it is na when
	// no attempt measured the overhead.
	Verdicts   map[CheckID]CheckResult `json:"verdicts"`
	TTFTUIMs   Quantiles               `json:"ttft_ui_ms"`
	OverheadMs Quantiles               `json:"ui_overhead_ms"`
	// OverheadBasis says which statistic of the overhead U-05 was judged on:
	// "p95", or "p50" when there were too few attempts for a 95th percentile to
	// mean more than the slowest one. It is empty when no attempt measured the
	// overhead, because U-05 was then not judged.
	OverheadBasis string         `json:"ui_overhead_basis,omitzero"`
	MaxGapMs      Quantiles      `json:"max_gap_ms"`
	UpdatesMean   float64        `json:"updates_mean,omitzero"`
	Probes        []ProbeOutcome `json:"probes,omitempty"`
	// MustFailed lists the Must checks and probes that failed. Any makes the
	// run exit with status 2.
	MustFailed []CheckID `json:"must_failed,omitempty"`
	// Undecided lists the Must checks and probes that no attempt could decide:
	// their verdict is na. A run with some says "no required check failed", not
	// "all required checks passed".
	Undecided []CheckID `json:"undecided,omitempty"`
	// UIErrors counts the attempts the page broke, by where it broke (the
	// error class of the row). The browser dying or the session ending means
	// the page could not be driven at all.
	UIErrors map[string]int `json:"ui_errors,omitempty"`
}

// Clean is true when the UI side of a run has nothing to say but good news:
// no required check failed, none was left undecided, and the page broke no
// attempt. Only then may a report say that all required checks passed.
func (sc UIScorecard) Clean() bool {
	return len(sc.MustFailed) == 0 && len(sc.Undecided) == 0 && len(sc.UIErrors) == 0
}

// UIErrorTotal is how many attempts the page broke.
func (sc UIScorecard) UIErrorTotal() int {
	total := 0
	for _, n := range sc.UIErrors {
		total += n
	}
	return total
}

// UIErrorList says where the page broke attempts: "start_chat 2 · submit 1".
func (sc UIScorecard) UIErrorList() string {
	parts := make([]string, 0, len(sc.UIErrors))
	for _, class := range slices.Sorted(maps.Keys(sc.UIErrors)) {
		parts = append(parts, fmt.Sprintf("%s %d", class, sc.UIErrors[class]))
	}
	return strings.Join(parts, " · ")
}

// Attempts counts attempts in words: "1 attempt", "3 attempts".
func Attempts(n int) string {
	if n == 1 {
		return "1 attempt"
	}
	return fmt.Sprintf("%d attempts", n)
}

// Summary is everything a report is made of.
type Summary struct {
	RunID string         `json:"run_id"`
	Agent AgentScorecard `json:"agent"`
	UI    UIScorecard    `json:"ui"`
	// Thresholds are the limits the verdicts were judged against.
	Thresholds Thresholds `json:"thresholds"`
}
