// Package run asks problems through the browser and records what happened.
package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/score"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/uicheck"
)

// Error classes a row can carry, naming the step that went wrong. They are
// fixed words: the text of a driver error may hold the page's or the user's
// words, so it goes to the log and not to the results.
const (
	ClassStartChat     = "start_chat"
	ClassSubmit        = "submit"
	ClassAwaitReply    = "await_reply"
	ClassReadRaw       = "read_raw"
	ClassCollect       = "collect_failed"
	ClassAgentFailed   = "agent_failed"
	ClassTimeout       = "timeout"
	ClassUnexpectedEnd = "unexpected_end"
)

// defaultStopWait is how long Stop gets to take effect after a timeout.
const defaultStopWait = 10 * time.Second

// Request is one try at one problem.
type Request struct {
	RunID   string
	Problem domain.Problem
	// Attempt counts from 1: the n-th try at this problem in the run.
	Attempt int
}

// Attempter makes attempts through one driver, one at a time. Like the driver
// it holds, it is not safe for concurrent use.
//
// An attempt follows the flow of the spec: open a new conversation, type the
// prompt, wait for the reply (pressing Stop when time runs out), read the reply
// as stored, take the final answer from it, score it, then read what the
// browser observed and judge the UI checks. Whatever goes wrong becomes a row
// with a status; only a dead driver or a cancelled context is an error, because
// then the run cannot go on as it was.
//
// Pattern: Failures Become Rows
type Attempter struct {
	Driver resource.UIDriver
	// Store receives the screenshots of failed attempts.
	Store      resource.ResultStore
	Prompt     score.Prompt // the zero value means the built-in prompt
	Thresholds domain.Thresholds
	// Timeout is how long the reply may take before Stop is pressed.
	Timeout time.Duration
	// StopWait is how long Stop gets to take effect; zero means ten seconds.
	StopWait time.Duration
	// KeepChats leaves the conversation in place afterwards.
	KeepChats bool
	// IncludeText puts question, truth and answer text in the row.
	IncludeText bool
	// Screenshots keeps a picture of the page when something is wrong with it.
	// The picture shows the question and the reply.
	Screenshots bool
	Logger      *slog.Logger
}

// Run makes one attempt and returns its row.
func (a *Attempter) Run(ctx context.Context, req Request) (domain.Result, error) {
	p := req.Problem
	switch {
	case a.Timeout <= 0:
		return domain.Result{}, errors.New("the attempt timeout must be positive")
	case req.Attempt < 1:
		return domain.Result{}, fmt.Errorf("attempt %d of %s: attempts start at 1", req.Attempt, p.TaskID)
	case p.Unscored:
		return domain.Result{}, fmt.Errorf("problem %s has no answer to score against: it should have been skipped", p.TaskID)
	}
	if err := p.Valid(); err != nil {
		return domain.Result{}, err
	}

	t := &try{
		Attempter: a,
		req:       req,
		log:       a.logger().With("task", p.TaskID, "attempt", req.Attempt),
		row:       domain.Result{RunID: req.RunID, TaskID: p.TaskID, Level: p.Level, Attempt: req.Attempt},
	}
	if a.IncludeText {
		t.row.Question, t.row.Truth = p.Question, p.Truth
	}
	if err := t.run(ctx); err != nil {
		return domain.Result{}, err
	}
	t.log.InfoContext(ctx, "attempt finished", "status", t.row.Status, "class", t.row.ErrorClass)
	return t.row, nil
}

func (a *Attempter) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// try is the state of one attempt in progress.
type try struct {
	*Attempter
	req Request
	log *slog.Logger
	row domain.Result

	opened   bool // a conversation was started
	timedOut bool
	reply    domain.Reply
	answer   string // the extracted final answer, when there is one
	findings []uicheck.Finding
}

func (t *try) run(ctx context.Context) error {
	submitted, err := t.open(ctx)
	if err != nil {
		return err
	}
	if submitted {
		if err := t.await(ctx); err != nil {
			return err
		}
		if t.row.Status == "" {
			if err := t.grade(ctx); err != nil {
				return err
			}
		}
		if err := t.observe(ctx); err != nil {
			return err
		}
	}
	t.keepEvidence(ctx)
	t.tidy(ctx)
	return nil
}

