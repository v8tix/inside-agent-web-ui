package chat_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
)

func (h *harness) expectGet(m domain.Message) {
	h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(m, nil).Once()
}

func TestStop(t *testing.T) {
	stopped := replyIn(domain.StatusStopped, "")
	tests := []struct {
		name       string
		setup      func(h *harness)
		wantStatus domain.Status
		wantErr    error
	}{
		{
			name: "unknown or foreign message",
			setup: func(h *harness) {
				h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(domain.Message{}, domain.ErrNotFound)
			},
			wantErr: domain.ErrNotFound,
		},
		{
			name: "a user message cannot be stopped",
			setup: func(h *harness) {
				m := replyIn(domain.StatusComplete, "hi")
				m.Role = domain.RoleUser
				h.expectGet(m)
			},
			wantErr: domain.ErrInvalidState,
		},
		{
			name:       "a finished reply is returned as it is",
			setup:      func(h *harness) { h.expectGet(replyIn(domain.StatusComplete, "done already")) },
			wantStatus: domain.StatusComplete,
		},
		{
			name: "a reply nobody started is marked stopped, so it never runs",
			setup: func(h *harness) {
				h.expectGet(replyIn(domain.StatusPending, ""))
				h.msgs.On("Claim", mock.Anything, userID, convID, replyID).
					Return(resource.Claimed{Reply: replyIn(domain.StatusStreaming, ""), Prompt: prompt}, nil).Once()
				h.expectFinish(resource.Outcome{Status: domain.StatusStopped}, nil)
				h.expectGet(stopped)
			},
			wantStatus: domain.StatusStopped,
		},
		{
			name: "a stream that starts at the same moment is looked at again",
			setup: func(h *harness) {
				h.expectGet(replyIn(domain.StatusPending, ""))
				h.msgs.On("Claim", mock.Anything, userID, convID, replyID).Return(resource.Claimed{}, domain.ErrAlreadyStreaming).Once()
				h.expectGet(replyIn(domain.StatusStopped, "partial"))
			},
			wantStatus: domain.StatusStopped,
		},
		{
			name: "a reply that finished at the same moment is returned as it is",
			setup: func(h *harness) {
				h.expectGet(replyIn(domain.StatusPending, ""))
				h.msgs.On("Claim", mock.Anything, userID, convID, replyID).Return(resource.Claimed{}, domain.ErrInvalidState).Once()
				h.expectGet(replyIn(domain.StatusComplete, "all of it"))
			},
			wantStatus: domain.StatusComplete,
		},
		{
			name: "claiming fails",
			setup: func(h *harness) {
				h.expectGet(replyIn(domain.StatusPending, ""))
				h.msgs.On("Claim", mock.Anything, userID, convID, replyID).Return(resource.Claimed{}, errors.New("db down")).Once()
			},
			wantErr: errors.New("db down"),
		},
		{
			name: "saving fails",
			setup: func(h *harness) {
				h.expectGet(replyIn(domain.StatusPending, ""))
				h.msgs.On("Claim", mock.Anything, userID, convID, replyID).
					Return(resource.Claimed{Reply: replyIn(domain.StatusStreaming, "")}, nil).Once()
				h.expectFinish(resource.Outcome{Status: domain.StatusStopped}, errors.New("db down"))
			},
			wantErr: errors.New("db down"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)

			got, err := h.svc.Stop(t.Context(), userID, convID, replyID)

			if tc.wantErr != nil {
				if errors.Is(tc.wantErr, domain.ErrNotFound) || errors.Is(tc.wantErr, domain.ErrInvalidState) {
					require.ErrorIs(t, err, tc.wantErr)
				} else {
					require.ErrorContains(t, err, tc.wantErr.Error())
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, got.Status)
		})
	}
}

func TestStop_FinishesAnUnstartedReplyEvenIfTheRequestGoesAway(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	h.expectGet(replyIn(domain.StatusPending, ""))
	h.msgs.On("Claim", mock.Anything, userID, convID, replyID).
		Run(func(mock.Arguments) { cancel() }). // the browser leaves right after the claim
		Return(resource.Claimed{Reply: replyIn(domain.StatusStreaming, "")}, nil).Once()
	h.expectFinish(resource.Outcome{Status: domain.StatusStopped}, nil) // asserts the context is still alive
	h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(replyIn(domain.StatusStopped, ""), context.Canceled).Maybe()

	_, err := h.svc.Stop(ctx, userID, convID, replyID)

	require.ErrorIs(t, err, context.Canceled, "the caller is told; the reply is stopped regardless")
}

