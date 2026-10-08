package web

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service/mocks"
)

var streamMsgID = uuid.NewV7()

type sseFrame struct {
	Event, Data, Comment, Retry string
	At                          time.Duration // when it was flushed, if the recorder knows
}

func streamPath() string { return "/c/" + convID.String() + "/m/" + streamMsgID.String() + "/stream" }

// flushRecorder records when each chunk was flushed. Every write the SSE writer
// makes is flushed at once, so chunk i is frame i.
type flushRecorder struct {
	*httptest.ResponseRecorder
	start   time.Time
	flushes []time.Duration
	sizes   []int
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{ResponseRecorder: httptest.NewRecorder(), start: time.Now()}
}

func (f *flushRecorder) Flush() {
	f.flushes = append(f.flushes, time.Since(f.start))
	f.sizes = append(f.sizes, f.Body.Len())
	f.ResponseRecorder.Flush()
}

// frames splits the body into frames and stamps each with its flush time.
func (f *flushRecorder) frames() []sseFrame {
	body := f.Body.String()
	var out []sseFrame
	prev := 0
	for i, size := range f.sizes {
		chunk := body[prev:size]
		prev = size
		fr := parseOneFrame(strings.TrimSuffix(chunk, "\n\n"))
		fr.At = f.flushes[i]
		out = append(out, fr)
	}
	return out
}

func parseOneFrame(chunk string) sseFrame {
	var fr sseFrame
	var data []string
	for line := range strings.SplitSeq(chunk, "\n") {
		switch {
		case strings.HasPrefix(line, ": "):
			fr.Comment = strings.TrimPrefix(line, ": ")
		case strings.HasPrefix(line, "event: "):
			fr.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		case strings.HasPrefix(line, "retry: "):
			fr.Retry = strings.TrimPrefix(line, "retry: ")
		}
	}
	fr.Data = strings.Join(data, "\n")
	return fr
}

func parseSSE(body string) []sseFrame {
	var out []sseFrame
	for chunk := range strings.SplitSeq(strings.TrimSpace(body), "\n\n") {
		out = append(out, parseOneFrame(chunk))
	}
	return out
}

func deltas(frames []sseFrame) []sseFrame {
	var out []sseFrame
	for _, f := range frames {
		if f.Event == "delta" {
			out = append(out, f)
		}
	}
	return out
}

// plan items for scripted: a time.Duration sleeps, a StreamEvent is yielded and
// an error ends the stream.
func scripted(stopped *bool, items ...any) iter.Seq2[service.StreamEvent, error] {
	return func(yield func(service.StreamEvent, error) bool) {
		for _, it := range items {
			switch v := it.(type) {
			case time.Duration:
				time.Sleep(v)
			case service.StreamEvent:
				if !yield(v, nil) {
					if stopped != nil {
						*stopped = true
					}
					return
				}
			case error:
				yield(service.StreamEvent{}, v)
				return
			}
		}
	}
}

func snap(text string) service.StreamEvent {
	return service.StreamEvent{Kind: service.StreamSnapshot, Text: text}
}

func done(status domain.Status, text string) service.StreamEvent {
	return service.StreamEvent{Kind: service.StreamDone, Status: status, Text: text}
}

func (h *chatHarness) expectStream(seq iter.Seq2[service.StreamEvent, error]) {
	h.chat.On("Stream", mock.Anything, alice.ID, convID, streamMsgID).Return(seq)
}

func (h *chatHarness) stream(rec http.ResponseWriter, ctx context.Context) {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, streamPath(), nil).WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	h.route.ServeHTTP(rec, req)
}

