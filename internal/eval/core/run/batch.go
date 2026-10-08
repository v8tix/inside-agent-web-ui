package run

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/report"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/score"
)

// ClassDriverDied marks a row for an attempt during which the browser session
// died twice, once more than the single retry allows.
const ClassDriverDied = "driver_died"

// closeWait is how long a driver gets to close its browser.
const closeWait = 10 * time.Second

// summaryWait is how long a run gets, once it is over, to write its summary and
// to mark itself finished.
const summaryWait = 10 * time.Second

var _ service.Runner = (*Runner)(nil)

// RunInfo describes the build for run.json.
type RunInfo struct {
	GitSHA     string
	UIContract string
	Versions   map[string]string
}

// Runner asks a set of problems through the browser, one worker per browser,
// and records every attempt as it finishes.
//
// Failures are rows, never errors: an attempt that went wrong has a status.
// The run itself stops only when it cannot go on (no browser to be had, wrong
// credentials, no place to write) or when the context is cancelled, which
// leaves what was recorded in place for a later --resume.
type Runner struct {
	Source  resource.ProblemSource
	Drivers resource.DriverFactory
	Store   resource.ResultStore

	Prompt     score.Prompt
	Thresholds domain.Thresholds
	Info       RunInfo
	Logger     *slog.Logger

	// Versions, when set, is asked for the versions of the tools that were used
	// (the browser, the MCP server) once the run is over, because some of them
	// are only known after a browser has been started.
	Versions func() map[string]string
	// StopWait is how long Stop gets to take effect after a timeout; zero means
	// the Attempter's default.
	StopWait time.Duration
	// Now tells the time; nil means time.Now.
	Now func() time.Time
}

// SelectionError means the problems could not be chosen: the dataset is
// missing or unreadable, or a task id asked for does not exist. It is the user's
// to fix, not a failure of the machinery.
type SelectionError struct{ Err error }

func (e *SelectionError) Error() string { return "choosing the problems: " + e.Err.Error() }
func (e *SelectionError) Unwrap() error { return e.Err }

// slot is one place in the run: a problem and which try at it this is.
type slot struct {
	problem domain.Problem
	attempt int
	skip    string // why it is not asked; empty means it is
}

// Run asks the problems the request selects and returns how it went.
func (r *Runner) Run(ctx context.Context, req service.RunRequest) (service.Outcome, error) {
	if err := validate(req); err != nil {
		return service.Outcome{}, err
	}
	slots, err := r.plan(ctx, req)
	if err != nil {
		return service.Outcome{}, &SelectionError{Err: err}
	}

	meta := r.meta(req)
	r.warnOfChangedSettings(ctx, req.RunID, meta.Settings)
	if err := r.Store.Begin(ctx, meta); err != nil {
		return service.Outcome{}, fmt.Errorf("starting the run: %w", err)
	}
	// Beginning a run that exists resumes it, and the store keeps the time it
	// first started. The run goes on with that time, not with this session's.
	if stored, err := r.Store.LoadMeta(ctx, req.RunID); err == nil && !stored.StartedAt.IsZero() {
		meta.StartedAt = stored.StartedAt
	}
	finished, err := r.Store.Index(ctx, req.RunID)
	if err != nil {
		return service.Outcome{}, fmt.Errorf("reading the finished attempts: %w", err)
	}
	slots = slices.DeleteFunc(slots, func(s slot) bool {
		_, done := finished[domain.AttemptKey{TaskID: s.problem.TaskID, Attempt: s.attempt}]
		return done
	})
	r.log().InfoContext(ctx, "run starting", "run", req.RunID, "left_to_do", len(slots), "already_done", len(finished))

	runErr := r.record(ctx, req, slots)
	interrupted := ctx.Err() != nil

	if r.Versions != nil {
		meta.Versions = mergeVersions(meta.Versions, r.Versions())
	}

	// The summary is written for a run cut short too: it is what the rows say so far.
	// Its context outlives Ctrl-C, so it gets a time of its own.
	summaryCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), summaryWait)
	defer stop()
	if !interrupted && runErr == nil {
		meta.FinishedAt = r.now()
	}
	summary, err := r.summarize(summaryCtx, req.RunID, meta)
	if err != nil {
		return service.Outcome{}, errors.Join(runErr, err)
	}
	if !meta.FinishedAt.IsZero() {
		if err := r.Store.Finish(summaryCtx, meta); err != nil {
			return service.Outcome{}, errors.Join(runErr, fmt.Errorf("finishing the run: %w", err))
		}
	}
	return service.Outcome{Meta: meta, Summary: summary, Interrupted: interrupted}, runErr
}

