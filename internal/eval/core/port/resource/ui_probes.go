package resource

import (
	"context"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// UIProbes runs the checks that are not a passive reading of one attempt: they
// do something to the page (stop, reload, resize) and look at what happens.
// Each method is one probe of the spec's table and returns its outcome; an
// error means the probe could not run, not that it failed.
//
//go:generate mockery --name UIProbes --filename ui_probes.go
type UIProbes interface {
	Stop(ctx context.Context) (domain.ProbeOutcome, error)            // P-01
	Regenerate(ctx context.Context) (domain.ProbeOutcome, error)      // P-02
	Lighthouse(ctx context.Context) (domain.ProbeOutcome, error)      // P-03
	Mobile(ctx context.Context) (domain.ProbeOutcome, error)          // P-04
	RenameDelete(ctx context.Context) (domain.ProbeOutcome, error)    // P-05
	Theme(ctx context.Context) (domain.ProbeOutcome, error)           // P-06
	ReloadMidStream(ctx context.Context) (domain.ProbeOutcome, error) // P-07
	KeyboardOnly(ctx context.Context) (domain.ProbeOutcome, error)    // P-08
	LayoutShift(ctx context.Context) (domain.ProbeOutcome, error)     // P-09
	AgentDown(ctx context.Context) (domain.ProbeOutcome, error)       // P-10

	// Close ends the browser session the probes used.
	Close(ctx context.Context) error
}
