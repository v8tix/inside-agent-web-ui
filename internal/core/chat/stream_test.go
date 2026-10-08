package chat_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
)

var replyID = uuid.NewV7()

const prompt = "what is the answer?"

func replyIn(status domain.Status, content string) domain.Message {
	return domain.Message{ID: replyID, ConversationID: convID, Seq: 2, Role: domain.RoleAssistant, Status: status, Content: content}
}

// Items for script: a time.Duration sleeps, a domain.AgentEvent is yielded and
// an error ends the stream with that error.
func script(seen *int, items ...any) iter.Seq2[domain.AgentEvent, error] {
	return func(yield func(domain.AgentEvent, error) bool) {
		for _, it := range items {
			switch v := it.(type) {
			case time.Duration:
				time.Sleep(v)
			case domain.AgentEvent:
				if !yield(v, nil) {
					if seen != nil {
						*seen = -1 // the consumer stopped us
					}
					return
				}
			case error:
				yield(domain.AgentEvent{}, v)
				return
			}
		}
	}
}

func delta(s string) domain.AgentEvent { return domain.AgentEvent{Kind: domain.AgentDelta, Text: s} }
func final(s string) domain.AgentEvent { return domain.AgentEvent{Kind: domain.AgentFinal, Text: s} }
func tool(n string) domain.AgentEvent  { return domain.AgentEvent{Kind: domain.AgentToolCall, Tool: n} }
func usage(p, c int) domain.AgentEvent {
	return domain.AgentEvent{Kind: domain.AgentUsage, Usage: domain.Usage{Prompt: p, Completion: c}}
}

func (h *harness) expectClaim() {
	h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(replyIn(domain.StatusPending, ""), nil).Once()
	h.msgs.On("Claim", mock.Anything, userID, convID, replyID).
		Return(resource.Claimed{Reply: replyIn(domain.StatusStreaming, ""), Prompt: prompt}, nil).Once()
}

func (h *harness) expectRun(seq iter.Seq2[domain.AgentEvent, error]) {
	h.agent.On("Run", mock.Anything, domain.AgentRequest{UserID: userID, ConversationID: convID, Text: prompt}).Return(seq).Once()
}

// expectFinish expects the outcome to be saved with a context that is still
// usable, even if the request's own context is gone.
func (h *harness) expectFinish(out resource.Outcome, err error) {
	h.msgs.On("Finish",
		mock.MatchedBy(func(ctx context.Context) bool { return ctx.Err() == nil }),
		replyID, out).Return(err).Once()
}

func collect(seq iter.Seq2[service.StreamEvent, error]) ([]service.StreamEvent, error) {
	var out []service.StreamEvent
	for ev, err := range seq {
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
	return out, nil
}

func snapshots(evs []service.StreamEvent) []string {
	var out []string
	for _, e := range evs {
		if e.Kind == service.StreamSnapshot {
			out = append(out, e.Text)
		}
	}
	return out
}

func lastOf(t *testing.T, evs []service.StreamEvent) service.StreamEvent {
	t.Helper()
	require.NotEmpty(t, evs)
	return evs[len(evs)-1]
}

func TestStream_Completes(t *testing.T) {
	t.Run("snapshots grow, the final text wins, and timing and usage are recorded", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t)
			h.expectClaim()
			h.expectRun(script(nil,
				tool("lookup"),
				150*time.Millisecond, delta("Hel"),
				50*time.Millisecond, delta("lo"),
				100*time.Millisecond, final("Hello"), usage(3, 2),
			))
			h.expectFinish(resource.Outcome{
				Content: "Hello",
				Status:  domain.StatusComplete,
				Meta: domain.Meta{
					TTFTMs: 150, DurationMs: 300,
					Usage: domain.Usage{Prompt: 3, Completion: 2}, ToolCalls: []string{"lookup"},
				},
			}, nil)

			evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

			require.NoError(t, err)
			assert.Equal(t, []string{"Hel", "Hello", "Hello"}, snapshots(evs))
			done := lastOf(t, evs)
			assert.Equal(t, service.StreamDone, done.Kind)
			assert.Equal(t, domain.StatusComplete, done.Status)
			assert.Equal(t, "Hello", done.Text)
			assert.Empty(t, done.Error)
			assert.Equal(t, int64(150), done.Meta.TTFTMs)
			assert.Equal(t, int64(300), done.Meta.DurationMs)
		})
	})

	t.Run("times under a millisecond are rounded up, so they still count as measured", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t)
			h.expectClaim()
			h.expectRun(script(nil,
				500*time.Microsecond, delta("Hi"),
				100*time.Microsecond, final("Hi"),
			))
			h.expectFinish(resource.Outcome{
				Content: "Hi",
				Status:  domain.StatusComplete,
				Meta:    domain.Meta{TTFTMs: 1, DurationMs: 1},
			}, nil)

			evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

			require.NoError(t, err)
			done := lastOf(t, evs)
			assert.Equal(t, int64(1), done.Meta.TTFTMs, "0 would read as unknown")
			assert.Equal(t, int64(1), done.Meta.DurationMs)
		})
	})

	t.Run("a reply of several segments (text, tool, text) is joined by a blank line", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t)
			h.expectClaim()
			h.expectRun(script(nil, final("Let me check."), tool("search"), usage(1, 1), final("It is 42."), usage(2, 3)))
			h.expectFinish(resource.Outcome{
				Content: "Let me check.\n\nIt is 42.",
				Status:  domain.StatusComplete,
				Meta:    domain.Meta{Usage: domain.Usage{Prompt: 3, Completion: 4}, ToolCalls: []string{"search"}},
			}, nil)

			evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

			require.NoError(t, err)
			assert.Equal(t, []string{"Let me check.", "Let me check.\n\nIt is 42."}, snapshots(evs))
			assert.Equal(t, "Let me check.\n\nIt is 42.", lastOf(t, evs).Text)
		})
	})

	t.Run("empty deltas make no snapshot", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t)
			h.expectClaim()
			h.expectRun(script(nil, delta(""), delta("hi"), final("hi")))
			h.expectFinish(resource.Outcome{Content: "hi", Status: domain.StatusComplete}, nil)

			evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

			require.NoError(t, err)
			assert.Equal(t, []string{"hi", "hi"}, snapshots(evs))
		})
	})
}