func TestStream_Basics(t *testing.T) {
	h := newChatHarness(t)
	meta := domain.Meta{TTFTMs: 120, DurationMs: 3400, Usage: domain.Usage{Prompt: 3, Completion: 5}, ToolCalls: []string{"lookup"}}
	end := done(domain.StatusComplete, "Hello **world**")
	end.Meta = meta
	h.expectStream(scripted(nil, snap("Hello"), end))
	rec := httptest.NewRecorder()

	h.stream(rec, t.Context())

	t.Run("it is an event stream that nothing may buffer", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
		assert.Equal(t, "no", rec.Header().Get("X-Accel-Buffering"))
		assert.True(t, strings.HasPrefix(rec.Body.String(), ": ok\n\n"), "the stream opens at once")
	})

	frames := parseSSE(rec.Body.String())
	t.Run("it ends with the final reply and a done event", func(t *testing.T) {
		require.GreaterOrEqual(t, len(frames), 3)
		last, prev := frames[len(frames)-1], frames[len(frames)-2]
		assert.Equal(t, "done", last.Event)
		assert.Equal(t, "done", last.Data, "an event needs data, or the browser drops it")
		assert.Equal(t, "delta", prev.Event)

		root := parseHTML(t, prev.Data)
		article := byTestID(root, "message")
		require.Len(t, article, 1)
		assert.Equal(t, "complete", attrOf(article[0], "data-status"))
		assert.Equal(t, streamMsgID.String(), attrOf(article[0], "data-id"))
		assert.Len(t, elementsOf(article[0], "strong"), 1, "the final text is rendered Markdown")
		assert.Equal(t, "120", attrOf(article[0], "data-ttft-ms"))
		assert.Equal(t, "3400", attrOf(article[0], "data-duration-ms"))
		assert.Equal(t, "3", attrOf(article[0], "data-prompt-tokens"))
		assert.Equal(t, "5", attrOf(article[0], "data-completion-tokens"))
		assert.Equal(t, "lookup", attrOf(article[0], "data-tool-calls"))
	})

	t.Run("updates during the run show a streaming reply", func(t *testing.T) {
		first := deltas(frames)[0]
		article := byTestID(parseHTML(t, first.Data), "message")[0]
		assert.Equal(t, "streaming", attrOf(article, "data-status"))
		assert.Contains(t, textContent(article), "Hello")
		assert.False(t, hasAttr(article, "data-ttft-ms"), "no metadata until the reply is finished")
	})
}

func TestStream_CoalescesBurstsAndFlushesTheTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newChatHarnessWithOptions(t, Options{FlushInterval: 100 * time.Millisecond})
		h.expectStream(scripted(nil,
			snap("a"),
			10*time.Millisecond, snap("ab"),
			10*time.Millisecond, snap("abc"),
			500*time.Millisecond, snap("abcd"),
			10*time.Millisecond, done(domain.StatusComplete, "abcd"),
		))
		rec := newFlushRecorder()

		h.stream(rec, t.Context())

		var shown []string
		var at []time.Duration
		for _, f := range deltas(rec.frames()) {
			shown = append(shown, strings.TrimSpace(textContent(byTestID(parseHTML(t, f.Data), "message-body")[0])))
			at = append(at, f.At)
		}
		assert.Equal(t, []string{"a", "abc", "abcd", "abcd"}, shown,
			"the burst 'ab','abc' is one update, and nothing waits for the next event to be shown")
		assert.Equal(t, []time.Duration{0, 100 * time.Millisecond, 520 * time.Millisecond, 530 * time.Millisecond}, at,
			"the first at once, the tail one interval later, the next straight away because the interval had passed, then the final")
	})
}

func TestStream_KeepsIdleConnectionsAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newChatHarness(t)
		h.expectStream(scripted(nil, 40*time.Second, done(domain.StatusComplete, "late")))
		rec := newFlushRecorder()

		h.stream(rec, t.Context())

		var beats []time.Duration
		for _, f := range rec.frames() {
			if f.Comment == "keep-alive" {
				beats = append(beats, f.At)
			}
		}
		assert.Equal(t, []time.Duration{15 * time.Second, 30 * time.Second}, beats)
	})
}