// warnOfChangedSettings says so when a run that already exists is resumed with
// other settings: run.json keeps only the latest, so the run quietly becomes a
// different one.
func (r *Runner) warnOfChangedSettings(ctx context.Context, runID string, now map[string]string) {
	before, err := r.Store.LoadMeta(ctx, runID)
	if err != nil || len(before.Settings) == 0 {
		return // a new run has nothing to compare with
	}
	var changes []string
	for _, key := range slices.Sorted(maps.Keys(maps.Collect(joinKeys(before.Settings, now)))) {
		was, is := before.Settings[key], now[key]
		if was != is {
			changes = append(changes, fmt.Sprintf("%s: %s -> %s", key, cmp.Or(was, "(unset)"), cmp.Or(is, "(unset)")))
		}
	}
	if len(changes) > 0 {
		r.log().WarnContext(ctx, "resuming a run with other settings", "run", runID, "changed", strings.Join(changes, "; "))
	}
}

// joinKeys yields every key of a and b, once.
func joinKeys(a, b map[string]string) iter.Seq2[string, struct{}] {
	return func(yield func(string, struct{}) bool) {
		for k := range a {
			if !yield(k, struct{}{}) {
				return
			}
		}
		for k := range b {
			if !yield(k, struct{}{}) {
				return
			}
		}
	}
}

// mergeVersions adds late to base; a name in both takes the late value.
func mergeVersions(base, late map[string]string) map[string]string {
	if len(late) == 0 {
		return base
	}
	out := maps.Clone(base)
	if out == nil {
		out = make(map[string]string, len(late))
	}
	maps.Copy(out, late)
	return out
}

func validate(req service.RunRequest) error {
	switch {
	case req.RunID == "":
		return errors.New("a run needs an id")
	case req.Repeats < 1:
		return fmt.Errorf("repeats must be at least 1 (got %d)", req.Repeats)
	case req.Workers < 1:
		return fmt.Errorf("workers must be at least 1 (got %d)", req.Workers)
	case req.Timeout <= 0:
		return errors.New("the attempt timeout must be positive")
	case req.Email == "" || req.Password == "":
		return errors.New("the evaluator needs the email and password of a Web UI user")
	}
	return nil
}

// plan lists the slots of the run in the order they are asked: every problem
// once, then every problem a second time, and so on, so that a run cut short
// has finished whole repeats first.
func (r *Runner) plan(ctx context.Context, req service.RunRequest) ([]slot, error) {
	var chosen []Selected
	for sel, err := range Select(ctx, r.Source, req.Filter) {
		if err != nil {
			return nil, err
		}
		chosen = append(chosen, sel)
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("no problem matches %s", describe(req.Filter))
	}
	var slots []slot
	for attempt := 1; attempt <= req.Repeats; attempt++ {
		for _, sel := range chosen {
			slots = append(slots, slot{problem: sel.Problem, attempt: attempt, skip: sel.Skip})
		}
	}
	return slots, nil
}

// describe says what a filter asks for, for the message about nothing matching it.
func describe(f service.Filter) string {
	var parts []string
	if f.Level != 0 {
		parts = append(parts, "level "+strconv.Itoa(f.Level))
	}
	if len(f.TaskIDs) > 0 {
		parts = append(parts, "task "+strings.Join(f.TaskIDs, ", "))
	}
	if len(parts) == 0 {
		return "the dataset (it holds no problems)"
	}
	return strings.Join(parts, " and ")
}