func TestStream_ReplaysFinishedReplies(t *testing.T) {
	tests := []struct {
		name       string
		stored     domain.Message
		wantStatus domain.Status
		wantError  string
	}{
		{"complete", withMeta(replyIn(domain.StatusComplete, "stored answer"), domain.Meta{TTFTMs: 7}), domain.StatusComplete, ""},
		{"stopped keeps its partial text", replyIn(domain.StatusStopped, "half an"), domain.StatusStopped, ""},
		{"failed shows why", withError(replyIn(domain.StatusFailed, "par"), "The agent took too long to answer."), domain.StatusFailed, "The agent took too long to answer."},
		{"stopped before any text", replyIn(domain.StatusStopped, ""), domain.StatusStopped, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t) // no Claim, Run or Finish expected: the mocks fail the test if called
			h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(tc.stored, nil)

			evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

			require.NoError(t, err)
			require.Len(t, evs, 1, "a replay is one final event: it already carries the whole text")
			done := evs[0]
			assert.Equal(t, service.StreamDone, done.Kind)
			assert.Equal(t, tc.wantStatus, done.Status)
			assert.Equal(t, tc.wantError, done.Error)
			assert.Equal(t, tc.stored.Content, done.Text)
			assert.Equal(t, tc.stored.Meta, done.Meta)
		})
	}
}

func withMeta(m domain.Message, meta domain.Meta) domain.Message { m.Meta = meta; return m }
func withError(m domain.Message, e string) domain.Message        { m.Error = e; return m }

func TestStream_Refusals(t *testing.T) {
	tests := []struct {
		name  string
		setup func(h *harness)
		want  error
	}{
		{"unknown or foreign message", func(h *harness) {
			h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(domain.Message{}, domain.ErrNotFound)
		}, domain.ErrNotFound},
		{"a user message cannot be streamed", func(h *harness) {
			m := replyIn(domain.StatusComplete, "hi")
			m.Role = domain.RoleUser
			h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(m, nil)
		}, domain.ErrInvalidState},
		{"another connection wins the claim", func(h *harness) {
			h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(replyIn(domain.StatusPending, ""), nil)
			h.msgs.On("Claim", mock.Anything, userID, convID, replyID).Return(resource.Claimed{}, domain.ErrAlreadyStreaming)
		}, domain.ErrAlreadyStreaming},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)

			evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

			require.ErrorIs(t, err, tc.want)
			assert.Empty(t, evs)
		})
	}
}

