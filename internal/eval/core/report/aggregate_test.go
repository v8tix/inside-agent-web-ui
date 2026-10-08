package report_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/report"
)

// allPass is every passive check passing.
var allPass = []string{"U-01=pass", "U-02=pass", "U-03=pass", "U-04=pass", "U-05=pass", "U-06=pass", "U-07=pass", "U-08=pass"}

// checksOf reads "U-01=pass" style entries; a later entry overrides an earlier one.
func checksOf(specs ...string) map[domain.CheckID]domain.CheckResult {
	m := map[domain.CheckID]domain.CheckResult{}
	for _, s := range specs {
		id, result, _ := strings.Cut(s, "=")
		m[domain.CheckID(id)] = domain.CheckResult(result)
	}
	return m
}

func plus(extra ...string) map[domain.CheckID]domain.CheckResult {
	return checksOf(append(slices.Clone(allPass), extra...)...)
}

func row(task string, level, attempt int, status domain.AttemptStatus, with ...func(*domain.Result)) domain.Result {
	r := domain.Result{RunID: "r1", TaskID: task, Level: level, Attempt: attempt, Status: status}
	if status == domain.StatusCorrect {
		r.Score = 1
	}
	for _, f := range with {
		f(&r)
	}
	return r
}

func latency(ms int64) func(*domain.Result) { return func(r *domain.Result) { r.LatencyMs = &ms } }
func ttft(ms int64) func(*domain.Result)    { return func(r *domain.Result) { r.TTFTUIMs = &ms } }
func overhead(ms int64) func(*domain.Result) {
	return func(r *domain.Result) { r.UIOverheadMs = &ms }
}
func gap(ms int64) func(*domain.Result)  { return func(r *domain.Result) { r.MaxGapMs = &ms } }
func updates(n int) func(*domain.Result) { return func(r *domain.Result) { r.Updates = n } }
func tokens(prompt, completion int) func(*domain.Result) {
	return func(r *domain.Result) { r.Tokens = domain.Tokens{Prompt: prompt, Completion: completion} }
}
func tools(names ...string) func(*domain.Result) {
	return func(r *domain.Result) { r.ToolCalls = names }
}
func ui(checks map[domain.CheckID]domain.CheckResult) func(*domain.Result) {
	return func(r *domain.Result) { r.UIChecks = checks }
}

// sampleRun is ten slots, worked out by hand in the test below.
func sampleRun() []domain.Result {
	return []domain.Result{
		row("t1", 1, 1, domain.StatusCorrect, latency(1000), ttft(150), overhead(20), gap(100), updates(10), tokens(100, 20), tools("search", "calc"), ui(plus())),
		row("t2", 1, 1, domain.StatusCorrect, latency(2000), ttft(250), overhead(40), gap(200), updates(30), tokens(200, 40), ui(plus())),
		row("t3", 1, 1, domain.StatusIncorrect, latency(3000), ttft(350), overhead(60), gap(300), updates(20), tokens(300, 60), tools("search"), ui(plus("U-04=na", "U-06=fail"))),
		row("t4", 1, 1, domain.StatusSkipped),
		row("t5", 2, 1, domain.StatusCorrect, latency(4000), ttft(450), overhead(80), updates(40), ui(plus("U-07=na"))),
		row("t6", 2, 1, domain.StatusNoFinalAnswer, latency(5000), ttft(550), overhead(120), ui(plus("U-06=na", "U-05=fail"))),
		row("t7", 2, 1, domain.StatusAgentError, latency(600), ui(checksOf("U-01=pass", "U-02=pass", "U-03=pass"))),
		row("t8", 3, 1, domain.StatusTimeout, latency(60000), ui(checksOf("U-03=fail"))),
		row("t9", 3, 1, domain.StatusUIError),
		row("t10", 3, 1, domain.StatusSkipped),
	}
}

