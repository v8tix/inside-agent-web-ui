package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/gaia"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/store"
)

func percent(p domain.Proportion) string {
	if p.Total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%% (95%% CI %.1f–%.1f%%, %d of %d)", p.Rate*100, p.Low*100, p.High*100, p.Successes, p.Total)
}

// printOutcome writes the digest of a finished run: the numbers that matter and
// where the full report is.
func (c *cli) printOutcome(out service.Outcome, dir string) {
	var b strings.Builder
	a, ui := out.Summary.Agent, out.Summary.UI
	state := "finished"
	if out.Interrupted {
		state = "interrupted"
	}
	fmt.Fprintf(&b, "\nRun %s (%s) %s", out.Meta.RunID, out.Meta.Command, state)
	if !out.Meta.FinishedAt.IsZero() {
		fmt.Fprintf(&b, " in %s", out.Meta.FinishedAt.Sub(out.Meta.StartedAt).Round(1e9))
	}
	if out.Meta.Command != "probe" { // a run of probes alone asks no problems
		b.WriteString("\n\nAgent\n")
		c.agentDigest(&b, a)
	}

	b.WriteString("\nWeb UI\n")
	switch {
	case ui.Clean():
		b.WriteString("  all required checks passed\n")
	case len(ui.MustFailed) > 0:
		fmt.Fprintf(&b, "  FAILED: %s\n", named(ui.MustFailed))
	case len(ui.Undecided) > 0:
		fmt.Fprintf(&b, "  no required check failed; not decided: %s\n", named(ui.Undecided))
	}
	if n := ui.UIErrorTotal(); n > 0 {
		fmt.Fprintf(&b, "  the page broke %s: %s\n", domain.Attempts(n), ui.UIErrorList())
	}
	if q := ui.OverheadMs; q.N > 0 {
		basis := "95th percentile"
		if ui.OverheadBasis == "p50" {
			basis = "median: too few attempts for a 95th percentile"
		}
		fmt.Fprintf(&b, "  UI overhead: p50 %.0f ms, p95 %.0f ms (limit %.0f ms, judged on the %s)\n", q.P50, q.P95, out.Summary.Thresholds.OverheadP95Ms, basis)
	}
	if len(ui.Probes) > 0 {
		b.WriteString("\nProbes\n")
		for _, p := range ui.Probes {
			fmt.Fprintf(&b, "  %s %-18s %-5s %s\n", p.ID, p.ID.Name(), p.Result, firstLine(p.Detail))
		}
	}
	fmt.Fprintf(&b, "\nFull report: %s/summary.md\n", dir)
	c.say(c.stdout, b.String())
}

// agentDigest writes the lines about the agent's answers.
func (c *cli) agentDigest(b *strings.Builder, a domain.AgentScorecard) {
	fmt.Fprintf(b, "  asked %d of %d problems (coverage %.1f%%), %d correct\n", a.Attempted, a.Eligible, a.Coverage*100, a.Correct)
	fmt.Fprintf(b, "  accuracy of those asked:   %s\n", percent(a.AccuracyAttempted))
	fmt.Fprintf(b, "  accuracy of all eligible:  %s\n", percent(a.AccuracyEligible))
	for _, l := range a.Levels {
		fmt.Fprintf(b, "  level %d: %d of %d asked correct\n", l.Level, l.Correct, l.Attempted)
	}
	var statuses []string
	for _, s := range domain.Statuses {
		if n := a.Statuses[s]; n > 0 {
			statuses = append(statuses, fmt.Sprintf("%s %d", s, n))
		}
	}
	fmt.Fprintf(b, "  statuses: %s\n", strings.Join(statuses, " · "))
	if r := a.Repeatability; r != nil {
		fmt.Fprintf(b, "  repeatability: mean accuracy %.1f%% ± %.1f pp over %d repeats; %d of %d problems flipped\n",
			r.MeanAccuracy*100, r.SDAccuracy*100, r.Repeats, r.Flipped, r.Tasks)
	}
}

func (c *cli) printFetched(files []gaia.FetchedFile, dir string) {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	row := func(format string, args ...any) { _, _ = fmt.Fprintf(tw, format+"\n", args...) }
	row("CONFIG\tSPLIT\tFILE\tSIZE\t")
	for _, f := range files {
		note := "already there"
		if f.Downloaded {
			note = "downloaded"
		}
		row("%s\t%s\t%s\t%d bytes\t%s", f.Config, f.Split, f.Name, f.Bytes, note)
	}
	_ = tw.Flush()
	fmt.Fprintf(&b, "\n%d files in %s\n", len(files), dir)
	c.say(c.stdout, b.String())
}

func (c *cli) printInventory(inv []gaia.DatasetInfo, cfg config.Config) {
	if len(inv) == 0 {
		c.say(c.stdout, fmt.Sprintf("There are no GAIA files in %s.\nDownload them with: agent-eval datasets fetch\n", cfg.GAIA.DatasetsDir))
		return
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	row := func(format string, args ...any) { _, _ = fmt.Fprintf(tw, format+"\n", args...) }
	row("CONFIG\tSPLIT\tFILES\tROWS\tLEVEL 1\tLEVEL 2\tLEVEL 3\tATTACHMENTS\tNO ANSWER\t")
	for _, d := range inv {
		row("%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t", d.Config, d.Split, d.Files, d.Rows, d.Levels[1], d.Levels[2], d.Levels[3], d.Attachments, d.Unscored)
	}
	_ = tw.Flush()
	fmt.Fprintf(&b, "\nIn %s. Runs use split %q of %s.\n", cfg.GAIA.DatasetsDir, cfg.GAIA.Split, strings.Join(cfg.GAIA.Levels, ", "))
	if cfg.GAIA.Attachments == domain.AttachmentsSkip {
		b.WriteString("Problems with an attachment are skipped (GAIA_ATTACHMENTS=skip) and lower the coverage.\n")
	}
	c.say(c.stdout, b.String())
}

func (c *cli) printReport(format string, meta domain.RunMeta, summary domain.Summary) error {
	switch format {
	case "json":
		data, err := json.Marshal(summary, json.Deterministic(true), jsontext.WithIndent("  "))
		if err != nil {
			return fail(exitInfra, "%w", err)
		}
		c.say(c.stdout, string(data)+"\n")
	default:
		c.say(c.stdout, store.RenderMarkdown(meta, summary))
	}
	return nil
}

// firstLine keeps a detail to one line of a sensible length.
func firstLine(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > 100 {
		return string(r[:100]) + "…"
	}
	return text
}

// named lists check ids with their names: "U-07 Scroll follow, P-01 Stop".
func named(ids []domain.CheckID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = string(id) + " " + id.Name()
	}
	return strings.Join(parts, ", ")
}