func (r *Runner) meta(req service.RunRequest) domain.RunMeta {
	f := req.Filter
	settings := map[string]string{
		"attachments": string(f.Attachments),
		"repeats":     strconv.Itoa(req.Repeats),
		"workers":     strconv.Itoa(req.Workers),
		"timeout":     req.Timeout.String(),
		"keep_chats":  strconv.FormatBool(req.KeepChats),
	}
	if f.Level != 0 {
		settings["level"] = strconv.Itoa(f.Level)
	}
	if f.Limit != 0 {
		settings["limit"] = strconv.Itoa(f.Limit)
	}
	if len(f.TaskIDs) > 0 {
		settings["task_ids"] = strings.Join(f.TaskIDs, ",")
	}
	// A run that types the prompt key by key says so: it uses the page in another
	// way than one that fills the prompt in, and a resume should keep to it.
	if req.HumanTyping {
		settings["human_typing"] = "true"
	}
	return domain.RunMeta{
		RunID:       req.RunID,
		Command:     req.Command,
		StartedAt:   r.now(),
		GitSHA:      r.Info.GitSHA,
		UIContract:  r.Info.UIContract,
		Versions:    maps.Clone(r.Info.Versions),
		Settings:    settings,
		Thresholds:  r.Thresholds,
		IncludeText: req.IncludeText,
	}
}

func (r *Runner) summarize(ctx context.Context, runID string, meta domain.RunMeta) (domain.Summary, error) {
	rows, err := r.Store.Load(ctx, runID)
	if err != nil {
		return domain.Summary{}, fmt.Errorf("reading the results: %w", err)
	}
	probes, err := r.Store.LoadProbes(ctx, runID)
	if err != nil {
		return domain.Summary{}, fmt.Errorf("reading the probe outcomes: %w", err)
	}
	summary := report.Aggregate(report.Input{RunID: runID, Rows: rows, Probes: probes, Thresholds: r.Thresholds})
	if err := r.Store.WriteSummary(ctx, meta, summary); err != nil {
		return domain.Summary{}, fmt.Errorf("writing the summary: %w", err)
	}
	return summary, nil
}

// record writes the skipped slots as rows, then asks the rest with the workers.
func (r *Runner) record(ctx context.Context, req service.RunRequest, slots []slot) error {
	var asked []slot
	for _, s := range slots {
		if s.skip == "" {
			asked = append(asked, s)
			continue
		}
		row := domain.Result{
			RunID: req.RunID, TaskID: s.problem.TaskID, Level: s.problem.Level, Attempt: s.attempt,
			Status: domain.StatusSkipped, ErrorClass: s.skip,
		}
		if err := r.Store.Append(ctx, req.RunID, row); err != nil {
			return fmt.Errorf("recording a skipped problem: %w", err)
		}
	}
	if len(asked) == 0 {
		return nil
	}
	return r.work(ctx, req, asked)
}