func assertProportion(t *testing.T, want domain.Proportion, got domain.Proportion, msg string) {
	t.Helper()
	assert.Equal(t, want.Successes, got.Successes, msg+" successes")
	assert.Equal(t, want.Total, got.Total, msg+" total")
	assert.InDelta(t, want.Rate, got.Rate, 1e-9, msg+" rate")
	assert.InDelta(t, want.Low, got.Low, 1e-9, msg+" low")
	assert.InDelta(t, want.High, got.High, 1e-9, msg+" high")
}

// The Wilson bounds below were computed outside Go, in Python, from the formula.
func TestAggregate_Agent(t *testing.T) {
	got := report.Aggregate(report.Input{RunID: "r1", Rows: sampleRun()}).Agent

	t.Run("counts and coverage", func(t *testing.T) {
		assert.Equal(t, 10, got.Eligible)
		assert.Equal(t, 8, got.Attempted, "all but the two skipped")
		assert.Equal(t, 3, got.Correct)
		assert.InDelta(t, 0.8, got.Coverage, 1e-12)
	})
	t.Run("accuracy on what was attempted and on everything eligible", func(t *testing.T) {
		assertProportion(t, domain.Proportion{Successes: 3, Total: 8, Rate: 0.375, Low: 0.136844285824, High: 0.694257605397}, got.AccuracyAttempted, "attempted")
		assertProportion(t, domain.Proportion{Successes: 3, Total: 10, Rate: 0.3, Low: 0.107791267406, High: 0.603221852539}, got.AccuracyEligible, "eligible")
	})
	t.Run("status counts, with zeros present", func(t *testing.T) {
		want := map[domain.AttemptStatus]int{
			domain.StatusCorrect: 3, domain.StatusIncorrect: 1, domain.StatusNoFinalAnswer: 1,
			domain.StatusAgentError: 1, domain.StatusTimeout: 1, domain.StatusUIError: 1, domain.StatusSkipped: 2,
		}
		assert.Equal(t, want, got.Statuses)

		empty := report.Aggregate(report.Input{Rows: []domain.Result{row("a", 1, 1, domain.StatusCorrect)}}).Agent.Statuses
		assert.Len(t, empty, len(domain.Statuses))
		assert.Zero(t, empty[domain.StatusTimeout])
	})
	t.Run("per level", func(t *testing.T) {
		require.Len(t, got.Levels, 3)
		l1, l2, l3 := got.Levels[0], got.Levels[1], got.Levels[2]

		assert.Equal(t, []int{1, 2, 3}, []int{l1.Level, l2.Level, l3.Level})
		assert.Equal(t, [3]int{4, 3, 2}, [3]int{l1.Eligible, l1.Attempted, l1.Correct})
		assertProportion(t, domain.Proportion{Successes: 2, Total: 3, Rate: 2.0 / 3, Low: 0.207659600802, High: 0.93850805528}, l1.AccuracyAttempted, "L1 attempted")
		assertProportion(t, domain.Proportion{Successes: 2, Total: 4, Rate: 0.5, Low: 0.150038989152, High: 0.849961010848}, l1.AccuracyEligible, "L1 eligible")
		assert.Equal(t, [3]int{3, 3, 1}, [3]int{l2.Eligible, l2.Attempted, l2.Correct})
		assertProportion(t, domain.Proportion{Successes: 1, Total: 3, Rate: 1.0 / 3, Low: 0.06149194472, High: 0.792340399198}, l2.AccuracyAttempted, "L2 attempted")
		assert.Equal(t, [3]int{3, 2, 0}, [3]int{l3.Eligible, l3.Attempted, l3.Correct})
		assertProportion(t, domain.Proportion{Successes: 0, Total: 2, Rate: 0, Low: 0, High: 0.657619772493}, l3.AccuracyAttempted, "L3 attempted")
		assertProportion(t, domain.Proportion{Successes: 0, Total: 3, Rate: 0, Low: 0, High: 0.561497031755}, l3.AccuracyEligible, "L3 eligible")
	})
	t.Run("format compliance is judged on answered replies", func(t *testing.T) {
		// answered: t1 t2 t3 t5 t6; with the marker: all but t6
		assertProportion(t, domain.Proportion{Successes: 4, Total: 5, Rate: 0.8, Low: 0.375534629763, High: 0.963775891368}, got.FormatCompliance, "format")
	})
	t.Run("latency leaves out timeouts and broken pages", func(t *testing.T) {
		// 600 1000 2000 3000 4000 5000; the 60000 of the timeout is not a latency
		assert.Equal(t, domain.Quantiles{N: 6, Mean: 2600, P50: 2000, P95: 5000, Max: 5000}, got.LatencyMs)
	})
	t.Run("tokens and tool calls", func(t *testing.T) {
		want := domain.TokenStats{
			Reported: 3, PromptTotal: 600, CompletionTotal: 120, PromptMean: 200, CompletionMean: 40,
			ToolCallsTotal: 3, ToolCallsMean: 0.6, AttemptsWithTool: 2,
		}
		assert.Equal(t, want, got.Tokens)
	})
	t.Run("no repeats, no repeatability", func(t *testing.T) {
		assert.Nil(t, got.Repeatability)
	})
}

