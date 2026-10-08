package resource

import (
	"context"
	"errors"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// ErrRunNotFound is returned when asked for a run that has no files.
var ErrRunNotFound = errors.New("no such run")

// ResultStore keeps a run's files: results.jsonl, run.json, probes.json,
// summary.json, summary.md and the evidence directory. Methods that take a run
// id fail with ErrRunNotFound when the run has no files.
//
//go:generate mockery --name ResultStore --filename result_store.go
type ResultStore interface {
	// Begin creates the run's directory and writes run.json. Beginning a run
	// that exists is allowed: that is how a run is resumed.
	Begin(ctx context.Context, meta domain.RunMeta) error

	// Append adds one row and flushes it, so an interrupted run leaves a valid
	// file.
	Append(ctx context.Context, runID string, row domain.Result) error

	// Load returns every complete row of the run. A last line cut short by a
	// crash is ignored.
	Load(ctx context.Context, runID string) ([]domain.Result, error)

	// Index returns the attempts already in the run, for resuming.
	Index(ctx context.Context, runID string) (map[domain.AttemptKey]struct{}, error)

	// Finish records how the run ended in run.json.
	Finish(ctx context.Context, meta domain.RunMeta) error

	// LoadMeta returns what run.json says about the run.
	LoadMeta(ctx context.Context, runID string) (domain.RunMeta, error)

	// SaveProbes keeps what the probes found, replacing any earlier outcomes.
	SaveProbes(ctx context.Context, runID string, probes []domain.ProbeOutcome) error

	// LoadProbes returns what SaveProbes kept; none is not an error.
	LoadProbes(ctx context.Context, runID string) ([]domain.ProbeOutcome, error)

	// WriteSummary writes summary.json and summary.md.
	WriteSummary(ctx context.Context, meta domain.RunMeta, summary domain.Summary) error

	// EvidencePath returns where a file called name goes in the run's evidence
	// directory, which it creates.
	EvidencePath(ctx context.Context, runID, name string) (string, error)
}