func TestStream_Stopping(t *testing.T) {
	t.Run("a consumer that stops iterating stops the agent and saves the partial text", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t)
			var seen int
			h.expectClaim()
			h.expectRun(script(&seen, 150*time.Millisecond, delta("Hel"), delta("lo"), delta(" there")))
			h.expectFinish(resource.Outcome{
				Content: "Hel",
				Status:  domain.StatusStopped,
				Meta:    domain.Meta{TTFTMs: 150, DurationMs: 150},
			}, nil)

			for ev := range h.svc.Stream(t.Context(), userID, convID, replyID) {
				assert.Equal(t, "Hel", ev.Text)
				break
			}

			assert.Equal(t, -1, seen, "the agent's iterator was told to stop")
		})
	})

	t.Run("a canceled context stops the run and still saves, on a live context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t)
			h.expectClaim()
			h.agent.On("Run", mock.Anything, mock.Anything).Return(
				func(ctx context.Context, _ domain.AgentRequest) iter.Seq2[domain.AgentEvent, error] {
					return func(yield func(domain.AgentEvent, error) bool) {
						if !yield(delta("Hel"), nil) {
							return
						}
						<-ctx.Done()
						yield(domain.AgentEvent{}, fmt.Errorf("upstream: %w", ctx.Err()))
					}
				})
			h.expectFinish(resource.Outcome{Content: "Hel", Status: domain.StatusStopped, Meta: domain.Meta{TTFTMs: 0, DurationMs: 0}}, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			var evs []service.StreamEvent
			for ev, err := range h.svc.Stream(ctx, userID, convID, replyID) {
				require.NoError(t, err)
				evs = append(evs, ev)
				if ev.Kind == service.StreamSnapshot {
					cancel() // the browser went away
				}
			}

			assert.Equal(t, domain.StatusStopped, lastOf(t, evs).Status)
			assert.Equal(t, "Hel", lastOf(t, evs).Text)
		})
	})
}

// A model that does not stop would grow the reply in memory, in the database and,
// whole on every update, on the wire, for as long as the run is allowed to last.
func TestStream_ARunawayReplyIsCutOff(t *testing.T) {
	t.Run("it is ended as failed, with the text so far, and the agent is told to stop", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarnessWithReplyLimit(t, 25)
			var seen int
			h.expectClaim()
			h.expectRun(script(&seen, delta("0123456789"), delta("0123456789"), delta("0123456789"), delta("never read")))
			h.expectFinish(resource.Outcome{Content: "012345678901234567890123456789", Status: domain.StatusFailed, Error: domain.FailureTooLong}, nil)

			evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

			require.NoError(t, err)
			done := lastOf(t, evs)
			assert.Equal(t, domain.StatusFailed, done.Status)
			assert.Equal(t, domain.FailureTooLong, done.Error)
			assert.Equal(t, "012345678901234567890123456789", done.Text)
			assert.Equal(t, -1, seen, "the run was stopped, not left to go on")
			assert.Contains(t, h.logs.String(), "reply failed")
		})
	})

	t.Run("a reply exactly at the limit is fine", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarnessWithReplyLimit(t, 20)
			h.expectClaim()
			h.expectRun(script(nil, delta("0123456789"), delta("0123456789"), final("01234567890123456789")))
			h.expectFinish(resource.Outcome{Content: "01234567890123456789", Status: domain.StatusComplete}, nil)

			evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

			require.NoError(t, err)
			assert.Equal(t, domain.StatusComplete, lastOf(t, evs).Status)
		})
	})

	t.Run("the final text replaces its deltas, so it is not counted twice", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarnessWithReplyLimit(t, 12)
			h.expectClaim()
			h.expectRun(script(nil, delta("hello "), delta("world"), final("hello world"), final("again")))
			h.expectFinish(resource.Outcome{Content: "hello world\n\nagain", Status: domain.StatusFailed, Error: domain.FailureTooLong}, nil)

			evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

			require.NoError(t, err)
			assert.Equal(t, domain.FailureTooLong, lastOf(t, evs).Error, "11 bytes then 16 with the next segment")
		})
	})

	t.Run("a million bytes is the default", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t)
			big := strings.Repeat("x", 600_000)
			h.expectClaim()
			h.expectRun(script(nil, delta(big), delta(big)))
			h.expectFinish(resource.Outcome{Content: big + big, Status: domain.StatusFailed, Error: domain.FailureTooLong}, nil)

			evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

			require.NoError(t, err)
			assert.Equal(t, domain.FailureTooLong, lastOf(t, evs).Error)
		})
	})
}