func TestAggregate_UI(t *testing.T) {
	got := report.Aggregate(report.Input{RunID: "r1", Rows: sampleRun()}).UI

	t.Run("each check is tallied over the attempted slots, missing entries as na", func(t *testing.T) {
		want := map[domain.CheckID][3]int{ // pass, fail, na
			domain.U01: {6, 0, 2}, domain.U02: {6, 0, 2}, domain.U03: {6, 1, 1}, domain.U04: {4, 0, 4},
			domain.U05: {4, 1, 3}, domain.U06: {3, 1, 4}, domain.U07: {4, 0, 4}, domain.U08: {5, 0, 3},
		}
		require.Len(t, got.Checks, len(want))
		for id, w := range want {
			tally := got.Checks[id]
			assert.Equal(t, w, [3]int{tally.Pass, tally.Fail, tally.NA}, string(id))
			assert.Equal(t, 8, tally.Pass+tally.Fail+tally.NA, "%s: pass + fail + na is the number attempted", id)
			assert.Equal(t, domain.Must, tally.Level)
		}
	})
	t.Run("verdicts: any failure fails; U-05, with so few attempts, goes by the median", func(t *testing.T) {
		want := map[domain.CheckID]domain.CheckResult{
			domain.U01: domain.Pass, domain.U02: domain.Pass, domain.U03: domain.Fail, domain.U04: domain.Pass,
			domain.U05: domain.Pass, // five attempts: the median of 20 40 60 80 120 is 60, within 100 (the slowest, 120, is not what decides)
			domain.U06: domain.Fail, domain.U07: domain.Pass, domain.U08: domain.Pass,
		}
		assert.Equal(t, want, got.Verdicts)
		assert.Equal(t, "p50", got.OverheadBasis)
		assert.Equal(t, []domain.CheckID{domain.U03, domain.U06}, got.MustFailed)
	})
	t.Run("times", func(t *testing.T) {
		assert.Equal(t, domain.Quantiles{N: 5, Mean: 350, P50: 350, P95: 550, Max: 550}, got.TTFTUIMs)
		assert.Equal(t, domain.Quantiles{N: 5, Mean: 64, P50: 60, P95: 120, Max: 120}, got.OverheadMs)
		assert.Equal(t, domain.Quantiles{N: 3, Mean: 200, P50: 200, P95: 300, Max: 300}, got.MaxGapMs)
		assert.InDelta(t, 25, got.UpdatesMean, 1e-12, "a reply with no updates is not an average of zero")
	})
}

