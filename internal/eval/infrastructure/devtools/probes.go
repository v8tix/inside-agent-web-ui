package devtools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/probe"
)

// How long the probes wait for the page.
const (
	replyStarts = 30 * time.Second // for a reply to show its first text
	replyEnds   = 60 * time.Second // for a reply to reach a final status
	stopEnds    = 10 * time.Second // for Stop to take effect
	tidyWait    = 20 * time.Second // for a probe's conversation to be deleted
)

// ProbePrompts are what the probes say to the agent.
type ProbePrompts struct {
	// Long asks for a reply that takes seconds to write, so that there is time to
	// stop it or reload the page in the middle.
	Long string
	// Short asks for a reply of a word or two, for the probes that need an
	// answer and nothing more.
	Short string
	// Flaky asks for a reply that fails the first time and succeeds the second,
	// which only the scripted agent can give. Empty means no such agent, and the
	// probe that needs one is not run.
	Flaky string
}

// Probes carries out the UI probes in a browser, one conversation at a time.
// It opens its browser on first use and closes it with Close.
type Probes struct {
	Factory         *Factory
	Email, Password string
	Prompts         ProbePrompts
	Thresholds      domain.Thresholds
	// KeepChats leaves the conversations the probes start in place.
	KeepChats bool
	Logger    *slog.Logger

	d *Driver
}

var _ resource.UIProbes = (*Probes)(nil)

func (p *Probes) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// driver returns the browser the probes work in, signed in.
func (p *Probes) driver(ctx context.Context) (*Driver, error) {
	if p.d != nil {
		return p.d, nil
	}
	d, err := p.Factory.Open(ctx)
	if err != nil {
		return nil, err
	}
	if err := d.Login(ctx, p.Email, p.Password); err != nil {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = d.Close(closeCtx)
		return nil, err
	}
	p.d = d
	return d, nil
}

// Close ends the browser session.
func (p *Probes) Close(ctx context.Context) error {
	if p.d == nil {
		return nil
	}
	err := p.d.Close(ctx)
	p.d = nil
	return err
}

// tidy deletes the conversation a probe started, unless asked to keep it. The
// outcome of a probe does not depend on it. A probe defers it before the first
// step that can start a conversation, so that it runs however the probe ends.
func (p *Probes) tidy(d *Driver) { p.tidyChat(d, d.Conversation()) }

// tidyChat is tidy for one of the conversations a probe started, which the driver
// is no longer showing. A probe can end with the page in no state to delete
// from: a row whose title is being typed has no delete button. So a deletion
// that fails is tried once more, on the page loaded again.
func (p *Probes) tidyChat(d *Driver, conv string) {
	if p.KeepChats || conv == "" {
		return
	}
	d.conversation = conv
	ctx, cancel := context.WithTimeout(context.Background(), tidyWait)
	defer cancel()
	err := d.DeleteChat(ctx)
	if err != nil && d.Reload(ctx) == nil {
		err = d.DeleteChat(ctx)
	}
	if err != nil {
		p.log().Warn("could not delete a probe's conversation", "error", err)
	}
}

// ask starts a conversation with the prompt and waits for the reply to end.
func (p *Probes) ask(ctx context.Context, d *Driver, prompt string) (domain.Reply, error) {
	if err := d.StartChat(ctx); err != nil {
		return domain.Reply{}, err
	}
	return p.say(ctx, d, prompt)
}

// say sends a message in the current conversation and waits for the reply to end.
func (p *Probes) say(ctx context.Context, d *Driver, prompt string) (domain.Reply, error) {
	if err := d.Submit(ctx, prompt); err != nil {
		return domain.Reply{}, err
	}
	return awaitFor(ctx, d, replyEnds)
}

func awaitFor(ctx context.Context, d *Driver, within time.Duration) (domain.Reply, error) {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	return d.AwaitReply(ctx)
}

// timedOut reports whether a wait for the page ended because its own time ran
// out, with ctx (the probe's) still going. When the wait is for something the
// page is meant to do, the page not doing it is what the probe came to find;
// when ctx has ended too, the run was stopped, and nothing was found.
func timedOut(ctx context.Context, err error) bool {
	return errors.Is(err, domain.ErrReplyTimeout) && ctx.Err() == nil
}