// fatal tells a failure the run cannot go on after (the driver died, or the
// run was cancelled) from one that is only this attempt's outcome. It returns
// the error in the first case and nil in the second.
func fatal(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, domain.ErrDriverDied):
		return err
	}
	return nil
}

// broke records that the page failed this attempt.
func (t *try) broke(class string, err error) {
	t.log.Warn("the page failed the attempt", "step", class, "error", err)
	t.row.Status, t.row.ErrorClass = domain.StatusUIError, class
}

// open starts a conversation and types the prompt. submitted is false when
// either failed, in which case the row says so.
func (t *try) open(ctx context.Context) (submitted bool, _ error) {
	if err := t.Driver.StartChat(ctx); err != nil {
		if f := fatal(ctx, err); f != nil {
			return false, f
		}
		t.broke(ClassStartChat, err)
		return false, nil
	}
	t.opened = true

	prompt := t.Prompt
	if prompt == (score.Prompt{}) {
		prompt = score.DefaultPrompt()
	}
	if err := t.Driver.Submit(ctx, prompt.Compose(t.req.Problem.Question)); err != nil {
		if f := fatal(ctx, err); f != nil {
			return false, f
		}
		t.broke(ClassSubmit, err)
		return false, nil
	}
	return true, nil
}

// await waits for the reply to reach a final status. When time runs out it
// presses Stop, so the UI gets the chance to show it can end a reply.
func (t *try) await(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	reply, err := t.Driver.AwaitReply(waitCtx)
	cancel()
	t.reply = reply

	switch {
	case err == nil:
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, domain.ErrDriverDied):
		return err
	case errors.Is(err, domain.ErrReplyTimeout):
		t.timedOut = true
		t.row.Status, t.row.ErrorClass = domain.StatusTimeout, ClassTimeout
		t.log.Warn("the reply did not finish in time; pressing Stop")
		return t.stop(ctx)
	default:
		t.broke(ClassAwaitReply, err)
		return nil
	}

	switch t.reply.Status {
	case domain.ReplyComplete:
	case domain.ReplyFailed:
		t.row.Status, t.row.ErrorClass = domain.StatusAgentError, ClassAgentFailed
	default:
		// Stopped by nobody we know of, or not final although the wait is over.
		t.broke(ClassUnexpectedEnd, fmt.Errorf("the reply ended as %q", t.reply.Status))
	}
	return nil
}

// stop presses Stop after a timeout and waits to see the reply end. A Stop that
// does not work is not this attempt's status (the agent was slow); the missing
// final status shows up in check U-03.
func (t *try) stop(ctx context.Context) error {
	wait := t.StopWait
	if wait <= 0 {
		wait = defaultStopWait
	}
	stopCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	if err := t.Driver.Stop(stopCtx); err != nil {
		if f := fatal(ctx, err); f != nil {
			return f
		}
		t.log.Warn("Stop did not work", "error", err)
		return nil
	}
	final, err := t.Driver.AwaitReply(stopCtx)
	if err != nil {
		if f := fatal(ctx, err); f != nil {
			return f
		}
		t.log.Warn("the reply did not end after Stop", "error", err)
		return nil
	}
	t.reply = final
	return nil
}

// grade reads the reply as stored, takes the final answer from it and scores it.
func (t *try) grade(ctx context.Context) error {
	raw, err := t.Driver.ReadRaw(ctx, t.reply)
	if err != nil {
		if f := fatal(ctx, err); f != nil {
			return f
		}
		t.broke(ClassReadRaw, err)
		return nil
	}
	answer, found := score.Extract(raw)
	if t.IncludeText {
		t.row.Answer = score.Answer(raw)
	}
	if !found {
		t.row.Status = domain.StatusNoFinalAnswer
		return nil
	}
	t.answer = answer
	if score.Question(answer, t.req.Problem.Truth) {
		t.row.Status, t.row.Score = domain.StatusCorrect, 1
	} else {
		t.row.Status = domain.StatusIncorrect
	}
	return nil
}

