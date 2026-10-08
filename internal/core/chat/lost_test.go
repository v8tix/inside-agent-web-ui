package chat_test

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/chat"
	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource/mocks"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
)

// memStore keeps one reply in memory and behaves like the database where these
// tests need it to: a claim that the caller gave up on may still have been
// committed, and a reply can only finish from streaming. Any other call fails the
// test, because the mock it embeds has no expectations.
type memStore struct {
	*mocks.MessageStore

	mu    sync.Mutex
	reply domain.Message

	// gate, once held, makes every Claim wait until release: the database is slow.
	gate chan struct{}
	// saveGate, once held, makes the first Finish wait until it is let through: the
	// reply is claimed and its save is on the way. A later Finish does not wait.
	saveGate chan struct{}
	// beforeFinish runs just before Finish looks at the reply: a run that ends on
	// its own, at the wrong moment.
	beforeFinish func(*domain.Message)
	finishErr    error
}

func newMemStore(t *testing.T, reply domain.Message) *memStore {
	return &memStore{MessageStore: mocks.NewMessageStore(t), reply: reply}
}

func (m *memStore) hold()    { m.gate = make(chan struct{}) }
func (m *memStore) release() { close(m.gate) }

// holdFirstSave makes the first Finish wait until the function it returns is
// called.
func (m *memStore) holdFirstSave() (letThrough func()) {
	gate := make(chan struct{})
	m.saveGate = gate
	return func() { close(gate) }
}

// firstSaveGate hands the gate to the first Finish that asks, and to no other.
func (m *memStore) firstSaveGate() chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	gate := m.saveGate
	m.saveGate = nil
	return gate
}

func (m *memStore) current() domain.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reply
}