func TestStream_ReplaysAFinishedReply(t *testing.T) {
	h := newChatHarness(t)
	h.expectStream(scripted(nil, done(domain.StatusStopped, "half an answer")))
	rec := httptest.NewRecorder()

	h.stream(rec, t.Context())

	frames := parseSSE(rec.Body.String())
	require.Len(t, deltas(frames), 1, "a replay is one update")
	article := byTestID(parseHTML(t, deltas(frames)[0].Data), "message")[0]
	assert.Equal(t, "stopped", attrOf(article, "data-status"))
	assert.Contains(t, textContent(article), "half an answer")
	assert.Len(t, byTestID(article, "stopped-note"), 1)
	assert.Equal(t, "done", frames[len(frames)-1].Event)
}

func TestStream_Problems(t *testing.T) {
	tests := []struct {
		name       string
		items      []any
		wantRetry  bool
		wantDelta  string // text expected in the final update; empty when none
		wantStatus string
		wantDone   bool
		wantKept   string // partial text that must stay visible
		wantLogged bool   // the failure is the server's, and is logged as an error
	}{
		{
			name:      "another connection is writing the reply: try again shortly",
			items:     []any{domain.ErrAlreadyStreaming},
			wantRetry: true,
		},
		{
			name: "someone else's or a missing reply", items: []any{domain.ErrNotFound},
			wantDelta: replyUnavailable, wantStatus: "failed", wantDone: true,
		},
		{
			name: "a reply in the wrong state", items: []any{domain.ErrInvalidState},
			wantDelta: replyUnavailable, wantStatus: "failed", wantDone: true,
		},
		{
			name: "an unexpected failure shows a plain message", items: []any{errors.New("db password is hunter2")},
			wantDelta: domain.FailureUnknown, wantStatus: "failed", wantDone: true, wantLogged: true,
		},
		{
			name: "a failure after some text keeps that text", items: []any{snap("partial answer"), errors.New("boom")},
			wantDelta: domain.FailureUnknown, wantStatus: "failed", wantDone: true, wantKept: "partial answer", wantLogged: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newChatHarness(t)
				log, logs := capturingLogger(slog.LevelInfo)
				h.app.log = log
				h.expectStream(scripted(nil, tc.items...))
				rec := httptest.NewRecorder()

				h.stream(rec, t.Context())

				assert.Equal(t, http.StatusOK, rec.Code, "problems are reported inside the stream: the browser retries HTTP errors forever")
				body := rec.Body.String()
				frames := parseSSE(body)
				assert.NotContains(t, body, "hunter2")
				assert.Equal(t, tc.wantRetry, strings.Contains(body, "retry: 1500\n"))
				assert.Equal(t, tc.wantDone, frames[len(frames)-1].Event == "done")
				logged := strings.Contains(logs.String(), `level=ERROR msg="streaming a reply failed" message_id=`+streamMsgID.String())
				assert.Equal(t, tc.wantLogged, logged, "what the browser is not told is in the log, with the reply's id")
				if tc.wantDelta == "" {
					return
				}
				all := deltas(frames)
				article := byTestID(parseHTML(t, all[len(all)-1].Data), "message")[0]
				assert.Equal(t, tc.wantStatus, attrOf(article, "data-status"))
				assert.Equal(t, tc.wantDelta, textContent(byTestID(article, "error")[0]))
				if tc.wantKept != "" {
					assert.Contains(t, textContent(byTestID(article, "message-body")[0]), tc.wantKept)
				}
			})
		})
	}
}

// The use case ends every reply with a final event or with an error. An iterator
// that only ends has not said how the reply ended, so the stream ends without a
// done event, and the browser connects again to be told.
func TestStream_AnIteratorThatEndsWithoutAFinalEvent(t *testing.T) {
	h := newChatHarness(t)
	h.expectStream(scripted(nil, snap("half an answer")))
	rec := httptest.NewRecorder()

	h.stream(rec, t.Context())

	all := deltas(parseSSE(rec.Body.String()))
	require.Len(t, all, 1, "the update that came is shown")
	article := byTestID(parseHTML(t, all[0].Data), "message")[0]
	assert.Equal(t, "streaming", attrOf(article, "data-status"))
	assert.Contains(t, textContent(article), "half an answer")
	assert.NotContains(t, rec.Body.String(), "event: done", "done would stop the browser from connecting again")
}

