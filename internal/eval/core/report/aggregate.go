package report

import (
	"cmp"
	"maps"
	"slices"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// MinOverheadSamples is how many attempts U-05 needs to be judged on the 95th
// percentile. Of fewer, the nearest-rank 95th percentile is the slowest one, and
// one slow request would decide a verdict about the UI; the median is used then.
const MinOverheadSamples = 20

// Input is what a run's summary is made of.
type Input struct {
	RunID string
	// Rows holds the run's results: one for every (task, attempt) slot, the
	// skipped ones included. Their order does not matter. If a slot appears
	// twice, the later row wins.
	Rows       []domain.Result
	Probes     []domain.ProbeOutcome
	Thresholds domain.Thresholds
}

// Aggregate turns a run's rows into the agent and UI scorecards.
func Aggregate(in Input) domain.Summary {
	th := cmp.Or(in.Thresholds, domain.DefaultThresholds())
	rows := lastPerSlot(in.Rows)
	attempted := slices.DeleteFunc(slices.Clone(rows), func(r domain.Result) bool { return !r.Status.Attempted() })
	return domain.Summary{
		RunID:      in.RunID,
		Agent:      agentScorecard(rows, attempted),
		UI:         uiScorecard(attempted, in.Probes, th),
		Thresholds: th,
	}
}

// lastPerSlot keeps one row for each (task, attempt), the last one given.
func lastPerSlot(rows []domain.Result) []domain.Result {
	position := make(map[domain.AttemptKey]int, len(rows))
	out := make([]domain.Result, 0, len(rows))
	for _, r := range rows {
		if i, seen := position[r.Key()]; seen {
			out[i] = r
			continue
		}
		position[r.Key()] = len(out)
		out = append(out, r)
	}
	return out
}

// --- agent ---------------------------------------------------------------------

func agentScorecard(rows, attempted []domain.Result) domain.AgentScorecard {
	correct := countStatus(attempted, domain.StatusCorrect)
	sc := domain.AgentScorecard{
		Eligible:          len(rows),
		Attempted:         len(attempted),
		Correct:           correct,
		AccuracyAttempted: Wilson(correct, len(attempted)),
		AccuracyEligible:  Wilson(correct, len(rows)),
		Levels:            levelScores(rows),
		Statuses:          statusCounts(rows),
		Tokens:            tokenStats(attempted),
		Repeatability:     repeatability(attempted),
	}
	if len(rows) > 0 {
		sc.Coverage = float64(len(attempted)) / float64(len(rows))
	}

	// Format compliance is judged on replies there was to judge: not on timeouts
	// or error bubbles, which carry no answer to have a marker.
	var answered, marked int
	for _, r := range attempted {
		if r.Status.Answered() {
			answered++
		}
		if r.Status.HasMarker() {
			marked++
		}
	}
	sc.FormatCompliance = Wilson(marked, answered)

	// A latency is the time to the end of a reply. A timeout or a broken page
	// carries the moment the attempt gave up, which says nothing about speed.
	sc.LatencyMs = Summarize(sample(attempted, func(r domain.Result) *int64 {
		if r.Status == domain.StatusTimeout || r.Status == domain.StatusUIError {
			return nil
		}
		return r.LatencyMs
	}))
	return sc
}

func countStatus(rows []domain.Result, status domain.AttemptStatus) int {
	n := 0
	for _, r := range rows {
		if r.Status == status {
			n++
		}
	}
	return n
}

// statusCounts has an entry for every status, so a count of zero reads as zero
// rather than as a missing key.
func statusCounts(rows []domain.Result) map[domain.AttemptStatus]int {
	counts := make(map[domain.AttemptStatus]int, len(domain.Statuses))
	for _, s := range domain.Statuses {
		counts[s] = 0
	}
	for _, r := range rows {
		counts[r.Status]++
	}
	return counts
}

func levelScores(rows []domain.Result) []domain.LevelScore {
	byLevel := map[int]*domain.LevelScore{}
	for _, r := range rows {
		ls, ok := byLevel[r.Level]
		if !ok {
			ls = &domain.LevelScore{Level: r.Level}
			byLevel[r.Level] = ls
		}
		ls.Eligible++
		if r.Status.Attempted() {
			ls.Attempted++
		}
		if r.Status == domain.StatusCorrect {
			ls.Correct++
		}
	}
	var out []domain.LevelScore
	for _, ls := range byLevel {
		ls.AccuracyAttempted = Wilson(ls.Correct, ls.Attempted)
		ls.AccuracyEligible = Wilson(ls.Correct, ls.Eligible)
		out = append(out, *ls)
	}
	slices.SortFunc(out, func(a, b domain.LevelScore) int { return cmp.Compare(a.Level, b.Level) })
	return out
}

// tokenStats sums what the agent reported. Token means are over the attempts
// that reported any; tool calls are counted on replies that were answered, since
// a timeout never got to say what it used.
func tokenStats(attempted []domain.Result) domain.TokenStats {
	var ts domain.TokenStats
	answered := 0
	for _, r := range attempted {
		if r.Tokens.Prompt+r.Tokens.Completion > 0 {
			ts.Reported++
			ts.PromptTotal += r.Tokens.Prompt
			ts.CompletionTotal += r.Tokens.Completion
		}
		if r.Status.Answered() {
			answered++
			ts.ToolCallsTotal += len(r.ToolCalls)
			if len(r.ToolCalls) > 0 {
				ts.AttemptsWithTool++
			}
		}
	}
	if ts.Reported > 0 {
		ts.PromptMean = float64(ts.PromptTotal) / float64(ts.Reported)
		ts.CompletionMean = float64(ts.CompletionTotal) / float64(ts.Reported)
	}
	if answered > 0 {
		ts.ToolCallsMean = float64(ts.ToolCallsTotal) / float64(answered)
	}
	return ts
}

// repeatability describes runs that asked the same problem more than once. It
// is nil when no problem was attempted twice.
func repeatability(attempted []domain.Result) *domain.Repeatability {
	outcomes := map[string][]bool{}
	type tally struct{ correct, total int }
	perRepeat := map[int]*tally{}
	for _, r := range attempted {
		right := r.Status == domain.StatusCorrect
		outcomes[r.TaskID] = append(outcomes[r.TaskID], right)
		t, ok := perRepeat[r.Attempt]
		if !ok {
			t = &tally{}
			perRepeat[r.Attempt] = t
		}
		t.total++
		if right {
			t.correct++
		}
	}
	flipped, tasks := Flips(outcomes)
	if tasks == 0 {
		return nil
	}
	var accuracies []float64
	for _, attempt := range slices.Sorted(maps.Keys(perRepeat)) {
		t := perRepeat[attempt]
		accuracies = append(accuracies, float64(t.correct)/float64(t.total))
	}
	return &domain.Repeatability{
		Repeats:      len(accuracies),
		Tasks:        tasks,
		MeanAccuracy: Mean(accuracies),
		SDAccuracy:   SD(accuracies),
		Flipped:      flipped,
		FlipRate:     float64(flipped) / float64(tasks),
	}
}

// --- UI ------------------------------------------------------------------------

func uiScorecard(attempted []domain.Result, probes []domain.ProbeOutcome, th domain.Thresholds) domain.UIScorecard {
	sc := domain.UIScorecard{
		Checks:     checkTallies(attempted),
		TTFTUIMs:   Summarize(sample(attempted, func(r domain.Result) *int64 { return r.TTFTUIMs })),
		OverheadMs: Summarize(sample(attempted, func(r domain.Result) *int64 { return r.UIOverheadMs })),
		MaxGapMs:   Summarize(sample(attempted, func(r domain.Result) *int64 { return r.MaxGapMs })),
		Probes:     slices.SortedFunc(slices.Values(probes), func(a, b domain.ProbeOutcome) int { return cmp.Compare(a.ID, b.ID) }),
	}

	var updates []float64
	for _, r := range attempted {
		if r.Updates > 0 {
			updates = append(updates, float64(r.Updates))
		}
	}
	sc.UpdatesMean = Mean(updates)

	// U-05 has a basis only when there is an overhead to judge: with none measured
	// its verdict is na, and no statistic decided it.
	switch n := sc.OverheadMs.N; {
	case n >= MinOverheadSamples:
		sc.OverheadBasis = "p95"
	case n > 0:
		sc.OverheadBasis = "p50"
	}
	sc.Verdicts = verdicts(sc, th)
	sc.MustFailed = mustWithVerdict(sc, domain.Fail)
	// A probe that was not run at all is not in sc.Probes: it was not asked for.
	sc.Undecided = mustWithVerdict(sc, domain.NA)
	sc.UIErrors = uiErrors(attempted)
	return sc
}

// uiErrors counts the attempts the page broke, by the class of the row.
func uiErrors(attempted []domain.Result) map[string]int {
	var counts map[string]int
	for _, r := range attempted {
		if r.Status != domain.StatusUIError {
			continue
		}
		if counts == nil {
			counts = map[string]int{}
		}
		counts[cmp.Or(r.ErrorClass, "unknown")]++
	}
	return counts
}

func checkTallies(attempted []domain.Result) map[domain.CheckID]domain.CheckTally {
	ids := map[domain.CheckID]bool{}
	for _, id := range domain.PassiveChecks {
		ids[id] = true
	}
	for _, r := range attempted {
		for id := range r.UIChecks {
			ids[id] = true
		}
	}
	tallies := make(map[domain.CheckID]domain.CheckTally, len(ids))
	for id := range ids {
		t := domain.CheckTally{Level: id.Level()}
		for _, r := range attempted {
			switch r.UIChecks[id] { // a missing entry is the empty result: not decided
			case domain.Pass:
				t.Pass++
			case domain.Fail:
				t.Fail++
			default:
				t.NA++
			}
		}
		tallies[id] = t
	}
	return tallies
}

func verdicts(sc domain.UIScorecard, th domain.Thresholds) map[domain.CheckID]domain.CheckResult {
	out := make(map[domain.CheckID]domain.CheckResult, len(sc.Checks))
	for id, t := range sc.Checks {
		switch {
		case id == domain.U05:
			// A single slow attempt is not the UI being slow: the verdict is on
			// the 95th percentile, or on the median when there are too few
			// attempts for that to be more than the slowest.
			value := sc.OverheadMs.P95
			if sc.OverheadBasis == "p50" {
				value = sc.OverheadMs.P50
			}
			switch {
			case sc.OverheadMs.N == 0:
				out[id] = domain.NA
			case value > th.OverheadP95Ms:
				out[id] = domain.Fail
			default:
				out[id] = domain.Pass
			}
		case t.Fail > 0:
			out[id] = domain.Fail
		case t.Pass > 0:
			out[id] = domain.Pass
		default:
			out[id] = domain.NA
		}
	}
	return out
}

// mustWithVerdict lists the Must checks and probes whose verdict is result, in id
// order: the ones that failed, or the ones that could not be decided.
func mustWithVerdict(sc domain.UIScorecard, result domain.CheckResult) []domain.CheckID {
	var ids []domain.CheckID
	for id, verdict := range sc.Verdicts {
		if verdict == result && sc.Checks[id].Level == domain.Must {
			ids = append(ids, id)
		}
	}
	for _, p := range sc.Probes {
		if p.Result == result && cmp.Or(p.Level, p.ID.Level()) == domain.Must {
			ids = append(ids, p.ID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// sample collects the values a field has, as floats, leaving out rows without it.
func sample(rows []domain.Result, field func(domain.Result) *int64) []float64 {
	var values []float64
	for _, r := range rows {
		if v := field(r); v != nil {
			values = append(values, float64(*v))
		}
	}
	return values
}