func TestAggregate_OverheadVerdict(t *testing.T) {
	series := func(values ...int64) []domain.Result {
		var rows []domain.Result
		for i, v := range values {
			rows = append(rows, row(fmt.Sprintf("t%02d", i), 1, 1, domain.StatusCorrect, overhead(v), ui(plus())))
		}
		return rows
	}
	// repeat makes n attempts of value, then the given ones.
	repeat := func(n int, value int64, then ...int64) []int64 {
		out := make([]int64, n)
		for i := range out {
			out[i] = value
		}
		return append(out, then...)
	}
	// unmeasured makes n attempts that were answered and have no overhead.
	unmeasured := func(n int) []domain.Result {
		var rows []domain.Result
		for i := range n {
			rows = append(rows, row(fmt.Sprintf("t%02d", i), 1, 1, domain.StatusCorrect, ui(plus("U-05=na"))))
		}
		return rows
	}
	tests := []struct {
		name      string
		rows      []domain.Result
		limit     float64
		want      domain.CheckResult
		wantBasis string
		wantStat  float64 // the statistic the verdict used
	}{
		// Fewer than 20 attempts: the median decides, and one slow attempt does not.
		{"a few attempts, the median at the limit passes", series(10, 20, 100, 200, 300), 100, domain.Pass, "p50", 100},
		{"a few attempts, the median a little over fails", series(10, 20, 101, 200, 300), 100, domain.Fail, "p50", 101},
		{"a cold first request among a dozen does not fail the check", series(repeat(11, 45, 400)...), 100, domain.Pass, "p50", 45},
		{"but a UI that is slow all the time does, however few the attempts", series(150, 160, 170), 100, domain.Fail, "p50", 160},
		{"nineteen attempts is still a few", series(repeat(18, 10, 500)...), 100, domain.Pass, "p50", 10},
		// Twenty or more: the 95th percentile decides.
		{"twenty attempts, the 95th percentile exactly at the limit passes", series(repeat(19, 10, 100)...), 100, domain.Pass, "p95", 10},
		{"twenty, with one outlier: it is the slowest, not the 95th percentile", series(repeat(19, 10, 500)...), 100, domain.Pass, "p95", 10},
		{"twenty, with two outliers: the 95th percentile is one of them", series(repeat(18, 10, 500, 500)...), 100, domain.Fail, "p95", 500},
		{"twenty, the 95th percentile one over the limit", series(repeat(18, 10, 101, 101)...), 100, domain.Fail, "p95", 101},
		{"a stricter limit from the settings", series(repeat(20, 30)...), 25, domain.Fail, "p95", 30},
		// No overhead measured: U-05 is not judged, so there is no statistic to name.
		{"nothing measured is not a pass", []domain.Result{row("a", 1, 1, domain.StatusCorrect, ui(plus()))}, 100, domain.NA, "", 0},
		{"a run with no overhead has no basis", unmeasured(report.MinOverheadSamples), 100, domain.NA, "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			th := domain.DefaultThresholds()
			th.OverheadP95Ms = tc.limit

			got := report.Aggregate(report.Input{Rows: tc.rows, Thresholds: th}).UI

			assert.Equal(t, tc.want, got.Verdicts[domain.U05])
			assert.Equal(t, tc.wantBasis, got.OverheadBasis)
			stat := got.OverheadMs.P95
			if tc.wantBasis == "p50" {
				stat = got.OverheadMs.P50
			}
			assert.InDelta(t, tc.wantStat, stat, 1e-9)
			assert.Equal(t, tc.want == domain.Fail, slices.Contains(got.MustFailed, domain.U05))
		})
	}
	t.Run("the cut-off is twenty attempts", func(t *testing.T) {
		assert.Equal(t, 20, report.MinOverheadSamples)
	})
}

