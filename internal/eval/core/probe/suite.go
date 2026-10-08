package probe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service"
)

// closeWait is how long the browser gets to close at the end.
const closeWait = 15 * time.Second

// keepWait is how long keeping what the probes found, and making the summary
// again, gets once the browser is closed.
const keepWait = 10 * time.Second

// errKeepTime is why keeping fails when it runs out of its time. It is not
// context.DeadlineExceeded: the command line takes that for the user's Ctrl-C.
var errKeepTime = fmt.Errorf("took longer than %s", keepWait)

var _ service.Prober = (*Suite)(nil)

// Suite runs the UI probes one after another, keeps what they found with the run,
// and has the run's summary made again so that it shows them.
//
// A probe that cannot be carried out (a button was not there, the page would not
// load) is recorded as not applicable with the reason; it is never taken for a
// failure of the UI, and it never stops the others, unless the browser itself is
// gone.
type Suite struct {
	Probes   resource.UIProbes
	Store    resource.ResultStore
	Reporter service.Reporter
	Logger   *slog.Logger
}

// Probe runs the probes for the run, which must exist. Their outcomes take the
// place of the ones the run has from an earlier suite.
//
// When ctx ends, the probes that finished are kept and the error is returned
// with their summary. A probe that did not finish keeps the outcome the run
// already has, if it has one. Keeping has ten seconds; an error that says it
// took longer is not the end of ctx.
func (s *Suite) Probe(ctx context.Context, runID string) (domain.Summary, error) {
	log := s.log()
	run := map[domain.CheckID]func(context.Context) (domain.ProbeOutcome, error){
		domain.P01: s.Probes.Stop, domain.P02: s.Probes.Regenerate, domain.P03: s.Probes.Lighthouse,
		domain.P04: s.Probes.Mobile, domain.P05: s.Probes.RenameDelete, domain.P06: s.Probes.Theme,
		domain.P07: s.Probes.ReloadMidStream, domain.P08: s.Probes.KeyboardOnly, domain.P09: s.Probes.LayoutShift,
		domain.P10: s.Probes.AgentDown,
	}

	var outcomes []domain.ProbeOutcome
	browserGone := ""
	for _, id := range Order {
		if ctx.Err() != nil {
			break
		}
		if browserGone != "" {
			outcomes = append(outcomes, NotRun(id, browserGone))
			continue
		}
		o, err := run[id](ctx)
		switch {
		case ctx.Err() != nil:
			// Interrupted in the middle of one: it is not recorded as anything, not
			// even a verdict it got to, which would rest on a page that was no longer
			// being waited for.
			continue
		case err == nil:
			o.ID, o.Level = id, id.Level()
			log.InfoContext(ctx, "probe done", "probe", id, "name", id.Name(), "result", o.Result, "detail", o.Detail)
		case errors.Is(err, domain.ErrDriverDied):
			log.WarnContext(ctx, "the browser session ended during a probe", "probe", id, "error", err)
			o = NotRun(id, "the browser session ended: "+oneLine(err))
			browserGone = "not run: the browser session ended during " + string(id)
		default:
			log.WarnContext(ctx, "a probe could not be carried out", "probe", id, "error", err)
			o = NotRun(id, "could not be carried out: "+oneLine(err))
		}
		outcomes = append(outcomes, o)
	}
	// A suite that was cut short has no outcome for the probes it did not finish.
	cutShort := len(outcomes) < len(Order)

	// Whatever the probes found is kept, even when the run was interrupted. That
	// outlives Ctrl-C, so the closing and the keeping each get a time of their own.
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeWait)
	defer cancel()
	if err := s.Probes.Close(closeCtx); err != nil {
		log.Warn("could not close the browser cleanly", "error", err)
	}
	keepCtx, stop := context.WithTimeoutCause(context.WithoutCancel(ctx), keepWait, errKeepTime)
	defer stop()
	if cutShort {
		// The store replaces what it holds, so what an earlier suite found is read
		// first. When it cannot be read, nothing is saved over it.
		earlier, err := s.Store.LoadProbes(keepCtx, runID)
		if err != nil {
			return domain.Summary{}, fmt.Errorf("reading the earlier probe outcomes: %w", keepFailure(keepCtx, err))
		}
		outcomes = withEarlier(outcomes, earlier)
	}
	if err := s.Store.SaveProbes(keepCtx, runID, Sorted(outcomes)); err != nil {
		return domain.Summary{}, fmt.Errorf("keeping the probe outcomes: %w", keepFailure(keepCtx, err))
	}
	summary, err := s.Reporter.Report(keepCtx, runID)
	if err != nil {
		return domain.Summary{}, fmt.Errorf("making the summary again: %w", keepFailure(keepCtx, err))
	}
	return summary, ctx.Err()
}

// withEarlier returns found and, for every probe that has no outcome in it, the
// outcome that earlier holds for that probe.
func withEarlier(found, earlier []domain.ProbeOutcome) []domain.ProbeOutcome {
	out := slices.Clone(found)
	for _, e := range earlier {
		if !slices.ContainsFunc(found, func(o domain.ProbeOutcome) bool { return o.ID == e.ID }) {
			out = append(out, e)
		}
	}
	return out
}

// keepFailure is why keeping failed. When keepCtx has ended that is the time it
// was given: a store that stops when its context ends returns the context's own
// error, which is not the run's.
func keepFailure(keepCtx context.Context, err error) error {
	if keepCtx.Err() != nil {
		return context.Cause(keepCtx)
	}
	return err
}

func (s *Suite) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// oneLine shortens an error to a single line fit for a report.
func oneLine(err error) string {
	text := strings.Join(strings.Fields(err.Error()), " ")
	if r := []rune(text); len(r) > 240 {
		return string(r[:240]) + "…"
	}
	return text
}
