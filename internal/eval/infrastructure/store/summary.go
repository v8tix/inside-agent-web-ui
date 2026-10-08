package store

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/report"
)

// WriteSummary writes summary.json and summary.md.
func (s *Store) WriteSummary(_ context.Context, meta domain.RunMeta, summary domain.Summary) error {
	dir, err := s.existingRunDir(meta.RunID)
	if err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, summaryJSON), summary); err != nil {
		return fmt.Errorf("writing %s: %w", summaryJSON, err)
	}
	if err := writeFileAtomic(filepath.Join(dir, summaryMD), []byte(RenderMarkdown(meta, summary))); err != nil {
		return fmt.Errorf("writing %s: %w", summaryMD, err)
	}
	return nil
}

// doc builds markdown a line at a time.
type doc struct{ b strings.Builder }

func (r *doc) line(format string, args ...any) {
	fmt.Fprintf(&r.b, format, args...)
	r.b.WriteByte('\n')
}

func (r *doc) blank() { r.b.WriteByte('\n') }

// RenderMarkdown is the summary for people. It holds numbers and names only:
// nothing in a Summary is question, truth or answer text.
func RenderMarkdown(meta domain.RunMeta, s domain.Summary) string {
	var r doc
	r.line("# agent-eval run %s", meta.RunID)
	r.blank()
	writeFacts(&r, meta)
	writeAgent(&r, s.Agent)
	writeUI(&r, s)
	writeSettings(&r, meta)
	return r.b.String()
}