// untilText waits until the newest reply shows text, or has ended without any.
func untilText(ctx context.Context, d *Driver, within time.Duration) (status domain.ReplyStatus, chars int, err error) {
	err = d.poll(ctx, within, func(ctx context.Context) (bool, error) {
		var found bool
		var err error
		status, chars, found, err = d.Progress(ctx)
		return err == nil && found && (chars > 0 || status.Terminal()), err
	})
	if err != nil {
		return status, chars, fmt.Errorf("waiting for the reply to start: %w", err)
	}
	return status, chars, nil
}

// repliesOf is what replies.js returns.
type repliesOf struct {
	Assistants []struct {
		ID     *string `json:"id"`
		Status string  `json:"status"`
		Chars  int     `json:"chars"`
		Text   string  `json:"text"`
	} `json:"assistants"`
	Users      int `json:"users"`
	Regenerate int `json:"regenerate"`
	Retry      int `json:"retry"`
}

func (r repliesOf) id(i int) string {
	if r.Assistants[i].ID == nil {
		return ""
	}
	return *r.Assistants[i].ID
}

func replies(ctx context.Context, d *Driver) (repliesOf, error) {
	var r repliesOf
	err := d.Eval(ctx, script("replies"), &r)
	return r, err
}

// --- P-01 -------------------------------------------------------------------------------

// Stop presses Stop in the middle of a reply and times how long it takes to work.
func (p *Probes) Stop(ctx context.Context) (domain.ProbeOutcome, error) {
	d, err := p.driver(ctx)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	defer p.tidy(d)

	if err := d.StartChat(ctx); err != nil {
		return domain.ProbeOutcome{}, err
	}
	if err := d.Submit(ctx, p.Prompts.Long); err != nil {
		return domain.ProbeOutcome{}, err
	}
	status, _, err := untilText(ctx, d, replyStarts)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	if status.Terminal() {
		return probe.JudgeStop(probe.StopMeasures{Finished: true}, p.Thresholds), nil
	}
	if err := d.Stop(ctx); err != nil {
		if !errors.Is(err, ErrNoStopButton) {
			return domain.ProbeOutcome{}, err
		}
		// A reply that ended as Stop was reached for has no button to show, and says
		// nothing of the page. One that is still being written has, so the reply is
		// looked at again to tell which. A page that no longer shows it is not judged.
		status, _, found, err := d.Progress(ctx)
		switch {
		case err != nil:
			return domain.ProbeOutcome{}, err
		case !found:
			return domain.ProbeOutcome{}, errors.New("there is no Stop button, and the reply is no longer on the page")
		}
		return probe.JudgeStop(probe.StopMeasures{Finished: status.Terminal(), NoButton: !status.Terminal()}, p.Thresholds), nil
	}
	reply, err := awaitFor(ctx, d, stopEnds)
	if timedOut(ctx, err) {
		// Pressed, and the reply was still being written when the time was up.
		return probe.JudgeStop(probe.StopMeasures{Status: reply.Status}, p.Thresholds), nil
	}
	if err != nil {
		return domain.ProbeOutcome{}, fmt.Errorf("after Stop: %w", err)
	}
	obs, err := d.Collect(ctx)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	raw, err := d.ReadRaw(ctx, reply)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}

	m := probe.StopMeasures{Status: reply.Status, KeptChars: len([]rune(raw))}
	if obs.StopAt == 0 {
		return domain.ProbeOutcome{}, errors.New("the click on Stop was not seen by the observer")
	}
	for _, s := range obs.Statuses {
		if s.Status == domain.ReplyStopped {
			m.StopMs = s.At - obs.StopAt
		}
	}
	for _, u := range obs.Updates {
		if u.At <= obs.StopAt {
			m.CharsBeforeStop = u.Chars
		}
	}
	return probe.JudgeStop(m, p.Thresholds), nil
}

// --- P-02 -------------------------------------------------------------------------------