func TestStream_Failures(t *testing.T) {
	tests := []struct {
		name        string
		items       []any
		wantContent string
		wantReason  string
		wantLogged  string
	}{
		{
			name:       "agent unreachable",
			items:      []any{fmt.Errorf("%w: connection refused", domain.ErrAgentUnavailable)},
			wantReason: domain.FailureUnavailable,
			wantLogged: "connection refused",
		},
		{
			name:       "the agent reports an error, whose details stay in the logs",
			items:      []any{&domain.AgentError{Detail: "quota exceeded for project x"}},
			wantReason: domain.FailureAgentError,
			wantLogged: "quota exceeded for project x",
		},
		{
			name:        "the stream is cut off after some text",
			items:       []any{delta("par"), domain.ErrIncompleteStream},
			wantContent: "par",
			wantReason:  domain.FailureIncomplete,
		},
		{
			name:       "the agent ends with no text and no error",
			wantReason: domain.FailureIncomplete,
			wantLogged: "ended before the reply was complete",
		},
		{
			name:       "the run times out",
			items:      []any{fmt.Errorf("agent run timed out: %w", context.DeadlineExceeded)},
			wantReason: domain.FailureTimeout,
		},
		{
			name:       "anything else",
			items:      []any{errors.New("kaboom")},
			wantReason: domain.FailureUnknown,
			wantLogged: "kaboom",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.expectClaim()
				h.expectRun(script(nil, tc.items...))
				h.expectFinish(resource.Outcome{Content: tc.wantContent, Status: domain.StatusFailed, Error: tc.wantReason}, nil)

				evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

				require.NoError(t, err, "a failed run is reported as a Done event, not as an error")
				done := lastOf(t, evs)
				assert.Equal(t, domain.StatusFailed, done.Status)
				assert.Equal(t, tc.wantReason, done.Error)
				assert.Equal(t, tc.wantContent, done.Text)
				if tc.wantLogged != "" {
					assert.Contains(t, h.logs.String(), tc.wantLogged)
					assert.NotContains(t, done.Error, tc.wantLogged, "internals never reach the user")
				}
			})
		})
	}

	t.Run("failing to save the outcome is an error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t)
			h.expectClaim()
			h.expectRun(script(nil, final("ok")))
			h.expectFinish(resource.Outcome{Content: "ok", Status: domain.StatusComplete}, errors.New("db down"))

			evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

			require.Error(t, err)
			assert.Contains(t, err.Error(), "saving reply")
			assert.Equal(t, []string{"ok"}, snapshots(evs), "the text was shown before the save failed")
		})
	})

	t.Run("a conversation deleted meanwhile ends the stream quietly", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t)
			h.expectClaim()
			h.expectRun(script(nil, final("ok")))
			h.expectFinish(resource.Outcome{Content: "ok", Status: domain.StatusComplete}, domain.ErrNotFound)

			evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

			require.NoError(t, err, "nobody is left to tell about it")
			assert.Equal(t, []string{"ok"}, snapshots(evs))
			assert.NotEqual(t, service.StreamDone, lastOf(t, evs).Kind)
			assert.Contains(t, h.logs.String(), "conversation was deleted")
			assert.NotContains(t, h.logs.String(), "level=ERROR")
		})
	})
}

// The outcome is saved on a context that the request cannot end, so a database
// that has stopped answering could hold the run for ever. The save is given five
// seconds, and then the stream ends with the error.
func TestStream_ASaveThatHangsIsGivenFiveSeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.expectClaim()
		h.expectRun(script(nil, final("ok")))
		// If nothing ends the wait, an hour of the test's clock does, so that a
		// missing limit fails the test with a message.
		hangs := func(ctx context.Context, _ uuid.UUID, _ resource.Outcome) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Hour):
				return errors.New("nothing ended the wait")
			}
		}
		h.msgs.On("Finish", mock.Anything, replyID, mock.Anything).Return(hangs).Once()
		start := time.Now()

		evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.ErrorContains(t, err, "saving reply")
		assert.Equal(t, 5*time.Second, time.Since(start), "the save is given five seconds, and no more")
		assert.Equal(t, []string{"ok"}, snapshots(evs), "the text was shown before the save gave up")
	})
}

// A stream registers as the writer of its reply before it claims it. When the
// claim fails the stream gives the reply back, and the next stream writes it.
func TestStream_AClaimThatFailedLeavesTheReplyFree(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(replyIn(domain.StatusPending, ""), nil).Twice()
		h.msgs.On("Claim", mock.Anything, userID, convID, replyID).Return(resource.Claimed{}, errors.New("db down")).Once()
		h.msgs.On("Claim", mock.Anything, userID, convID, replyID).
			Return(resource.Claimed{Reply: replyIn(domain.StatusStreaming, ""), Prompt: prompt}, nil).Once()
		h.expectRun(script(nil, final("ok")))
		h.expectFinish(resource.Outcome{Content: "ok", Status: domain.StatusComplete}, nil)

		_, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))
		require.ErrorContains(t, err, "db down")

		evs, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

		require.NoError(t, err, "the reply is not held by the stream that failed")
		assert.Equal(t, domain.StatusComplete, lastOf(t, evs).Status)
		assert.Equal(t, "ok", lastOf(t, evs).Text)
	})
}
