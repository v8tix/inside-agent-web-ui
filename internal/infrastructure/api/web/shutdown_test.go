package web

import (
	"bufio"
	"context"
	"iter"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
)

// runUntilEnded is a reply being written: it shows "Hel", then waits until the
// run is canceled, as the real use case does, and reports the reply as stopped
// with the text so far. running counts runs that have not finished.
func runUntilEnded(running *atomic.Int32, ctxSeen chan<- context.Context) func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) iter.Seq2[service.StreamEvent, error] {
	return func(ctx context.Context, _, _, _ uuid.UUID) iter.Seq2[service.StreamEvent, error] {
		return func(yield func(service.StreamEvent, error) bool) {
			running.Add(1)
			defer running.Add(-1)
			if ctxSeen != nil {
				ctxSeen <- ctx
			}
			if !yield(snap("Hel"), nil) {
				return
			}
			<-ctx.Done()
			yield(done(domain.StatusStopped, "Hel"), nil)
		}
	}
}

func TestEndStreams(t *testing.T) {
	t.Run("a running reply is saved as stopped and the browser is told", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newChatHarness(t)
			var running atomic.Int32
			ctxSeen := make(chan context.Context, 1)
			h.chat.On("Stream", mock.Anything, alice.ID, convID, streamMsgID).Return(runUntilEnded(&running, ctxSeen))
			rec := newFlushRecorder()
			requestCtx, leave := context.WithCancel(t.Context())
			defer leave()
			over := make(chan struct{})
			go func() {
				defer close(over)
				h.stream(rec, requestCtx)
			}()

			time.Sleep(time.Second) // the reply is under way
			runCtx := <-ctxSeen
			assert.NoError(t, runCtx.Err())
			h.app.EndStreams()
			select {
			case <-over:
			case <-time.After(5 * time.Second):
				t.Fatal("the stream did not end when streams were ended")
			}

			assert.Error(t, runCtx.Err(), "the run was ended")
			assert.NoError(t, requestCtx.Err(), "while the connection stayed up to say so")
			frames := rec.frames()
			require.GreaterOrEqual(t, len(frames), 3)
			finalDelta, last := frames[len(frames)-2], frames[len(frames)-1]
			assert.Equal(t, "delta", finalDelta.Event)
			article := byTestID(parseHTML(t, finalDelta.Data), "message")
			require.Len(t, article, 1)
			assert.Equal(t, "stopped", attrOf(article[0], "data-status"))
			assert.Contains(t, finalDelta.Data, "Hel")
			assert.Equal(t, "done", last.Event, "and the stream is closed properly")
			assert.EqualValues(t, 0, running.Load())
		})
	})

	t.Run("streams that begin during shutdown are told to come back", func(t *testing.T) {
		h := newChatHarness(t) // the use case has no expectations: starting a run would fail the test
		h.app.EndStreams()
		rec := newFlushRecorder()

		h.stream(rec, t.Context())

		frames := rec.frames()
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
		require.NotEmpty(t, frames)
		assert.Equal(t, "1500", frames[len(frames)-1].Retry, "the browser reconnects, to the next server")
		assert.Empty(t, deltas(frames))
		assert.NotContains(t, rec.Body.String(), "event: done", "done would stop it from reconnecting")
	})

	t.Run("it can be called more than once", func(t *testing.T) {
		app := testAppWith(t, Services{}, Options{})

		app.EndStreams()
		app.EndStreams()
	})
}

// sseClient reads a stream line by line over a real connection.
type sseClient struct {
	body  *bufio.Reader
	close func()
}

func openStream(t *testing.T, addr, path string) *sseClient {
	t.Helper()
	// A stream that never ends must fail the test, not hang it.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return &sseClient{body: bufio.NewReader(resp.Body), close: func() { _ = resp.Body.Close() }}
}

// next returns the next event's name and data, or ok=false at the end.
func (c *sseClient) next() (event, data string, ok bool) {
	var lines []string
	for {
		line, err := c.body.ReadString('\n')
		if err != nil {
			return "", "", false
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(lines) == 0 {
				continue
			}
			for _, l := range lines {
				switch {
				case strings.HasPrefix(l, "event: "):
					event = strings.TrimPrefix(l, "event: ")
				case strings.HasPrefix(l, "data: "):
					data += strings.TrimPrefix(l, "data: ")
				}
			}
			if event == "" && data == "" {
				lines = nil // a comment
				continue
			}
			return event, data, true
		}
		lines = append(lines, line)
	}
}

func TestServer_ShutdownEndsStreams(t *testing.T) {
	h := newChatHarness(t)
	var running atomic.Int32
	h.chat.On("Stream", mock.Anything, alice.ID, convID, streamMsgID).Return(runUntilEnded(&running, nil))
	srv, ln := newTestServer(t, h.route, 10*time.Second, h.app.EndStreams)
	log, logs := capturingLogger(slog.LevelInfo)
	srv.log = log
	ctx, shutDown := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, ln) }()

	stream := openStream(t, ln.Addr().String(), streamPath())
	defer stream.close()
	event, _, ok := stream.next()
	require.True(t, ok)
	require.Equal(t, "delta", event, "the reply is under way")

	started := time.Now()
	shutDown() // SIGTERM

	event, data, ok := stream.next()
	require.True(t, ok, "the browser is still told")
	assert.Equal(t, "delta", event)
	assert.Equal(t, "stopped", attrOf(byTestID(parseHTML(t, data), "message")[0], "data-status"))
	event, _, ok = stream.next()
	require.True(t, ok)
	assert.Equal(t, "done", event)
	_, _, ok = stream.next()
	assert.False(t, ok, "and the connection is closed")

	select {
	case err := <-served:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not stop")
	}
	assert.Less(t, time.Since(started), 2*time.Second, "well within the 10 s grace: nothing waited for it")
	assert.EqualValues(t, 0, running.Load(), "the run has finished, nothing is left going")
	assert.Contains(t, logs.String(), "msg=listening addr="+ln.Addr().String(), "the start says where the server listens")
	assert.Contains(t, logs.String(), `msg="shutting down" grace=10s`, "and the stop says how long requests are given")
}