func TestAggregate_Probes(t *testing.T) {
	probe := func(id domain.CheckID, level domain.Level, result domain.CheckResult) domain.ProbeOutcome {
		return domain.ProbeOutcome{ID: id, Level: level, Result: result}
	}
	rows := []domain.Result{row("a", 1, 1, domain.StatusCorrect, ui(plus("U-03=fail")))}
	tests := []struct {
		name   string
		probes []domain.ProbeOutcome
		want   []domain.CheckID
	}{
		{"none", nil, []domain.CheckID{domain.U03}},
		{"a failed Must probe", []domain.ProbeOutcome{probe(domain.P01, domain.Must, domain.Fail)}, []domain.CheckID{domain.P01, domain.U03}},
		{"a failed Should probe does not fail the run", []domain.ProbeOutcome{probe(domain.P04, domain.Should, domain.Fail)}, []domain.CheckID{domain.U03}},
		{"passed and na ones do not either", []domain.ProbeOutcome{probe(domain.P01, domain.Must, domain.Pass), probe(domain.P02, domain.Must, domain.NA)}, []domain.CheckID{domain.U03}},
		{"the level comes from the id when the probe has none", []domain.ProbeOutcome{probe(domain.P03, "", domain.Fail), probe(domain.P09, "", domain.Fail)}, []domain.CheckID{domain.P03, domain.U03}},
		{"a probe given twice is listed once, in id order", []domain.ProbeOutcome{probe(domain.P02, domain.Must, domain.Fail), probe(domain.P01, domain.Must, domain.Fail), probe(domain.P01, domain.Must, domain.Fail)},
			[]domain.CheckID{domain.P01, domain.P02, domain.U03}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := report.Aggregate(report.Input{Rows: rows, Probes: tc.probes}).UI

			assert.Equal(t, tc.want, got.MustFailed)
			assert.True(t, slices.IsSortedFunc(got.Probes, func(a, b domain.ProbeOutcome) int { return strings.Compare(string(a.ID), string(b.ID)) }), "probes are listed in id order")
		})
	}
}

// A run in which the page broke every attempt has decided nothing about it, and
// must not read as a run in which nothing failed.
func TestAggregate_WhereThePageBrokeAndWhatIsNotDecided(t *testing.T) {
	broke := func(class string) func(*domain.Result) { return func(r *domain.Result) { r.ErrorClass = class } }

	t.Run("attempts the page broke are counted by where", func(t *testing.T) {
		rows := []domain.Result{
			row("a", 1, 1, domain.StatusUIError, broke("start_chat")),
			row("b", 1, 1, domain.StatusUIError, broke("start_chat")),
			row("c", 1, 1, domain.StatusUIError, broke("submit")),
			row("d", 1, 1, domain.StatusCorrect, ui(plus())),
			row("e", 1, 1, domain.StatusSkipped),
		}

		got := report.Aggregate(report.Input{Rows: rows}).UI

		assert.Equal(t, map[string]int{"start_chat": 2, "submit": 1}, got.UIErrors)
	})
	t.Run("an attempt asked again and answered is no longer broken", func(t *testing.T) {
		rows := []domain.Result{
			row("a", 1, 1, domain.StatusUIError, broke("start_chat")),
			row("a", 1, 1, domain.StatusCorrect, ui(plus())), // the retry on resume
		}

		got := report.Aggregate(report.Input{Rows: rows}).UI

		assert.Empty(t, got.UIErrors)
	})
	t.Run("a row without a class is counted as unknown", func(t *testing.T) {
		got := report.Aggregate(report.Input{Rows: []domain.Result{row("a", 1, 1, domain.StatusUIError)}}).UI

		assert.Equal(t, map[string]int{"unknown": 1}, got.UIErrors)
	})
	t.Run("a Must check nobody could decide is listed, a Should one is not", func(t *testing.T) {
		rows := []domain.Result{row("a", 1, 1, domain.StatusCorrect, overhead(20), ui(plus("U-07=na", "U-06=na")))}
		probes := []domain.ProbeOutcome{
			{ID: domain.P01, Level: domain.Must, Result: domain.NA},
			{ID: domain.P02, Level: domain.Must, Result: domain.Pass},
			{ID: domain.P04, Level: domain.Should, Result: domain.NA},
		}

		got := report.Aggregate(report.Input{Rows: rows, Probes: probes}).UI

		assert.Equal(t, []domain.CheckID{domain.P01, domain.U06, domain.U07}, got.Undecided)
	})
	t.Run("a run that decided everything lists nothing", func(t *testing.T) {
		rows := []domain.Result{row("a", 1, 1, domain.StatusCorrect, overhead(20), ui(plus()))}

		got := report.Aggregate(report.Input{Rows: rows, Probes: []domain.ProbeOutcome{{ID: domain.P01, Level: domain.Must, Result: domain.Pass}}}).UI

		assert.Empty(t, got.Undecided)
		assert.Empty(t, got.UIErrors)
	})
	t.Run("a run with no attempts decides none of its passive checks", func(t *testing.T) {
		got := report.Aggregate(report.Input{}).UI

		assert.Equal(t, []domain.CheckID{domain.U01, domain.U02, domain.U03, domain.U04, domain.U05, domain.U06, domain.U07, domain.U08}, got.Undecided)
	})
}