func (m *memStore) Get(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (domain.Message, error) {
	return m.current(), nil
}

func (m *memStore) Claim(ctx context.Context, _, _, _ uuid.UUID) (resource.Claimed, error) {
	if m.gate != nil {
		<-m.gate
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.reply.Status {
	case domain.StatusPending:
	case domain.StatusStreaming:
		return resource.Claimed{}, domain.ErrAlreadyStreaming
	default:
		return resource.Claimed{}, domain.ErrInvalidState
	}
	m.reply.Status = domain.StatusStreaming // committed, as the UPDATE is
	if err := ctx.Err(); err != nil {
		return resource.Claimed{}, err // but the caller was no longer there to hear it
	}
	return resource.Claimed{Reply: m.reply, Prompt: prompt}, nil
}

func (m *memStore) Finish(_ context.Context, _ uuid.UUID, out resource.Outcome) error {
	if gate := m.firstSaveGate(); gate != nil {
		<-gate
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.beforeFinish != nil {
		m.beforeFinish(&m.reply)
	}
	if m.finishErr != nil {
		return m.finishErr
	}
	if m.reply.Status != domain.StatusStreaming {
		return domain.ErrInvalidState
	}
	m.reply.Status, m.reply.Content, m.reply.Error = out.Status, out.Content, out.Error
	return nil
}

// lostRig is a chat service over a memStore whose agent waits to be canceled.
func lostRig(t *testing.T, reply domain.Message) (*chat.Service, *memStore) {
	t.Helper()
	return lostRigWith(t, reply, agentBlockedUntilCanceled())
}

// lostRigWith is lostRig with the agent of your choice.
func lostRigWith(t *testing.T, reply domain.Message, run func(context.Context, domain.AgentRequest) iter.Seq2[domain.AgentEvent, error]) (*chat.Service, *memStore) {
	t.Helper()
	store := newMemStore(t, reply)
	agent := mocks.NewAgentGateway(t)
	agent.On("Run", mock.Anything, mock.Anything).Return(run).Maybe()
	svc := chat.NewService(chat.Dependencies{
		Conversations:   mocks.NewConversationStore(t),
		Messages:        store,
		Agent:           agent,
		Logger:          slog.New(slog.DiscardHandler),
		MaxMessageChars: maxChars,
	})
	return svc, store
}

// A reply that is streaming in the database while nothing here writes it can
// never finish by itself: the page would wait for it for good, and Retry is
// refused until it ends. Whoever finds it ends it.
func TestALostReplyIsEnded(t *testing.T) {
	entries := []struct {
		name string
		act  func(*testing.T, *chat.Service) (domain.Message, error)
	}{
		{"Stop", func(t *testing.T, svc *chat.Service) (domain.Message, error) {
			return svc.Stop(t.Context(), userID, convID, replyID)
		}},
		{"Stream", func(t *testing.T, svc *chat.Service) (domain.Message, error) {
			evs, err := collect(svc.Stream(t.Context(), userID, convID, replyID))
			if err != nil {
				return domain.Message{}, err
			}
			require.Len(t, evs, 1, "the whole state comes as one event")
			done := evs[0]
			require.Equal(t, service.StreamDone, done.Kind)
			return domain.Message{Status: done.Status, Content: done.Text, Error: done.Error}, nil
		}},
	}
	for _, entry := range entries {
		t.Run(entry.name+" fails it as interrupted and keeps its text", func(t *testing.T) {
			svc, store := lostRig(t, replyIn(domain.StatusStreaming, "so far"))

			got, err := entry.act(t, svc)

			require.NoError(t, err)
			assert.Equal(t, domain.StatusFailed, got.Status)
			assert.Equal(t, domain.FailureInterrupted, got.Error)
			assert.Equal(t, "so far", got.Content)
			saved := store.current()
			assert.Equal(t, domain.StatusFailed, saved.Status, "and it is saved, not only reported")
			assert.Equal(t, domain.FailureInterrupted, saved.Error)
			assert.Equal(t, "so far", saved.Content)
		})

		t.Run(entry.name+" reports what a reply that ended on its own became", func(t *testing.T) {
			svc, store := lostRig(t, replyIn(domain.StatusStreaming, ""))
			store.beforeFinish = func(r *domain.Message) { // it was alive after all, and finishes now
				r.Status, r.Content = domain.StatusComplete, "all of it"
				store.beforeFinish = nil
			}

			got, err := entry.act(t, svc)

			require.NoError(t, err)
			assert.Equal(t, domain.StatusComplete, got.Status, "a finished reply is never overwritten")
			assert.Equal(t, "all of it", got.Content)
			assert.Empty(t, got.Error)
		})

		t.Run(entry.name+" passes on a failure to save", func(t *testing.T) {
			svc, store := lostRig(t, replyIn(domain.StatusStreaming, ""))
			store.finishErr = errors.New("db down")

			_, err := entry.act(t, svc)

			require.ErrorContains(t, err, "db down")
			assert.Equal(t, domain.StatusStreaming, store.current().Status)
		})
	}
}

// A run that is really writing the reply is not lost, and is left alone.
func TestARunningReplyIsNotEnded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, store := lostRig(t, replyIn(domain.StatusPending, ""))
		ctx, cancel := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		wg.Go(func() {
			for range svc.Stream(ctx, userID, convID, replyID) {
			}
		})
		synctest.Wait() // the stream is up and the reply is streaming
		require.Equal(t, domain.StatusStreaming, store.current().Status)

		_, err := collect(svc.Stream(t.Context(), userID, convID, replyID))

		require.ErrorIs(t, err, domain.ErrAlreadyStreaming, "the browser is told to come back")
		assert.Equal(t, domain.StatusStreaming, store.current().Status, "and the run is not cut short")
		cancel()
		wg.Wait()
		assert.Equal(t, domain.StatusStopped, store.current().Status)
	})
}

