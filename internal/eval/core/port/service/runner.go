// Package service declares the evaluator's use cases: what the command line
// asks for. The packages under internal/eval/core implement them.
package service

import (
	"context"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// Filter picks which problems to ask.
type Filter struct {
	// Level is 1 to 3, or 0 for every level.
	Level int
	// Limit stops after this many problems; 0 means no limit.
	Limit int
	// TaskIDs asks only these; empty means all.
	TaskIDs []string
	// Attachments says what to do with problems that come with a file.
	Attachments domain.AttachmentsMode
}

// RunRequest is one run.
type RunRequest struct {
	// RunID names the run. When it names a run that exists, finished attempts
	// are skipped (resume).
	RunID string
	// Command is "run" or "selftest", recorded in run.json.
	Command string

	Filter  Filter
	Repeats int // at least 1
	Workers int // at least 1
	// Timeout is how long one attempt may take before Stop is pressed.
	Timeout     time.Duration
	HumanTyping bool
	// IncludeText keeps question, truth and answer text in the results.
	IncludeText bool
	// KeepChats leaves the conversations in place after each attempt.
	KeepChats bool
	// Screenshots keeps a picture of the page when an attempt fails. It shows
	// the question and the reply, so a run over GAIA takes them only when it
	// keeps text (IncludeText); the self-test, whose problems are invented, always.
	Screenshots bool
	// Email and Password sign the driver in to the Web UI.
	Email, Password string
}

// Outcome is how a run went.
type Outcome struct {
	Meta    domain.RunMeta
	Summary domain.Summary
	// Interrupted is true when the run was cut short by cancellation.
	Interrupted bool
}

// Runner asks problems through the browser and records every attempt.
// Failures are rows, never errors; an error means the run itself could not go on.
//
//go:generate mockery --name Runner --filename runner.go
type Runner interface {
	Run(ctx context.Context, req RunRequest) (Outcome, error)
}

// Reporter rebuilds a run's summaries from its results file.
//
//go:generate mockery --name Reporter --filename reporter.go
type Reporter interface {
	Report(ctx context.Context, runID string) (domain.Summary, error)
}

// Prober runs the UI probes.
//
//go:generate mockery --name Prober --filename prober.go
type Prober interface {
	Probe(ctx context.Context, runID string) (domain.Summary, error)
}