// rendererFailingFor returns a renderer whose reply partial is a bare article,
// and fails for a reply in one of the given states.
func rendererFailingFor(t *testing.T, states ...domain.Status) *Renderer {
	t.Helper()
	var partial strings.Builder
	partial.WriteString(`{{define "partial:assistant"}}`)
	for _, state := range states {
		fmt.Fprintf(&partial, `{{if eq .Status %q}}{{.NoSuchField}}{{end}}`, state)
	}
	partial.WriteString(`<article data-testid="message" data-status="{{.Status}}">{{.Body}}</article>{{end}}`)
	rd, err := NewRenderer(fstest.MapFS{
		"templates/base.html":               {Data: []byte(`<html>{{template "page:main" .}}</html>`)},
		"templates/pages/chat.html":         {Data: []byte(`{{define "page:main"}}chat{{end}}`)},
		"templates/partials/assistant.html": {Data: []byte(partial.String())},
	})
	require.NoError(t, err)
	return rd
}

// A reply that cannot be rendered is a fault of the page, not a client that
// left: it is logged as an error, with the reply's id. The stream ends without a
// done event, so the browser connects again and is shown the reply as saved.
func TestStream_ATemplateThatFails(t *testing.T) {
	tests := []struct {
		name       string
		failsFor   domain.Status
		items      []any
		wantDeltas int
	}{
		{
			name: "on an update", failsFor: domain.StatusStreaming,
			items: []any{snap("half an answer"), time.Minute, done(domain.StatusComplete, "the whole answer")},
		},
		{
			name: "on the final event", failsFor: domain.StatusComplete,
			items:      []any{snap("half an answer"), done(domain.StatusComplete, "the whole answer")},
			wantDeltas: 1,
		},
		{
			name: "on a failure", failsFor: domain.StatusFailed,
			items:      []any{snap("half an answer"), errors.New("boom")},
			wantDeltas: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newChatHarness(t)
				log, logs := capturingLogger(slog.LevelDebug)
				h.app.log = log
				h.app.renderer = rendererFailingFor(t, tc.failsFor)
				h.expectStream(scripted(nil, tc.items...))
				rec := httptest.NewRecorder()

				h.stream(rec, t.Context())
				time.Sleep(time.Hour) // let the scripted agent find out that nobody is listening
				synctest.Wait()

				frames := parseSSE(rec.Body.String())
				assert.Len(t, deltas(frames), tc.wantDeltas)
				assert.NotContains(t, rec.Body.String(), "event: done", "done would stop the browser from connecting again")
				assert.Contains(t, logs.String(), `level=ERROR msg="rendering a reply failed" message_id=`+streamMsgID.String())
				assert.NotContains(t, logs.String(), "client went away", "the client is still there")
			})
		})
	}
}

// The use case runs on the pump's own goroutine, out of the recovery
// middleware's reach: a panic there would end the process, and every other
// user's stream with it. It must end that one stream.
func TestStream_APanicEndsThatStreamNotTheServer(t *testing.T) {
	h := newChatHarness(t)
	log, logs := capturingLogger(slog.LevelInfo)
	h.app.log = log
	h.expectStream(func(yield func(service.StreamEvent, error) bool) {
		yield(snap("partial answer"), nil)
		panic("kaboom")
	})
	rec := httptest.NewRecorder()

	h.stream(rec, t.Context())

	assert.Equal(t, http.StatusOK, rec.Code)
	frames := parseSSE(rec.Body.String())
	assert.Equal(t, "done", frames[len(frames)-1].Event, "the browser is told, and closes the connection")
	all := deltas(frames)
	article := byTestID(parseHTML(t, all[len(all)-1].Data), "message")[0]
	assert.Equal(t, "failed", attrOf(article, "data-status"))
	assert.Equal(t, domain.FailureUnknown, textContent(byTestID(article, "error")[0]))
	assert.Contains(t, textContent(byTestID(article, "message-body")[0]), "partial answer", "what was shown stays")
	assert.NotContains(t, rec.Body.String(), "kaboom", "internals never reach the browser")
	assert.Contains(t, logs.String(), "kaboom")
	assert.Contains(t, logs.String(), "stack=", "the log has what is needed to find the bug")
}