// The database can commit a claim the caller has given up on. If the claim
// followed the request's own context, a Stop or a closed tab at that moment
// would leave the reply streaming with nothing to write it.
func TestAClaimIsNotAbandonedWhenTheRequestIs(t *testing.T) {
	t.Run("Stop is pressed while the stream's claim is in flight", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, store := lostRig(t, replyIn(domain.StatusPending, ""))
			store.hold()
			var wg sync.WaitGroup
			wg.Go(func() {
				for range svc.Stream(t.Context(), userID, convID, replyID) {
				}
			})
			synctest.Wait() // the stream is inside Claim
			wg.Go(func() { _, _ = svc.Stop(t.Context(), userID, convID, replyID) })
			synctest.Wait() // Stop has canceled the run and waits for it

			store.release()
			wg.Wait()

			assert.Equal(t, domain.StatusStopped, store.current().Status)
		})
	})

	t.Run("the browser leaves while the stream's claim is in flight", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, store := lostRig(t, replyIn(domain.StatusPending, ""))
			store.hold()
			ctx, cancel := context.WithCancel(t.Context())
			var wg sync.WaitGroup
			wg.Go(func() {
				for range svc.Stream(ctx, userID, convID, replyID) {
				}
			})
			synctest.Wait()
			cancel()
			synctest.Wait()

			store.release()
			wg.Wait()

			assert.Equal(t, domain.StatusStopped, store.current().Status)
		})
	})

	t.Run("the browser leaves while Stop's claim of an unstarted reply is in flight", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, store := lostRig(t, replyIn(domain.StatusPending, ""))
			store.hold()
			ctx, cancel := context.WithCancel(t.Context())
			var wg sync.WaitGroup
			wg.Go(func() { _, _ = svc.Stop(ctx, userID, convID, replyID) })
			synctest.Wait()
			cancel()
			synctest.Wait()

			store.release()
			wg.Wait()

			assert.Equal(t, domain.StatusStopped, store.current().Status)
		})
	})
}

// Between its claim and its save, a Stop of a reply nobody started leaves the
// reply streaming in the database, as a lost one is. Stop is its writer for that
// moment, so whoever finds the reply then does not end it as interrupted: a stream
// is told to come back, and another Stop waits for the save.
func TestStop_AnUnstartedReplyIsNotTakenForALostOne(t *testing.T) {
	t.Run("a stream that connects between the claim and the save is told to come back", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, store := lostRig(t, replyIn(domain.StatusPending, ""))
			letThrough := store.holdFirstSave()
			var (
				wg      sync.WaitGroup
				stopped domain.Message
				stopErr error
			)
			wg.Go(func() { stopped, stopErr = svc.Stop(t.Context(), userID, convID, replyID) })
			synctest.Wait() // Stop has claimed the reply and its save is on the way
			claimed := store.current().Status

			evs, err := collect(svc.Stream(t.Context(), userID, convID, replyID))
			letThrough()
			wg.Wait()

			require.Equal(t, domain.StatusStreaming, claimed, "the stream came between the claim and the save")
			require.ErrorIs(t, err, domain.ErrAlreadyStreaming, "Stop is writing the reply")
			assert.Empty(t, evs)
			require.NoError(t, stopErr)
			assert.Equal(t, domain.StatusStopped, stopped.Status)
			saved := store.current()
			assert.Equal(t, domain.StatusStopped, saved.Status)
			assert.Empty(t, saved.Error)
		})
	})

	t.Run("a second Stop waits for the save of the first", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			svc, store := lostRig(t, replyIn(domain.StatusPending, ""))
			letThrough := store.holdFirstSave()
			var (
				wg      sync.WaitGroup
				stopped [2]domain.Message
				stopErr [2]error
			)
			start := time.Now()
			wg.Go(func() { stopped[0], stopErr[0] = svc.Stop(t.Context(), userID, convID, replyID) })
			synctest.Wait() // the first Stop has claimed the reply and its save is on the way
			claimed := store.current().Status
			wg.Go(func() { stopped[1], stopErr[1] = svc.Stop(t.Context(), userID, convID, replyID) })
			synctest.Wait() // the second Stop has found the reply streaming

			letThrough()
			wg.Wait()

			require.Equal(t, domain.StatusStreaming, claimed, "the second Stop came between the claim and the save")
			assert.Zero(t, time.Since(start), "the save wakes the second Stop; it does not sit out its wait")
			for i := range stopped {
				require.NoError(t, stopErr[i])
				assert.Equal(t, domain.StatusStopped, stopped[i].Status)
			}
			saved := store.current()
			assert.Equal(t, domain.StatusStopped, saved.Status)
			assert.Empty(t, saved.Error)
		})
	})
}

