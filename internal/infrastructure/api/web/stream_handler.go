package web

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
)

const (
	defaultFlushInterval = 50 * time.Millisecond
	keepAliveEvery       = 15 * time.Second
	// renderCostFactor keeps updates at least this many times as far apart as
	// the last one took to make and send: the render, the write and the flush.
	// So rendering stays below a quarter of one core per stream, and a client
	// that is slow to take an update gets them further apart.
	renderCostFactor = 4
	// reconnectAfter is how soon a browser retries a reply that another
	// connection is still writing.
	reconnectAfter = 1500 * time.Millisecond

	replyUnavailable = "This reply isn't available."
	sessionEnded     = "Your session has ended. Reload the page to sign in again."
)

// streamHandler sends a reply to the browser as it is written.
//
// It always answers 200 with an event stream, and reports problems inside the
// stream: the htmx event-stream extension retries any HTTP error forever.
//
// Each update carries the whole reply so far as ready-to-swap HTML. Updates are
// coalesced to one per flush interval, with a trailing flush so the last text
// is never left waiting for the next event. The stream ends with a "done" event
// that makes the browser close the connection.
//
// Pattern: Three Contexts
func (a *App) streamHandler(w http.ResponseWriter, r *http.Request) {
	ref, ok := messageRefOf(w, r)
	if !ok {
		return
	}

	sse, err := newSSEWriter(w)
	if err != nil {
		return // the client is already gone
	}
	if a.streams.Err() != nil {
		_ = sse.retry(reconnectAfter) // shutting down: try again, the next server will answer
		return
	}

	// The client leaving ends the stream and the run. The server shutting down
	// ends only the run: the stream stays up long enough to tell the browser.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel() // leaving for any reason stops the agent run and saves the partial reply
	runCtx, endRun := context.WithCancel(ctx)
	defer endRun()
	defer context.AfterFunc(a.streams, endRun)()

	a.pump(ctx, sse, ref.convID, ref.msgID, a.svc.Chat.Stream(runCtx, ref.userID, ref.convID, ref.msgID))
}

type streamItem struct {
	ev  service.StreamEvent
	err error
}

// feed runs the use case's iterator in a goroutine of its own, so that pump can
// also watch the clock, and hands over what it yields.
//
// Pattern: Iterator Behind a Channel
func (a *App) feed(ctx context.Context, msgID uuid.UUID, events iter.Seq2[service.StreamEvent, error]) <-chan streamItem {
	items := make(chan streamItem)
	go func() {
		defer close(items)
		// This goroutine is out of the recovery middleware's reach, and a panic in
		// it would end the process and every other user's stream. End this one.
		defer func() {
			if rec := recover(); rec != nil {
				logPanic(ctx, a.log, rec, "message_id", msgID)
				select {
				case items <- streamItem{err: fmt.Errorf("the reply stream panicked: %v", rec)}:
				case <-ctx.Done():
				}
			}
		}()
		for ev, err := range events {
			select {
			case items <- streamItem{ev, err}:
			case <-ctx.Done():
				return // returning stops the iterator, which stops the agent
			}
			if err != nil {
				return
			}
		}
	}()
	return items
}

// pacer decides when the reply so far is sent. The first update goes at once;
// after that a new one waits until the interval since the last has passed, and
// carries the newest text by then. The interval is the flush interval, or longer
// while an update is slow to make or to send (see nextInterval).
//
// Pattern: Pacer
type pacer struct {
	base, interval time.Duration
	text           string
	lastSent       time.Time
	pending        bool // text has come that has not been sent
	armed          bool // the timer is running
	timer          *time.Timer
}

func newPacer(base time.Duration) *pacer {
	if base <= 0 {
		base = defaultFlushInterval
	}
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	return &pacer{base: base, interval: base, timer: timer}
}

// update takes the newest text and reports whether to send it now. When it is too
// soon, the timer is set to fire when it is not.
func (p *pacer) update(text string) (sendNow bool) {
	p.text, p.pending = text, true
	wait := p.interval - time.Since(p.lastSent)
	if wait <= 0 {
		return true
	}
	if !p.armed {
		p.timer.Reset(wait)
		p.armed = true
	}
	return false
}

// due reports whether text is waiting when the timer fires.
func (p *pacer) due() bool {
	p.armed = false
	return p.pending
}

// sent records that the text went out, and what the update cost: the time it
// took to render it, write it and flush it.
func (p *pacer) sent(cost time.Duration) {
	p.pending, p.armed = false, false
	p.timer.Stop()
	p.lastSent = time.Now()
	p.interval = nextInterval(p.base, cost)
}

