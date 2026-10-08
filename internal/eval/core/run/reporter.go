package run

import (
	"cmp"
	"context"
	"fmt"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/report"
)

var _ service.Reporter = (*Reporter)(nil)

// Reporter rebuilds a run's summaries from what it recorded, and writes them
// again. It judges by the limits the run itself recorded, so that a report made
// later reaches the same verdicts as the run did.
type Reporter struct {
	Store resource.ResultStore
	// Thresholds judge a run that did not record its own.
	Thresholds domain.Thresholds
}

// Report reads the run and writes its summary.json and summary.md.
func (r *Reporter) Report(ctx context.Context, runID string) (domain.Summary, error) {
	meta, err := r.Store.LoadMeta(ctx, runID)
	if err != nil {
		return domain.Summary{}, err
	}
	rows, err := r.Store.Load(ctx, runID)
	if err != nil {
		return domain.Summary{}, fmt.Errorf("reading the results: %w", err)
	}
	probes, err := r.Store.LoadProbes(ctx, runID)
	if err != nil {
		return domain.Summary{}, fmt.Errorf("reading the probe outcomes: %w", err)
	}
	summary := report.Aggregate(report.Input{
		RunID: runID, Rows: rows, Probes: probes, Thresholds: cmp.Or(meta.Thresholds, r.Thresholds),
	})
	if err := r.Store.WriteSummary(ctx, meta, summary); err != nil {
		return domain.Summary{}, fmt.Errorf("writing the summary: %w", err)
	}
	return summary, nil
}