// Regenerate asks for a new answer to the last message of a conversation of two
// replies, and checks that only that reply was replaced.
func (p *Probes) Regenerate(ctx context.Context) (domain.ProbeOutcome, error) {
	d, err := p.driver(ctx)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	defer p.tidy(d)

	if _, err := p.ask(ctx, d, p.Prompts.Short+" (first)"); err != nil {
		return domain.ProbeOutcome{}, err
	}
	if _, err := p.say(ctx, d, p.Prompts.Short+" (second)"); err != nil {
		return domain.ProbeOutcome{}, err
	}
	before, err := replies(ctx, d)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	if len(before.Assistants) != 2 {
		return domain.ProbeOutcome{}, fmt.Errorf("expected a conversation of two replies, found %d", len(before.Assistants))
	}

	d.ExpectNewReply(ctx)
	if err := d.Click(ctx, Query{Role: "button", Name: "Regenerate response"}); err != nil {
		return domain.ProbeOutcome{}, err
	}
	switch reply, err := awaitFor(ctx, d, replyEnds); {
	case timedOut(ctx, err) && reply.MessageID != "":
		// A new reply is there, and is still being written: that is the agent being slow.
		return probe.NotRun(domain.P02, "the new reply had not finished after a minute: the agent was too slow for this probe"), nil
	case err != nil && !timedOut(ctx, err):
		return domain.ProbeOutcome{}, err
	}
	// Nothing new after a minute is judged with the rest: the last reply was not replaced.
	after, err := replies(ctx, d)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}

	m := probe.RegenerateMeasures{RepliesBefore: len(before.Assistants), RepliesAfter: len(after.Assistants), ButtonsOffered: after.Regenerate}
	if len(after.Assistants) > 0 {
		last := len(after.Assistants) - 1
		m.FirstUnchanged = after.id(0) == before.id(0) && after.Assistants[0].Text == before.Assistants[0].Text
		m.LastReplaced = after.id(last) != before.id(len(before.Assistants)-1) && domain.ReplyStatus(after.Assistants[last].Status).Terminal()
	}
	return probe.JudgeRegenerate(m), nil
}

// --- P-05 -------------------------------------------------------------------------------

const renamedTitle = "Renamed by agent-eval"

// RenameDelete renames a conversation and deletes it, and checks that both
// hold after the page is loaded again.
func (p *Probes) RenameDelete(ctx context.Context) (domain.ProbeOutcome, error) {
	d, err := p.driver(ctx)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	// Deleting the conversation is the probe's own last step. Tidying is for when
	// it does not get that far.
	defer p.tidy(d)

	reply, err := p.ask(ctx, d, p.Prompts.Short)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	conv := reply.ConversationID
	var m probe.RenameDeleteMeasures

	if err := d.PressRowButton(ctx, RowRename, conv); err != nil {
		return domain.ProbeOutcome{}, err
	}
	if err := d.Fill(ctx, Query{Role: "textbox", Name: "Conversation title"}, renamedTitle); err != nil {
		return domain.ProbeOutcome{}, fmt.Errorf("typing the new title: %w", err)
	}
	if err := d.Click(ctx, Query{Role: "button", Name: "Save"}); err != nil {
		return domain.ProbeOutcome{}, fmt.Errorf("pressing Save: %w", err)
	}
	m.RenamedShown = d.waitTitle(ctx, conv, renamedTitle)
	if err := d.Reload(ctx); err != nil {
		return domain.ProbeOutcome{}, err
	}
	m.RenamedAfterReload = d.titleOf(ctx, conv) == renamedTitle

	if err := d.DeleteChat(ctx); err == nil {
		m.DeletedShown = true
	} else {
		p.log().Warn("the conversation was still listed after deleting it", "error", err)
	}
	// A fresh load of the start page lists what the server has.
	if err := d.StartChat(ctx); err != nil {
		return domain.ProbeOutcome{}, err
	}
	m.DeletedAfterReload = !d.rowExists(ctx, conv)
	return probe.JudgeRenameDelete(m), nil
}

// titleOf is the title a conversation has in the sidebar, "" when it is not there.
func (d *Driver) titleOf(ctx context.Context, conv string) string {
	var title *string
	if err := d.tab.eval(ctx, scriptWith("rowTitle", conv), &title); err != nil || title == nil {
		return ""
	}
	return *title
}