// agentBlockedUntilCanceled streams one delta, then waits for its context to
// end, as a real agent run does while it thinks.
func agentBlockedUntilCanceled() func(context.Context, domain.AgentRequest) iter.Seq2[domain.AgentEvent, error] {
	return func(ctx context.Context, _ domain.AgentRequest) iter.Seq2[domain.AgentEvent, error] {
		return func(yield func(domain.AgentEvent, error) bool) {
			if !yield(delta("Hel"), nil) {
				return
			}
			<-ctx.Done()
			yield(domain.AgentEvent{}, fmt.Errorf("upstream: %w", ctx.Err()))
		}
	}
}

func TestStop_EndsARunningReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.expectClaim() // the stream's own read and claim
		h.agent.On("Run", mock.Anything, mock.Anything).Return(agentBlockedUntilCanceled())
		h.expectFinish(resource.Outcome{Content: "Hel", Status: domain.StatusStopped, Meta: domain.Meta{}}, nil)
		h.expectGet(replyIn(domain.StatusStreaming, "Hel")) // Stop's first look
		h.expectGet(replyIn(domain.StatusStopped, "Hel"))   // and after the run saved

		var (
			wg     sync.WaitGroup
			events []service.StreamEvent
		)
		wg.Go(func() {
			for ev, err := range h.svc.Stream(t.Context(), userID, convID, replyID) {
				assert.NoError(t, err)
				events = append(events, ev)
			}
		})
		synctest.Wait() // the stream is up, waiting for the agent
		start := time.Now()

		got, err := h.svc.Stop(t.Context(), userID, convID, replyID)

		require.NoError(t, err)
		assert.Zero(t, time.Since(start), "the run wakes Stop when it has saved; Stop does not sit out its wait")
		assert.Equal(t, domain.StatusStopped, got.Status)
		assert.Equal(t, "Hel", got.Content)
		wg.Wait()
		done := lastOf(t, events)
		assert.Equal(t, service.StreamDone, done.Kind, "the stream that was connected is told how it ended")
		assert.Equal(t, domain.StatusStopped, done.Status)
	})
}

func TestStop_GivesUpWaitingForASlowSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.expectClaim()
		h.agent.On("Run", mock.Anything, mock.Anything).Return(agentBlockedUntilCanceled())
		release := make(chan struct{})
		h.msgs.On("Finish", mock.Anything, replyID, mock.Anything).
			Run(func(mock.Arguments) { <-release }). // the database is slow
			Return(nil).Once()
		h.expectGet(replyIn(domain.StatusStreaming, "Hel"))
		h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(replyIn(domain.StatusStreaming, "Hel"), nil)

		var wg sync.WaitGroup
		wg.Go(func() {
			for range h.svc.Stream(t.Context(), userID, convID, replyID) {
			}
		})
		synctest.Wait()

		start := time.Now()
		got, err := h.svc.Stop(t.Context(), userID, convID, replyID)

		require.NoError(t, err)
		assert.Equal(t, domain.StatusStreaming, got.Status, "the caller gets what is stored, however slow the save")
		assert.Equal(t, 3*time.Second*3, time.Since(start), "three looks, each waiting at most three seconds")
		close(release)
		wg.Wait()
	})
}

func TestStream_OnlyOneRunPerReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(replyIn(domain.StatusPending, ""), nil)
		h.msgs.On("Claim", mock.Anything, userID, convID, replyID).
			Return(resource.Claimed{Reply: replyIn(domain.StatusStreaming, ""), Prompt: prompt}, nil).Once() // a second claim would fail the test
		h.agent.On("Run", mock.Anything, mock.Anything).Return(agentBlockedUntilCanceled()).Once()
		h.msgs.On("Finish", mock.Anything, replyID, mock.Anything).Return(nil).Once()

		ctx, cancel := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		wg.Go(func() {
			for range h.svc.Stream(ctx, userID, convID, replyID) {
			}
		})
		synctest.Wait()

		_, err := collect(h.svc.Stream(t.Context(), userID, convID, replyID))

		require.ErrorIs(t, err, domain.ErrAlreadyStreaming, "answered from memory, before asking the database")
		cancel()
		wg.Wait()
	})
}