// A bug that panics while a reply is written ends that request (the transport
// recovers it), but it must not leave the reply streaming: the page would wait
// for it, and Retry would be refused, until the next reload or restart.
func TestAPanicInARunStillEndsTheReply(t *testing.T) {
	panics := func(context.Context, domain.AgentRequest) iter.Seq2[domain.AgentEvent, error] {
		return func(yield func(domain.AgentEvent, error) bool) {
			yield(delta("par"), nil)
			panic("kaboom")
		}
	}
	svc, store := lostRigWith(t, replyIn(domain.StatusPending, ""), panics)

	require.PanicsWithValue(t, "kaboom", func() {
		for range svc.Stream(t.Context(), userID, convID, replyID) {
		}
	}, "the panic goes on to whoever recovers it")

	saved := store.current()
	assert.Equal(t, domain.StatusFailed, saved.Status)
	assert.Equal(t, domain.FailureInterrupted, saved.Error)
	assert.Equal(t, "par", saved.Content, "with the text that had arrived")
}

// The guard that ends a reply after a panic works on a context of its own. The
// run's context may be over by then, and a store that has stopped answering does
// not hold the panic back for ever. What the guard cannot do, it reports.
func TestAPanicInARun_TheGuardOutlivesTheRunAndGivesUpInTime(t *testing.T) {
	const reported = "ending a reply after a panic failed"

	t.Run("a reply that cannot be saved is reported", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t)
			h.expectClaim()
			h.expectRun(func(yield func(domain.AgentEvent, error) bool) {
				yield(delta("par"), nil)
				panic("kaboom")
			})
			h.expectFinish(resource.Outcome{Content: "par", Status: domain.StatusFailed, Error: domain.FailureInterrupted}, errors.New("db down"))

			require.PanicsWithValue(t, "kaboom", func() {
				for range h.svc.Stream(t.Context(), userID, convID, replyID) {
				}
			})

			assert.Contains(t, h.logs.String(), reported)
			assert.Contains(t, h.logs.String(), "db down")
		})
	})

	t.Run("a reply saved before the panic is left as it finished, though the run's context is over", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t)
			h.expectClaim()
			h.agent.On("Run", mock.Anything, mock.Anything).Return(agentBlockedUntilCanceled())
			h.expectFinish(resource.Outcome{Content: "Hel", Status: domain.StatusStopped}, nil)
			// The guard asks to end the reply, is told that it has finished, and reads it back.
			h.expectFinish(resource.Outcome{Content: "Hel", Status: domain.StatusFailed, Error: domain.FailureInterrupted}, domain.ErrInvalidState)
			h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(
				func(ctx context.Context, _, _, _ uuid.UUID) (domain.Message, error) {
					if err := ctx.Err(); err != nil {
						return domain.Message{}, err // what a database answers on a context that is over
					}
					return replyIn(domain.StatusStopped, "Hel"), nil
				}).Once()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			require.PanicsWithValue(t, "kaboom", func() {
				for ev := range h.svc.Stream(ctx, userID, convID, replyID) {
					if ev.Kind == service.StreamDone {
						panic("kaboom") // whoever reads the stream panics on its last event
					}
					cancel() // the browser leaves, and the run is saved as stopped
				}
			})

			assert.NotContains(t, h.logs.String(), reported)
		})
	})

	t.Run("reading back a reply that had finished is given five seconds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t)
			h.expectClaim()
			h.expectRun(script(nil, final("ok")))
			h.expectFinish(resource.Outcome{Content: "ok", Status: domain.StatusComplete}, nil)
			h.expectFinish(resource.Outcome{Content: "ok", Status: domain.StatusFailed, Error: domain.FailureInterrupted}, domain.ErrInvalidState)
			// If nothing ends the wait, an hour of the test's clock does, so that a
			// missing limit fails the test with a message.
			hangs := func(ctx context.Context, _, _, _ uuid.UUID) (domain.Message, error) {
				select {
				case <-ctx.Done():
					return domain.Message{}, ctx.Err()
				case <-time.After(time.Hour):
					return domain.Message{}, errors.New("nothing ended the wait")
				}
			}
			h.msgs.On("Get", mock.Anything, userID, convID, replyID).Return(hangs).Once()
			start := time.Now()

			require.PanicsWithValue(t, "kaboom", func() {
				for ev := range h.svc.Stream(t.Context(), userID, convID, replyID) {
					if ev.Kind == service.StreamDone {
						panic("kaboom")
					}
				}
			})

			assert.Equal(t, 5*time.Second, time.Since(start), "the guard is given five seconds, and no more")
			assert.Contains(t, h.logs.String(), reported)
			assert.Contains(t, h.logs.String(), "deadline exceeded")
		})
	})
}