// A closed tab must end the run at once, not at the next update. Models stay
// silent for tens of seconds while they think, and until the run ends the agent
// goes on working, and being paid for, on a reply nobody will read.
func TestStream_ClientGoesAway(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newChatHarness(t)
		var stopped bool
		h.expectStream(scripted(&stopped, snap("one"), time.Hour, snap("two")))
		ctx, cancel := context.WithCancel(t.Context())
		rec := newFlushRecorder()
		started := time.Now()

		time.AfterFunc(500*time.Millisecond, cancel) // the tab is closed half a second in, mid-silence
		h.stream(rec, ctx)
		returned := time.Since(started)
		time.Sleep(2 * time.Hour) // let the scripted agent wake up and find out
		synctest.Wait()

		assert.Less(t, returned, time.Second, "the handler ended when the tab closed, not when the next update came")
		assert.Len(t, deltas(rec.frames()), 1, "nothing is written after the client left")
		assert.NotContains(t, rec.Body.String(), "event: done")
		assert.True(t, stopped, "the use case's iterator was told to stop, which is what ends the agent run")
	})
}

// failingWriter accepts a few writes and then fails, like a client that stopped reading.
type failingWriter struct {
	*httptest.ResponseRecorder
	okWrites int
}

func (f *failingWriter) Write(b []byte) (int, error) {
	if f.okWrites <= 0 {
		return 0, errors.New("write: broken pipe")
	}
	f.okWrites--
	return f.ResponseRecorder.Write(b)
}

// WriteString is needed too: io.WriteString, which the server-sent events writer
// uses, calls it when there is one, and the recorder's own would be promoted past
// the Write above, so that no write ever failed and this writer was a recorder.
func (f *failingWriter) WriteString(s string) (int, error) { return f.Write([]byte(s)) }

func TestStream_ClientThatStoppedReadingEndsTheRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newChatHarnessWithOptions(t, Options{FlushInterval: time.Millisecond})
		log, logs := capturingLogger(slog.LevelDebug)
		h.app.log = log
		ctxSeen := make(chan context.Context, 1)
		h.chat.On("Stream", mock.Anything, alice.ID, convID, streamMsgID).
			Return(func(ctx context.Context, _, _, _ uuid.UUID) iter.Seq2[service.StreamEvent, error] {
				ctxSeen <- ctx
				return scripted(nil, snap("a"), 10*time.Millisecond, snap("ab"), 10*time.Millisecond, snap("abc"), time.Hour)
			})
		w := &failingWriter{ResponseRecorder: httptest.NewRecorder(), okWrites: 2} // the preamble and one update
		started := time.Now()

		h.stream(w, t.Context())
		returned := time.Since(started)
		streamCtx := <-ctxSeen
		time.Sleep(2 * time.Hour) // let the scripted agent finish
		synctest.Wait()

		assert.Less(t, returned, time.Second,
			"the handler gave up when the write failed, not at the next keep-alive or when the agent finished an hour later "+
				"(a stalled reader would hold the connection and the run)")
		assert.Error(t, streamCtx.Err(), "the use case's context is canceled, so the agent run stops and the partial reply is saved")
		assert.Contains(t, logs.String(), `level=DEBUG msg="stream ended: client went away"`)
		assert.NotContains(t, logs.String(), "level=ERROR", "a client that leaves is no fault of the server")
	})
}

// heldWriter lets some writes through and then holds every write until it is
// released, like a connection whose client takes nothing more.
type heldWriter struct {
	*httptest.ResponseRecorder
	okWrites int
	release  chan struct{}
}

func (h *heldWriter) Write(b []byte) (int, error) {
	if h.okWrites <= 0 {
		<-h.release
		return 0, errors.New("write: i/o timeout")
	}
	h.okWrites--
	return h.ResponseRecorder.Write(b)
}

// WriteString is needed for the same reason as failingWriter's.
func (h *heldWriter) WriteString(s string) (int, error) { return h.Write([]byte(s)) }