func (d *Driver) rowExists(ctx context.Context, conv string) bool {
	var exists bool
	return d.tab.eval(ctx, scriptWith("rowExists", conv), &exists) == nil && exists
}

// waitTitle waits for the sidebar to show a title for the conversation.
func (d *Driver) waitTitle(ctx context.Context, conv, want string) bool {
	return d.poll(ctx, d.settle(), func(ctx context.Context) (bool, error) {
		return d.titleOf(ctx, conv) == want, nil
	}) == nil
}

// --- P-07 -------------------------------------------------------------------------------

// ReloadMidStream loads the page again while a reply is being written.
func (p *Probes) ReloadMidStream(ctx context.Context) (domain.ProbeOutcome, error) {
	d, err := p.driver(ctx)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	defer p.tidy(d)

	if err := d.StartChat(ctx); err != nil {
		return domain.ProbeOutcome{}, err
	}
	if err := d.Submit(ctx, p.Prompts.Long); err != nil {
		return domain.ProbeOutcome{}, err
	}
	status, before, err := untilText(ctx, d, replyStarts)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	if status.Terminal() {
		return probe.NotRun(domain.P07, "the reply finished before the page could be reloaded: the agent was too fast for this probe"), nil
	}
	if err := d.Reload(ctx); err != nil {
		return domain.ProbeOutcome{}, err
	}

	m := probe.ReloadMeasures{CharsBefore: before}
	waitCtx, cancel := context.WithTimeout(ctx, replyEnds)
	defer cancel()
	for {
		r, err := replies(waitCtx, d)
		if err == nil {
			m.UserMessages, m.AssistantMessages = r.Users, len(r.Assistants)
			if len(r.Assistants) > 0 {
				last := r.Assistants[len(r.Assistants)-1]
				m.CharsAfter = last.Chars
				if domain.ReplyStatus(last.Status).Terminal() {
					m.Ended = true
					break
				}
			}
		} else if ctx.Err() != nil || errors.Is(err, domain.ErrDriverDied) {
			return domain.ProbeOutcome{}, err
		}
		// A reply still not over when the time is up is what the probe is looking
		// for, whether the time ran out in a pause or in the middle of a look.
		if sleep(waitCtx, d.interval()) != nil {
			break
		}
	}
	return probe.JudgeReload(m), nil
}

// --- P-10 -------------------------------------------------------------------------------

// AgentDown asks for a reply that the scripted agent fails to give the first
// time, and checks that the page says so and that Retry gets the answer.
func (p *Probes) AgentDown(ctx context.Context) (domain.ProbeOutcome, error) {
	if p.Prompts.Flaky == "" {
		return probe.NotRun(domain.P10, "needs the scripted agent, which can be told to fail once: this agent cannot"), nil
	}
	d, err := p.driver(ctx)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	defer p.tidy(d)

	// The scripted agent fails a question the first time it is asked, and it
	// remembers: a wording of its own makes this run's first time its own.
	reply, err := p.ask(ctx, d, p.Prompts.Flaky+" #"+nonce())
	if err != nil && !timedOut(ctx, err) {
		return domain.ProbeOutcome{}, err
	}
	// A failure the scripted agent gives at once, and the page still shows nothing
	// final a minute later, is a failure the page did not show.
	m := probe.AgentDownMeasures{FailedShown: reply.Status == domain.ReplyFailed, MessageShown: reply.Error != ""}
	r, err := replies(ctx, d)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	m.RetryOffered = r.Retry == 1
	if m.RetryOffered {
		d.ExpectNewReply(ctx)
		if err := d.Click(ctx, Query{Role: "button", Name: "Retry"}); err != nil {
			return domain.ProbeOutcome{}, err
		}
		retried, err := awaitFor(ctx, d, replyEnds)
		if err != nil && !timedOut(ctx, err) {
			return domain.ProbeOutcome{}, err
		}
		m.RetryAnswered = retried.Status == domain.ReplyComplete
	}
	return probe.JudgeAgentDown(m), nil
}

// nonce is a few characters made up for the occasion.
func nonce() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// --- the audits --------------------------------------------------------------------------

// The audits are in audits.go.