func writeFacts(r *doc, meta domain.RunMeta) {
	r.line("- **Command:** %s", meta.Command)
	switch {
	case meta.StartedAt.IsZero():
	case meta.FinishedAt.IsZero():
		r.line("- **Started:** %s, not finished", stamp(meta.StartedAt))
	default:
		r.line("- **Started:** %s · **finished:** %s (%s)", stamp(meta.StartedAt), stamp(meta.FinishedAt), meta.FinishedAt.Sub(meta.StartedAt).Round(time.Second))
	}
	var tool []string
	if meta.UIContract != "" {
		tool = append(tool, "Web UI contract "+meta.UIContract)
	}
	if meta.GitSHA != "" {
		tool = append(tool, "git "+meta.GitSHA)
	}
	if len(tool) > 0 {
		r.line("- **Build:** %s", strings.Join(tool, " · "))
	}
	if versions := withoutSecrets(meta.Versions); len(versions) > 0 {
		var parts []string
		for _, name := range slices.Sorted(maps.Keys(versions)) {
			parts = append(parts, name+" "+versions[name])
		}
		r.line("- **Versions:** %s", strings.Join(parts, ", "))
	}
	r.blank()
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") }

// --- agent -----------------------------------------------------------------------

func writeAgent(r *doc, a domain.AgentScorecard) {
	r.line("## Agent")
	r.blank()
	r.line("| Level | Eligible | Attempted | Correct | Accuracy (attempted) | 95%% CI | Accuracy (eligible) | 95%% CI |")
	r.line("|:--|--:|--:|--:|--:|:--|--:|:--|")
	for _, l := range a.Levels {
		r.line("| %d | %d | %d | %d | %s | %s | %s | %s |", l.Level, l.Eligible, l.Attempted, l.Correct,
			share(l.AccuracyAttempted), interval(l.AccuracyAttempted), share(l.AccuracyEligible), interval(l.AccuracyEligible))
	}
	r.line("| **All** | %d | %d | %d | %s | %s | %s | %s |", a.Eligible, a.Attempted, a.Correct,
		share(a.AccuracyAttempted), interval(a.AccuracyAttempted), share(a.AccuracyEligible), interval(a.AccuracyEligible))
	r.blank()

	r.line("- **Coverage:** %s (%d of %d asked; a skipped problem lowers the eligible accuracy, not the attempted one)", pct(a.Coverage), a.Attempted, a.Eligible)
	r.line("- **Statuses:** %s", statuses(a.Statuses))
	if f := a.FormatCompliance; f.Total > 0 {
		r.line("- **Format compliance:** %s of %d answered replies ended with a `FINAL ANSWER:` marker (95%% CI %s)", pct(f.Rate), f.Total, interval(f))
	} else {
		r.line("- **Format compliance:** no replies to judge")
	}
	if q := a.LatencyMs; q.N > 0 {
		r.line("- **Latency** (submit to end of reply): p50 %s · p95 %s · mean %s · max %s (n=%d)", millis(q.P50), millis(q.P95), millis(q.Mean), millis(q.Max), q.N)
	} else {
		r.line("- **Latency:** no replies timed")
	}
	if t := a.Tokens; t.Reported > 0 {
		r.line("- **Tokens** (reported by %d attempts): mean %.0f prompt + %.0f completion", t.Reported, t.PromptMean, t.CompletionMean)
	} else {
		r.line("- **Tokens:** not reported by the agent")
	}
	if t := a.Tokens; t.ToolCallsTotal > 0 {
		r.line("- **Tool calls:** %d in all, %.2f per answered reply; %d replies used tools", t.ToolCallsTotal, t.ToolCallsMean, t.AttemptsWithTool)
	} else {
		r.line("- **Tool calls:** none reported")
	}
	if rep := a.Repeatability; rep != nil {
		r.line("- **Repeatability** (%d repeats, %d problems): mean accuracy %s ± %.1f pp (standard deviation across repeats); %d problems flipped between right and wrong (%s)",
			rep.Repeats, rep.Tasks, pct(rep.MeanAccuracy), rep.SDAccuracy*100, rep.Flipped, pct(rep.FlipRate))
	}
	r.blank()
}

func statuses(counts map[domain.AttemptStatus]int) string {
	var parts []string
	for _, s := range domain.Statuses {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", s, counts[s]))
		}
	}
	for _, s := range slices.Sorted(maps.Keys(counts)) { // anything this version does not know
		if !s.Valid() && counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", s, counts[s]))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " · ")
}

// --- UI ----------------------------------------------------------------------------

func writeUI(r *doc, s domain.Summary) {
	r.line("## Web UI")
	r.blank()
	writeChecks(r, s.UI)
	writeMeasures(r, s)
	writeProbes(r, s.UI.Probes)
	writeResult(r, s.UI)
	r.blank()
}

// writeChecks is the table of the checks made on every attempt.
func writeChecks(r *doc, ui domain.UIScorecard) {
	r.line("| Check | Level | Pass | Fail | N/A | Verdict |")
	r.line("|:--|:--|--:|--:|--:|:--|")
	ids := slices.Clone(domain.PassiveChecks)
	for _, id := range slices.Sorted(maps.Keys(ui.Checks)) {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		t, ok := ui.Checks[id]
		if !ok {
			continue
		}
		verdict := ui.Verdicts[id]
		if verdict == "" {
			verdict = domain.NA
		}
		r.line("| %s %s | %s | %d | %d | %d | %s |", id, id.Name(), t.Level, t.Pass, t.Fail, t.NA, verdict)
	}
	r.blank()
	switch ui.OverheadBasis {
	case "p50":
		r.line("U-05 is judged on the median of the overhead, because fewer than %d attempts is too few for a 95th percentile to be more than the slowest one.", report.MinOverheadSamples)
	case "p95":
		r.line("U-05 is judged on the 95th percentile of the overhead over all attempts, not on each one.")
	default:
		r.line("U-05 is not judged, because no attempt measured the overhead.")
	}
	r.blank()
}

// writeMeasures lists the timings the checks were judged on.
func writeMeasures(r *doc, s domain.Summary) {
	ui := s.UI
	if q := ui.TTFTUIMs; q.N > 0 {
		r.line("- **TTFT (UI)** (Enter to the first text in the reply): p50 %s · p95 %s (n=%d)", millis(q.P50), millis(q.P95), q.N)
	}
	if q := ui.OverheadMs; q.N > 0 {
		basis := "p95"
		if ui.OverheadBasis == "p50" {
			basis = "the median"
		}
		r.line("- **UI overhead** (TTFT in the UI minus the agent's own): p50 %s · p95 %s, against a limit of %s at %s (n=%d)",
			millis(q.P50), millis(q.P95), millis(s.Thresholds.OverheadP95Ms), basis, q.N)
	}
	if ui.UpdatesMean > 0 {
		line := fmt.Sprintf("- **Update cadence** (informational): %.1f updates per reply", ui.UpdatesMean)
		if q := ui.MaxGapMs; q.N > 0 {
			line += fmt.Sprintf(", largest gap p95 %s", millis(q.P95))
		}
		r.line("%s", line)
	}
	r.blank()
}

// writeProbes is the table of the probes, when any were run.
func writeProbes(r *doc, probes []domain.ProbeOutcome) {
	if len(probes) == 0 {
		return
	}
	r.line("### Probes")
	r.blank()
	r.line("| Probe | Level | Result | Detail |")
	r.line("|:--|:--|:--|:--|")
	for _, p := range probes {
		level := p.Level
		if level == "" {
			level = p.ID.Level()
		}
		r.line("| %s %s | %s | %s | %s |", p.ID, p.ID.Name(), level, p.Result, cell(p.Detail))
	}
	r.blank()
}

// writeResult says whether the run's required checks passed, and what the page
// broke.
func writeResult(r *doc, ui domain.UIScorecard) {
	switch {
	case ui.Clean():
		r.line("**Result:** all Must checks passed.")
	case len(ui.MustFailed) > 0:
		r.line("**Result:** Must checks failed: %s. The run exits with status 2.", domain.JoinChecks(ui.MustFailed))
	default:
		r.line("**Result:** no Must check failed%s.", notDecided(ui))
	}
	if n := ui.UIErrorTotal(); n > 0 {
		r.line("")
		r.line("The page broke %s: %s.", domain.Attempts(n), ui.UIErrorList())
	}
}

// notDecided says which required checks no attempt could decide, for the end of
// a sentence; empty when all were.
func notDecided(ui domain.UIScorecard) string {
	if len(ui.Undecided) == 0 {
		return ""
	}
	return ", but these were not decided: " + domain.JoinChecks(ui.Undecided)
}

func writeSettings(r *doc, meta domain.RunMeta) {
	settings := withoutSecrets(meta.Settings)
	if len(settings) == 0 {
		return
	}
	r.line("## Settings")
	r.blank()
	r.line("| Setting | Value |")
	r.line("|:--|:--|")
	for _, k := range slices.Sorted(maps.Keys(settings)) {
		r.line("| %s | %s |", k, cell(settings[k]))
	}
	r.blank()
}

// --- formatting --------------------------------------------------------------------

func pct(rate float64) string { return strconv.FormatFloat(rate*100, 'f', 1, 64) + "%" }

// share is a proportion as a percentage, or n/a when there was nothing to count.
func share(p domain.Proportion) string {
	if p.Total == 0 {
		return "n/a"
	}
	return pct(p.Rate)
}

func interval(p domain.Proportion) string {
	if p.Total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f–%.1f%%", p.Low*100, p.High*100)
}

// millis writes a time in milliseconds the way a person reads it.
func millis(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.1f s", v/1000)
	}
	return fmt.Sprintf("%.0f ms", v)
}

// cell makes text safe for a table cell: one line, no pipes, not too long.
func cell(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	text = strings.ReplaceAll(text, "|", `\|`)
	if runes := []rune(text); len(runes) > 200 {
		text = string(runes[:200]) + "…"
	}
	return text
}