// A client can leave while a write to it is held: its connection takes nothing
// more, and the write waits for its deadline. The run does not wait with it. Its
// context comes from the request's, so the run is told at once, while the handler
// is still inside the write.
func TestStream_AClientThatLeavesEndsTheRunWhileAWriteIsHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newChatHarness(t)
		var running atomic.Int32
		ctxSeen := make(chan context.Context, 1)
		h.chat.On("Stream", mock.Anything, alice.ID, convID, streamMsgID).Return(runUntilEnded(&running, ctxSeen))
		w := &heldWriter{ResponseRecorder: httptest.NewRecorder(), okWrites: 1, release: make(chan struct{})} // the preamble
		requestCtx, leave := context.WithCancel(t.Context())
		defer leave()
		over := make(chan struct{})
		go func() {
			defer close(over)
			h.stream(w, requestCtx)
		}()
		defer func() { // whatever the test finds, the write is let go and the handler ends
			close(w.release)
			<-over
		}()
		synctest.Wait() // the first update is being written, and is held
		runCtx := <-ctxSeen
		require.NoError(t, runCtx.Err())
		require.EqualValues(t, 1, running.Load())

		leave()
		synctest.Wait()

		assert.Error(t, runCtx.Err(), "the run was told when the client left")
		assert.EqualValues(t, 0, running.Load(), "and it has ended")
		select {
		case <-over:
			t.Fatal("the handler returned, so no write was held")
		default: // the handler is still inside the write
		}
	})
}

func TestStream_RefusesBadIDs(t *testing.T) {
	for _, path := range []string{"/c/nope/m/" + streamMsgID.String() + "/stream", "/c/" + convID.String() + "/m/nope/stream"} {
		t.Run(path, func(t *testing.T) {
			h := newChatHarness(t)
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
			rec := httptest.NewRecorder()

			h.route.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusNotFound, rec.Code)
		})
	}
}

func TestStream_AnonymousEventSourceGetsAMessageNotARedirect(t *testing.T) {
	authMock := mocks.NewAuth(t)
	route := testAppWith(t, Services{Auth: authMock, Chat: mocks.NewChat(t), Conversations: mocks.NewConversation(t)}, Options{}).Routes()
	req := httptest.NewRequest(http.MethodGet, streamPath(), nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()

	route.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "a redirect would make the browser's EventSource retry forever")
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	frames := parseSSE(rec.Body.String())
	article := byTestID(parseHTML(t, deltas(frames)[0].Data), "message")[0]
	assert.Equal(t, streamMsgID.String(), attrOf(article, "data-id"))
	assert.Equal(t, sessionEnded, textContent(byTestID(article, "error")[0]))
	assert.Equal(t, "done", frames[len(frames)-1].Event)
}

func TestNextInterval(t *testing.T) {
	tests := []struct {
		base, cost, want time.Duration
	}{
		{50 * time.Millisecond, time.Millisecond, 50 * time.Millisecond},
		{50 * time.Millisecond, 12 * time.Millisecond, 50 * time.Millisecond},
		{50 * time.Millisecond, 20 * time.Millisecond, 80 * time.Millisecond},
		{50 * time.Millisecond, 100 * time.Millisecond, 400 * time.Millisecond},
		{50 * time.Millisecond, 0, 50 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("base %v cost %v", tc.base, tc.cost), func(t *testing.T) {
			assert.Equal(t, tc.want, nextInterval(tc.base, tc.cost))
		})
	}
}

func TestSSEWriter_Framing(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := newSSEWriter(rec)
	require.NoError(t, err)

	require.NoError(t, w.event("delta", "line one\nline two\r\nline three"))
	require.NoError(t, w.comment("a\nb"))
	require.NoError(t, w.retry(1500*time.Millisecond))

	assert.Equal(t, ": ok\n\n"+
		"event: delta\ndata: line one\ndata: line two\ndata: line three\n\n"+
		": a b\n\n"+
		"retry: 1500\n\n", rec.Body.String(), "multi-line data becomes one data line each; carriage returns can't break the framing")
}