// pump sends the events of a reply to the browser until the reply is done, the
// client leaves, or the use case fails. It is the one place that decides when an
// update is sent (see pacer); the use case only says what the reply is so far.
func (a *App) pump(ctx context.Context, sse *sseWriter, convID, msgID uuid.UUID, events iter.Seq2[service.StreamEvent, error]) {
	items := a.feed(ctx, msgID, events)
	pace := newPacer(a.opts.FlushInterval)
	defer pace.timer.Stop()
	keepAlive := time.NewTicker(keepAliveEvery)
	defer keepAlive.Stop()

	// send renders the reply so far and sends it, and reports whether the stream
	// goes on. It ends for a reply that cannot be rendered, which is a fault of
	// the page and is logged as one, and for a write that fails, which is a
	// client that went away. Leaving then ends the run, and the reply is saved
	// with the text so far (see streamHandler).
	send := func() bool {
		started := time.Now()
		html, err := a.replyHTML(convID, msgID, domain.StatusStreaming, pace.text, "", domain.Meta{})
		if err != nil {
			a.log.ErrorContext(ctx, "rendering a reply failed", "message_id", msgID, "error", err)
			return false
		}
		if err := sse.event("delta", html); err != nil {
			a.log.DebugContext(ctx, "stream ended: client went away", "error", err)
			return false
		}
		pace.sent(time.Since(started))
		return true
	}

	for {
		select {
		case it, ok := <-items:
			if !ok {
				return // the use case ended without a final event; nothing more to say
			}
			if it.err != nil {
				a.streamFailed(ctx, sse, convID, msgID, pace.text, it.err)
				return
			}
			switch it.ev.Kind {
			case service.StreamSnapshot:
				if pace.update(it.ev.Text) && !send() {
					return
				}
			case service.StreamDone:
				a.finishStream(ctx, sse, convID, msgID, it.ev)
				return
			}
		case <-pace.timer.C:
			if pace.due() && !send() {
				return
			}
		case <-keepAlive.C:
			if err := sse.comment("keep-alive"); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// nextInterval spaces updates out at least renderCostFactor times the cost of
// the last one, so a long reply cannot make rendering eat the server. The cost
// is the render, the write and the flush together, so a client that is slow to
// take an update gets the next ones further apart.
//
// Pattern: Cost-Coupled Interval
func nextInterval(base, cost time.Duration) time.Duration {
	return max(base, renderCostFactor*cost)
}

// finishStream sends the final state of the reply and closes the stream.
func (a *App) finishStream(ctx context.Context, sse *sseWriter, convID, msgID uuid.UUID, done service.StreamEvent) {
	html, err := a.replyHTML(convID, msgID, done.Status, done.Text, done.Error, done.Meta)
	if err != nil {
		a.log.ErrorContext(ctx, "rendering a reply failed", "message_id", msgID, "error", err)
		return
	}
	if err := sse.event("delta", html); err != nil {
		return
	}
	_ = sse.event("done", "done")
}

// streamFailed reports a problem inside the stream. A reply that another
// connection is writing is retried later; anything else ends with an error
// shown in the reply's place.
//
// Pattern: In-Stream Failure
func (a *App) streamFailed(ctx context.Context, sse *sseWriter, convID, msgID uuid.UUID, text string, err error) {
	if errors.Is(err, domain.ErrAlreadyStreaming) {
		_ = sse.retry(reconnectAfter) // then the stream simply ends and the browser reconnects
		return
	}
	reason := domain.FailureUnknown
	if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrInvalidState) {
		reason = replyUnavailable
	} else {
		a.log.ErrorContext(ctx, "streaming a reply failed", "message_id", msgID, "error", err)
	}
	html, renderErr := a.replyHTML(convID, msgID, domain.StatusFailed, text, reason, domain.Meta{})
	if renderErr != nil {
		a.log.ErrorContext(ctx, "rendering a reply failed", "message_id", msgID, "error", renderErr)
		return
	}
	if sse.event("delta", html) == nil {
		_ = sse.event("done", "done")
	}
}

// replyHTML renders the assistant article for a reply in the given state.
func (a *App) replyHTML(convID, msgID uuid.UUID, status domain.Status, text, errText string, meta domain.Meta) (string, error) {
	return a.renderer.Block("partial:assistant", a.messageView(domain.Message{
		ID: msgID, ConversationID: convID, Role: domain.RoleAssistant,
		Status: status, Content: text, Error: errText, Meta: meta,
	}))
}

// wantsEventStream reports whether the client is an EventSource.
func wantsEventStream(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}