// observe reads what the browser saw and judges the UI checks. It runs for every
// attempt that got as far as typing the prompt, because a page that broke has
// often said why in its console.
func (t *try) observe(ctx context.Context) error {
	obs, err := t.Driver.Collect(ctx)
	if err != nil {
		if f := fatal(ctx, err); f != nil {
			return f
		}
		t.log.Warn("could not read what the browser observed", "error", err)
		if t.row.ErrorClass == "" {
			t.row.ErrorClass = ClassCollect
		}
		return nil
	}

	t.fillMetrics(obs)
	t.findings = uicheck.Evaluate(uicheck.Input{Obs: obs, Answer: t.answer, TimedOut: t.timedOut, Thresholds: t.Thresholds, IncludeText: t.IncludeText})
	t.row.UIChecks = uicheck.Results(t.findings)
	for _, f := range uicheck.Failed(t.findings) {
		t.log.Warn("a UI check failed", "check", f.ID, "detail", f.Detail)
		t.row.Notes = append(t.row.Notes, fmt.Sprintf("%s: %s", f.ID, f.Detail))
	}
	return nil
}

func (t *try) fillMetrics(obs domain.UIObservations) {
	r := &t.row
	r.ConversationID = t.reply.ConversationID
	if r.Status.Answered() || r.Status == domain.StatusAgentError { // a reply that ended by itself
		if v, ok := obs.Latency(); ok {
			r.LatencyMs = millis(v)
		}
	}
	if v, ok := obs.TTFTUI(); ok {
		r.TTFTUIMs = millis(v)
	}
	if v := obs.Message.TTFTMs; v != nil {
		r.TTFTAgentMs = new(*v)
	}
	if v, ok := obs.UIOverhead(); ok {
		r.UIOverheadMs = millis(v)
	}
	if v, ok := obs.MaxGap(); ok {
		r.MaxGapMs = millis(v)
	}
	r.Updates = len(obs.Updates)
	if v := obs.Message.PromptTokens; v != nil {
		r.Tokens.Prompt = *v
	}
	if v := obs.Message.CompletionTokens; v != nil {
		r.Tokens.Completion = *v
	}
	r.ToolCalls = slices.Clone(obs.Message.ToolCalls)
	r.ConsoleErrors = obs.ConsoleErrors()
	r.FailedRequests = obs.FailedRequests()
}

// millis rounds a time on the page's clock to a whole number of milliseconds.
func millis(v float64) *int64 { return new(int64(math.Round(v))) }

// unsafeInName matches what a file name made from a task id must not keep. Dots
// go too, so the name can neither start with one nor hold "..".
var unsafeInName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// keepEvidence takes a screenshot when the attempt shows something worth
// seeing: a page that broke, a reply that failed or never finished, or a UI
// check that failed. A wrong answer is the agent's affair, not the page's.
func (t *try) keepEvidence(ctx context.Context) {
	var reason string
	switch {
	case t.row.Status == domain.StatusUIError:
		reason = "ui-error"
	case t.row.Status == domain.StatusTimeout:
		reason = "timeout"
	case t.row.Status == domain.StatusAgentError:
		reason = "agent-error"
	case len(uicheck.Failed(t.findings)) > 0:
		reason = "ui-check"
	default:
		return
	}
	if !t.opened || t.Store == nil || !t.Screenshots {
		return // there is no page worth a picture, nowhere to put it, or it is not to be kept
	}
	name := fmt.Sprintf("%s-%d-%s.png", unsafeInName.ReplaceAllString(t.req.Problem.TaskID, "_"), t.req.Attempt, reason)
	path, err := t.Store.EvidencePath(ctx, t.req.RunID, name)
	if err != nil {
		t.log.Warn("no place for the screenshot", "error", err)
		return
	}
	if err := t.Driver.Screenshot(ctx, path); err != nil {
		t.log.Warn("could not take the screenshot", "error", err)
		return
	}
	t.row.Evidence = append(t.row.Evidence, "evidence/"+name)
}

// tidy deletes the conversation, so a long run does not fill the sidebar and
// the next attempt starts clean. A failure here does not change the attempt.
func (t *try) tidy(ctx context.Context) {
	if t.KeepChats || !t.opened {
		return
	}
	if err := t.Driver.DeleteChat(ctx); err != nil {
		t.log.Warn("could not delete the conversation", "error", err)
	}
}