func TestAggregate_Repeats(t *testing.T) {
	// Four problems, three repeats each:
	//   A right right right   (stable)        B right wrong right  (flips)
	//   C wrong wrong wrong   (stable)        D wrong right right  (flips)
	// Per repeat: 2/4, 2/4, 3/4 right, so mean 0.58333 and sample SD 0.14434.
	var rows []domain.Result
	for task, outcomes := range map[string][3]bool{"A": {true, true, true}, "B": {true, false, true}, "C": {false, false, false}, "D": {false, true, true}} {
		for attempt, right := range outcomes {
			status := domain.StatusIncorrect
			if right {
				status = domain.StatusCorrect
			}
			rows = append(rows, row(task, 1, attempt+1, status))
		}
	}
	// A skipped problem has a row for each repeat too; it is not an attempt.
	for attempt := 1; attempt <= 3; attempt++ {
		rows = append(rows, row("S", 1, attempt, domain.StatusSkipped))
	}

	got := report.Aggregate(report.Input{Rows: rows}).Agent

	assert.Equal(t, 15, got.Eligible)
	assert.Equal(t, 12, got.Attempted)
	assert.Equal(t, 7, got.Correct)
	assertProportion(t, domain.Proportion{Successes: 7, Total: 12, Rate: 7.0 / 12, Low: 0.31951131255, High: 0.806739686341}, got.AccuracyAttempted, "pooled")
	require.NotNil(t, got.Repeatability)
	r := got.Repeatability
	assert.Equal(t, 3, r.Repeats)
	assert.Equal(t, 4, r.Tasks, "the skipped problem is not counted")
	assert.InDelta(t, 0.58333333333, r.MeanAccuracy, 1e-9)
	assert.InDelta(t, 0.14433756730, r.SDAccuracy, 1e-9)
	assert.Equal(t, 2, r.Flipped)
	assert.InDelta(t, 0.5, r.FlipRate, 1e-12)
}

func TestAggregate_RepeatsWithOtherEndings(t *testing.T) {
	t.Run("right once and timed out once is a flip", func(t *testing.T) {
		rows := []domain.Result{
			row("A", 1, 1, domain.StatusCorrect), row("A", 1, 2, domain.StatusTimeout),
			row("B", 1, 1, domain.StatusIncorrect), row("B", 1, 2, domain.StatusNoFinalAnswer),
		}

		r := report.Aggregate(report.Input{Rows: rows}).Agent.Repeatability

		require.NotNil(t, r)
		assert.Equal(t, 1, r.Flipped)
		assert.Equal(t, 2, r.Tasks)
	})
	t.Run("a problem asked once among repeated ones does not count", func(t *testing.T) {
		rows := []domain.Result{
			row("A", 1, 1, domain.StatusCorrect), row("A", 1, 2, domain.StatusCorrect),
			row("B", 1, 1, domain.StatusCorrect), // the run stopped before B's second attempt
		}

		r := report.Aggregate(report.Input{Rows: rows}).Agent.Repeatability

		require.NotNil(t, r)
		assert.Equal(t, 1, r.Tasks)
		assert.Zero(t, r.Flipped)
	})
	t.Run("identical repeats have no spread", func(t *testing.T) {
		rows := []domain.Result{
			row("A", 1, 1, domain.StatusCorrect), row("A", 1, 2, domain.StatusCorrect),
			row("B", 1, 1, domain.StatusIncorrect), row("B", 1, 2, domain.StatusIncorrect),
		}

		r := report.Aggregate(report.Input{Rows: rows}).Agent.Repeatability

		require.NotNil(t, r)
		assert.InDelta(t, 0.5, r.MeanAccuracy, 1e-12)
		assert.Zero(t, r.SDAccuracy)
		assert.Zero(t, r.FlipRate)
	})
}