// work runs the workers over the slots until they are all done, the context
// ends, or one worker finds the run cannot go on.
func (r *Runner) work(ctx context.Context, req service.RunRequest, slots []slot) error {
	workCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	progress := &progress{total: len(slots)}
	queue := make(chan slot)
	var wg sync.WaitGroup
	for id := range min(req.Workers, len(slots)) {
		wg.Go(func() {
			if err := r.worker(workCtx, req, id+1, queue, progress); err != nil {
				stop(err)
			}
		})
	}

	feed(workCtx, queue, slots)
	close(queue)
	wg.Wait()

	if ctx.Err() != nil {
		return nil // interrupted; the caller reports that, and rows stay for --resume
	}
	if cause := context.Cause(workCtx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}

// feed hands the slots to the workers, one at a time, until none is left or the
// context ends. It looks at the context before every slot: when a worker is
// ready and the context has ended, the select below can go either way and picks
// at random, so by itself it would start one more attempt after Ctrl-C in half
// of the cases.
func feed(ctx context.Context, queue chan<- slot, slots []slot) {
	for _, s := range slots {
		if ctx.Err() != nil {
			return
		}
		select {
		case queue <- s:
		case <-ctx.Done():
			return
		}
	}
}

type progress struct {
	total int
	done  atomic.Int64
}

// worker owns one browser. It takes slots from the queue until it is closed.
func (r *Runner) worker(ctx context.Context, req service.RunRequest, id int, queue <-chan slot, prog *progress) error {
	log := r.log().With("worker", id)
	var driver resource.UIDriver
	defer func() {
		if driver != nil {
			closeDriver(ctx, log, driver)
		}
	}()

	// start opens a browser and signs in. When one is already open it was found
	// dead, so it is let go of first.
	start := func() error {
		if driver != nil {
			closeDriver(ctx, log, driver)
			driver = nil
		}
		d, err := r.Drivers.NewDriver(ctx)
		if err != nil {
			return fmt.Errorf("worker %d: starting a browser: %w", id, err)
		}
		if err := d.Login(ctx, req.Email, req.Password); err != nil {
			closeDriver(ctx, log, d)
			return fmt.Errorf("worker %d: signing in: %w", id, err)
		}
		driver = d
		return nil
	}
	if err := start(); err != nil {
		return err
	}

	attempter := &Attempter{
		Store: r.Store, Prompt: r.Prompt, Thresholds: r.Thresholds, Timeout: req.Timeout, StopWait: r.StopWait,
		KeepChats: req.KeepChats, IncludeText: req.IncludeText, Screenshots: req.Screenshots, Logger: log,
	}
	for s := range queue {
		attempter.Driver = driver
		row, err := attempter.Run(ctx, Request{RunID: req.RunID, Problem: s.problem, Attempt: s.attempt})
		if errors.Is(err, domain.ErrDriverDied) && ctx.Err() == nil {
			log.WarnContext(ctx, "the browser session ended; starting another and trying again", "task", s.problem.TaskID)
			if err := start(); err != nil {
				return err
			}
			attempter.Driver = driver
			row, err = attempter.Run(ctx, Request{RunID: req.RunID, Problem: s.problem, Attempt: s.attempt})
			if errors.Is(err, domain.ErrDriverDied) && ctx.Err() == nil {
				log.ErrorContext(ctx, "the browser session ended again; recording the attempt as failed", "task", s.problem.TaskID)
				row = domain.Result{
					RunID: req.RunID, TaskID: s.problem.TaskID, Level: s.problem.Level, Attempt: s.attempt,
					Status: domain.StatusUIError, ErrorClass: ClassDriverDied,
				}
				err = start() // so the next slot has a browser
				if err != nil {
					return errors.Join(r.Store.Append(ctx, req.RunID, row), err)
				}
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil // interrupted: not recorded, so --resume asks it again
			}
			return fmt.Errorf("worker %d: %s attempt %d: %w", id, s.problem.TaskID, s.attempt, err)
		}
		if err := r.Store.Append(ctx, req.RunID, row); err != nil {
			return fmt.Errorf("recording %s attempt %d: %w", s.problem.TaskID, s.attempt, err)
		}
		n := prog.done.Add(1)
		log.InfoContext(ctx, "attempt recorded", "progress", fmt.Sprintf("%d/%d", n, prog.total), "task", s.problem.TaskID, "attempt", s.attempt, "status", row.Status)
	}
	return nil
}

// closeDriver ends a browser session, on a context that outlives a cancelled run.
func closeDriver(ctx context.Context, log *slog.Logger, d resource.UIDriver) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeWait)
	defer cancel()
	if err := d.Close(closeCtx); err != nil {
		log.Warn("could not close the browser cleanly", "error", err)
	}
}

func (r *Runner) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