func TestAggregate_Shape(t *testing.T) {
	t.Run("nothing at all", func(t *testing.T) {
		got := report.Aggregate(report.Input{RunID: "empty"})

		assert.Equal(t, "empty", got.RunID)
		assert.Zero(t, got.Agent.Eligible)
		assert.Zero(t, got.Agent.Coverage, "no division by zero")
		assert.Zero(t, got.Agent.AccuracyAttempted.Total)
		assert.Nil(t, got.Agent.Levels)
		assert.Nil(t, got.Agent.Repeatability)
		assert.Empty(t, got.UI.MustFailed)
		for id, verdict := range got.UI.Verdicts {
			assert.Equal(t, domain.NA, verdict, "%s: with nothing to judge, nothing is passed", id)
		}
	})
	t.Run("only skipped rows", func(t *testing.T) {
		got := report.Aggregate(report.Input{Rows: []domain.Result{row("a", 1, 1, domain.StatusSkipped), row("b", 2, 1, domain.StatusSkipped)}}).Agent

		assert.Equal(t, 2, got.Eligible)
		assert.Zero(t, got.Attempted)
		assert.Zero(t, got.Coverage)
		assert.Zero(t, got.AccuracyEligible.Rate)
		assert.Equal(t, 2, got.AccuracyEligible.Total, "eligible, so the skipped still count against the agent there")
		assert.Zero(t, got.AccuracyAttempted.Total, "and not at all here")
	})
	t.Run("the order of the rows does not matter", func(t *testing.T) {
		want := report.Aggregate(report.Input{RunID: "r1", Rows: sampleRun()})
		rng := rand.New(rand.NewPCG(1, 2))

		for range 20 {
			rows := sampleRun()
			rng.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
			assert.Equal(t, want, report.Aggregate(report.Input{RunID: "r1", Rows: rows}))
		}
	})
	t.Run("a slot given twice counts once, the later row winning", func(t *testing.T) {
		rows := []domain.Result{
			row("a", 1, 1, domain.StatusTimeout),
			row("b", 1, 1, domain.StatusCorrect),
			row("a", 1, 1, domain.StatusCorrect), // a re-run of the same attempt
		}

		got := report.Aggregate(report.Input{Rows: rows}).Agent

		assert.Equal(t, 2, got.Eligible)
		assert.Equal(t, 2, got.Correct)
		assert.Zero(t, got.Statuses[domain.StatusTimeout])
	})
	t.Run("the input is not changed", func(t *testing.T) {
		rows := sampleRun()
		before := slices.Clone(rows)

		report.Aggregate(report.Input{Rows: rows})

		assert.Equal(t, before, rows)
	})
	t.Run("no thresholds means the defaults, and the summary says which", func(t *testing.T) {
		got := report.Aggregate(report.Input{})

		assert.Equal(t, domain.DefaultThresholds(), got.Thresholds)

		custom := domain.DefaultThresholds()
		custom.OverheadP95Ms = 5
		assert.Equal(t, custom, report.Aggregate(report.Input{Thresholds: custom}).Thresholds)
	})
	t.Run("a status the tool does not know shows up and is not attempted", func(t *testing.T) {
		got := report.Aggregate(report.Input{Rows: []domain.Result{row("a", 1, 1, "weird"), row("b", 1, 1, domain.StatusCorrect)}}).Agent

		assert.Equal(t, 1, got.Statuses["weird"])
		assert.Equal(t, 1, got.Attempted)
		assert.Equal(t, 2, got.Eligible)
	})
	t.Run("a check this version does not know is tallied with its own level", func(t *testing.T) {
		rows := []domain.Result{row("a", 1, 1, domain.StatusCorrect, ui(plus("U-99=fail")))}

		got := report.Aggregate(report.Input{Rows: rows}).UI

		assert.Equal(t, domain.CheckTally{Level: domain.Should, Fail: 1}, got.Checks["U-99"])
		assert.Empty(t, got.MustFailed, "a Should cannot fail a run")
	})
}
