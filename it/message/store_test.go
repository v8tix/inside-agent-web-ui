// Integration tests for the message store. Group A: needs Docker.
package message_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/postgres"
	"github.com/v8tix/inside-agent-web-ui/it/internal/dbtest"
)

type fixture struct {
	pool          *pgxpool.Pool
	conversations *postgres.ConversationStore
	messages      *postgres.MessageStore
	alice, bob    uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := dbtest.Pool(t)
	sql := dbtest.Statements(t)
	return &fixture{
		pool:          pool,
		conversations: postgres.NewConversationStore(pool, sql),
		messages:      postgres.NewMessageStore(pool, sql),
		alice:         dbtest.SeedUser(t, pool, "alice@example.com"),
		bob:           dbtest.SeedUser(t, pool, "bob@example.com"),
	}
}

func (f *fixture) conversation(t *testing.T, owner uuid.UUID) uuid.UUID {
	t.Helper()
	c, err := f.conversations.Create(t.Context(), domain.Conversation{ID: uuid.NewV7(), UserID: owner, Title: "chat"})
	require.NoError(t, err)
	return c.ID
}

// turn appends a pair and finishes the reply, so the next pair is allowed.
func (f *fixture) turn(t *testing.T, owner, conv uuid.UUID, prompt, answer string) (user, reply domain.Message) {
	t.Helper()
	user, reply, err := f.messages.AppendPair(t.Context(), owner, conv, prompt)
	require.NoError(t, err)
	_, err = f.messages.Claim(t.Context(), owner, conv, reply.ID)
	require.NoError(t, err)
	require.NoError(t, f.messages.Finish(t.Context(), reply.ID, resource.Outcome{Content: answer, Status: domain.StatusComplete}))
	return user, reply
}

func TestAppendPair(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	t.Run("adds a complete user message and a pending reply with consecutive numbers", func(t *testing.T) {
		conv := f.conversation(t, f.alice)

		user, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "hello")

		require.NoError(t, err)
		assert.Equal(t, 1, user.Seq)
		assert.Equal(t, domain.RoleUser, user.Role)
		assert.Equal(t, domain.StatusComplete, user.Status)
		assert.Equal(t, "hello", user.Content)
		assert.Equal(t, 2, reply.Seq)
		assert.Equal(t, domain.RoleAssistant, reply.Role)
		assert.Equal(t, domain.StatusPending, reply.Status)
		assert.Empty(t, reply.Content)
		assert.Equal(t, conv, reply.ConversationID)
		assert.NotEqual(t, user.ID, reply.ID)

		got, err := f.messages.List(ctx, f.alice, conv)
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, []int{1, 2}, []int{got[0].Seq, got[1].Seq})
	})

	t.Run("continues the numbering and bumps the conversation's activity", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		f.turn(t, f.alice, conv, "one", "answer one")
		before, err := f.conversations.Get(ctx, f.alice, conv)
		require.NoError(t, err)

		user, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "two")

		require.NoError(t, err)
		assert.Equal(t, 3, user.Seq)
		assert.Equal(t, 4, reply.Seq)
		after, err := f.conversations.Get(ctx, f.alice, conv)
		require.NoError(t, err)
		assert.True(t, after.UpdatedAt.After(before.UpdatedAt))
	})

	t.Run("refuses a new turn while the last reply is unfinished", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "first")
		require.NoError(t, err)

		_, _, err = f.messages.AppendPair(ctx, f.alice, conv, "second")
		assert.ErrorIs(t, err, domain.ErrInvalidState, "pending")

		_, err = f.messages.Claim(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		_, _, err = f.messages.AppendPair(ctx, f.alice, conv, "second")
		assert.ErrorIs(t, err, domain.ErrInvalidState, "streaming")

		require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{Status: domain.StatusStopped}))
		_, _, err = f.messages.AppendPair(ctx, f.alice, conv, "second")
		assert.NoError(t, err, "a stopped reply counts as finished")
	})

	t.Run("another user's conversation and unknown ones are not found", func(t *testing.T) {
		conv := f.conversation(t, f.alice)

		_, _, err := f.messages.AppendPair(ctx, f.bob, conv, "hi")
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, _, err = f.messages.AppendPair(ctx, f.alice, uuid.NewV7(), "hi")
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("twenty concurrent appends: exactly one wins, numbering stays gap-free", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		var wins, conflicts atomic.Int32
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				_, _, err := f.messages.AppendPair(ctx, f.alice, conv, "race")
				switch {
				case err == nil:
					wins.Add(1)
				case errors.Is(err, domain.ErrInvalidState):
					conflicts.Add(1)
				}
			})
		}
		wg.Wait()

		assert.EqualValues(t, 1, wins.Load())
		assert.EqualValues(t, 19, conflicts.Load())
		got, err := f.messages.List(ctx, f.alice, conv)
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, 1, got[0].Seq)
		assert.Equal(t, 2, got[1].Seq)
	})

	t.Run("ten finished turns number 1 to 20 with no gaps", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		for range 10 {
			f.turn(t, f.alice, conv, "q", "a")
		}

		got, err := f.messages.List(ctx, f.alice, conv)
		require.NoError(t, err)
		require.Len(t, got, 20)
		for i, m := range got {
			assert.Equal(t, i+1, m.Seq)
		}
	})
}

func TestClaim(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	conv := f.conversation(t, f.alice)
	user, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "what is 2+2?")
	require.NoError(t, err)

	t.Run("another user cannot claim it", func(t *testing.T) {
		_, err := f.messages.Claim(ctx, f.bob, conv, reply.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
	t.Run("the wrong conversation is not found", func(t *testing.T) {
		_, err := f.messages.Claim(ctx, f.alice, f.conversation(t, f.alice), reply.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
	t.Run("a user message is not claimable", func(t *testing.T) {
		_, err := f.messages.Claim(ctx, f.alice, conv, user.ID)
		assert.ErrorIs(t, err, domain.ErrInvalidState)
	})

	t.Run("ten concurrent claims: one wins and gets the prompt, nine lose", func(t *testing.T) {
		var wins, losses atomic.Int32
		var winner resource.Claimed
		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() {
				c, err := f.messages.Claim(ctx, f.alice, conv, reply.ID)
				switch {
				case err == nil:
					wins.Add(1)
					winner = c
				case errors.Is(err, domain.ErrAlreadyStreaming):
					losses.Add(1)
				}
			})
		}
		wg.Wait()

		assert.EqualValues(t, 1, wins.Load())
		assert.EqualValues(t, 9, losses.Load())
		assert.Equal(t, "what is 2+2?", winner.Prompt)
		assert.Equal(t, domain.StatusStreaming, winner.Reply.Status)
		assert.Equal(t, reply.ID, winner.Reply.ID)
	})

	t.Run("a finished reply is an invalid state, not a streaming conflict", func(t *testing.T) {
		require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{Content: "4", Status: domain.StatusComplete}))

		_, err := f.messages.Claim(ctx, f.alice, conv, reply.ID)

		assert.ErrorIs(t, err, domain.ErrInvalidState)
	})
}

func TestFinish(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	newStreaming := func(t *testing.T) (conv uuid.UUID, reply domain.Message) {
		t.Helper()
		conv = f.conversation(t, f.alice)
		_, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "q")
		require.NoError(t, err)
		_, err = f.messages.Claim(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		return conv, reply
	}

	t.Run("records content, status and metadata", func(t *testing.T) {
		conv, reply := newStreaming(t)
		meta := domain.Meta{TTFTMs: 120, DurationMs: 3400, Usage: domain.Usage{Prompt: 10, Completion: 25}, ToolCalls: []string{"search"}}

		require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{
			Content: "# Answer\n\nforty-two", Status: domain.StatusComplete, Meta: meta,
		}))

		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		assert.Equal(t, "# Answer\n\nforty-two", got.Content)
		assert.Equal(t, domain.StatusComplete, got.Status)
		assert.Equal(t, meta, got.Meta)
		assert.Empty(t, got.Error)
	})

	t.Run("a stopped reply keeps its partial text", func(t *testing.T) {
		conv, reply := newStreaming(t)

		require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{Content: "half an ans", Status: domain.StatusStopped}))

		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.StatusStopped, got.Status)
		assert.Equal(t, "half an ans", got.Content)
	})

	t.Run("a failed reply records why", func(t *testing.T) {
		conv, reply := newStreaming(t)

		require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{Status: domain.StatusFailed, Error: "the agent is unavailable"}))

		got, err := f.messages.Get(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.StatusFailed, got.Status)
		assert.Equal(t, "the agent is unavailable", got.Error)
	})

	t.Run("empty metadata is stored as an empty object", func(t *testing.T) {
		_, reply := newStreaming(t)
		require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{Status: domain.StatusComplete}))

		var raw string
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT meta::text FROM messages WHERE id = $1`, reply.ID).Scan(&raw))
		assert.JSONEq(t, `{}`, raw)
	})

	t.Run("invalid transitions are refused", func(t *testing.T) {
		_, pending, err := f.messages.AppendPair(ctx, f.alice, f.conversation(t, f.alice), "q")
		require.NoError(t, err)
		_, done := newStreaming(t)
		require.NoError(t, f.messages.Finish(ctx, done.ID, resource.Outcome{Status: domain.StatusComplete}))
		_, streaming := newStreaming(t)

		tests := []struct {
			name string
			id   uuid.UUID
			out  resource.Outcome
			want error
		}{
			{"finishing a pending reply", pending.ID, resource.Outcome{Status: domain.StatusComplete}, domain.ErrInvalidState},
			{"finishing twice", done.ID, resource.Outcome{Status: domain.StatusFailed}, domain.ErrInvalidState},
			{"a non-terminal target status", streaming.ID, resource.Outcome{Status: domain.StatusPending}, domain.ErrInvalidState},
			{"an unknown status", streaming.ID, resource.Outcome{Status: "thinking"}, domain.ErrInvalidState},
			{"an unknown message", uuid.NewV7(), resource.Outcome{Status: domain.StatusComplete}, domain.ErrNotFound},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				assert.ErrorIs(t, f.messages.Finish(ctx, tc.id, tc.out), tc.want)
			})
		}
	})
}

func TestGetAndList(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	conv := f.conversation(t, f.alice)
	user, reply := f.turn(t, f.alice, conv, "q", "a")

	t.Run("list returns the messages in order", func(t *testing.T) {
		got, err := f.messages.List(ctx, f.alice, conv)

		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, user.ID, got[0].ID)
		assert.Equal(t, reply.ID, got[1].ID)
		assert.Equal(t, "a", got[1].Content)
	})

	t.Run("ownership is enforced everywhere", func(t *testing.T) {
		tests := []struct {
			name string
			run  func() error
		}{
			{"list as another user", func() error { _, err := f.messages.List(ctx, f.bob, conv); return err }},
			{"list an unknown conversation", func() error { _, err := f.messages.List(ctx, f.alice, uuid.NewV7()); return err }},
			{"get as another user", func() error { _, err := f.messages.Get(ctx, f.bob, conv, reply.ID); return err }},
			{"get through the wrong conversation", func() error {
				_, err := f.messages.Get(ctx, f.alice, f.conversation(t, f.alice), reply.ID)
				return err
			}},
			{"get an unknown message", func() error { _, err := f.messages.Get(ctx, f.alice, conv, uuid.NewV7()); return err }},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				assert.ErrorIs(t, tc.run(), domain.ErrNotFound)
			})
		}
	})

	t.Run("an empty conversation lists as empty, not missing", func(t *testing.T) {
		got, err := f.messages.List(ctx, f.alice, f.conversation(t, f.alice))

		require.NoError(t, err)
		assert.Empty(t, got)
	})
}

func TestReplaceLastAssistant(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	t.Run("swaps a finished reply for a new pending one with the same number", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, old := f.turn(t, f.alice, conv, "q", "first answer")

		fresh, err := f.messages.ReplaceLastAssistant(ctx, f.alice, conv, old.ID)

		require.NoError(t, err)
		assert.NotEqual(t, old.ID, fresh.ID)
		assert.Equal(t, old.Seq, fresh.Seq)
		assert.Equal(t, domain.StatusPending, fresh.Status)
		assert.Empty(t, fresh.Content)
		_, err = f.messages.Get(ctx, f.alice, conv, old.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound, "the old reply is gone")
		got, err := f.messages.List(ctx, f.alice, conv)
		require.NoError(t, err)
		assert.Len(t, got, 2, "the user's message is untouched")

		claimed, err := f.messages.Claim(ctx, f.alice, conv, fresh.ID)
		require.NoError(t, err)
		assert.Equal(t, "q", claimed.Prompt, "the new reply answers the same question")
	})

	t.Run("a failed or stopped reply can be replaced too", func(t *testing.T) {
		for _, status := range []domain.Status{domain.StatusFailed, domain.StatusStopped} {
			conv := f.conversation(t, f.alice)
			_, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "q")
			require.NoError(t, err)
			_, err = f.messages.Claim(ctx, f.alice, conv, reply.ID)
			require.NoError(t, err)
			require.NoError(t, f.messages.Finish(ctx, reply.ID, resource.Outcome{Status: status}))

			_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, conv, reply.ID)
			assert.NoError(t, err, string(status))
		}
	})

	t.Run("an unfinished reply is not replaceable", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "q")
		require.NoError(t, err)

		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, conv, reply.ID)
		assert.ErrorIs(t, err, domain.ErrInvalidState, "pending")

		_, err = f.messages.Claim(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, conv, reply.ID)
		assert.ErrorIs(t, err, domain.ErrInvalidState, "streaming")
	})

	t.Run("only the last reply can be replaced", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, first := f.turn(t, f.alice, conv, "q1", "a1")
		user2, second := f.turn(t, f.alice, conv, "q2", "a2")

		_, err := f.messages.ReplaceLastAssistant(ctx, f.alice, conv, first.ID)
		assert.ErrorIs(t, err, domain.ErrInvalidState, "an earlier reply")
		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, conv, user2.ID)
		assert.ErrorIs(t, err, domain.ErrInvalidState, "a user message is never replaced")

		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, conv, second.ID)
		assert.NoError(t, err, "the last one is fine")
		got, err := f.messages.List(ctx, f.alice, conv)
		require.NoError(t, err)
		assert.Len(t, got, 4, "and nothing else changed")
		assert.Equal(t, first.ID, got[1].ID)
	})

	t.Run("a message that is not in the conversation is not found", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		f.turn(t, f.alice, conv, "q", "a")
		other := f.conversation(t, f.alice)
		_, elsewhere := f.turn(t, f.alice, other, "q", "a")

		_, err := f.messages.ReplaceLastAssistant(ctx, f.alice, conv, uuid.NewV7())
		assert.ErrorIs(t, err, domain.ErrNotFound, "unknown")
		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, conv, elsewhere.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound, "belongs to another conversation")
		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, f.conversation(t, f.alice), uuid.NewV7())
		assert.ErrorIs(t, err, domain.ErrNotFound, "an empty conversation")
	})

	t.Run("ownership is enforced", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, reply := f.turn(t, f.alice, conv, "q", "a")

		_, err := f.messages.ReplaceLastAssistant(ctx, f.bob, conv, reply.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = f.messages.ReplaceLastAssistant(ctx, f.alice, uuid.NewV7(), reply.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	// The old reply goes and the new one comes in one transaction. Between two
	// statements of their own, a reader would find the question alone.
	t.Run("a reader never sees the conversation without its last reply", func(t *testing.T) {
		conv := f.conversation(t, f.alice)
		_, reply := f.turn(t, f.alice, conv, "q", "a")
		const swaps, readers = 100, 4
		var (
			done    atomic.Bool
			reading sync.WaitGroup // released when every reader has looked once
			wg      sync.WaitGroup
			wrong   = make([]string, readers) // the first wrong view of each reader
		)
		reading.Add(readers)
		for i := range readers {
			wg.Go(func() {
				looked := sync.OnceFunc(reading.Done)
				for !done.Load() {
					got, err := f.messages.List(ctx, f.alice, conv)
					if wrong[i] == "" && (err != nil || len(got) != 2 || got[1].Role != domain.RoleAssistant) {
						wrong[i] = fmt.Sprintf("%d messages (error: %v)", len(got), err)
					}
					looked()
				}
			})
		}
		reading.Wait()

		swapAgainAndAgain := func() error {
			last := reply.ID
			for range swaps {
				fresh, err := f.messages.ReplaceLastAssistant(ctx, f.alice, conv, last)
				if err != nil {
					return err
				}
				if _, err := f.messages.Claim(ctx, f.alice, conv, fresh.ID); err != nil {
					return err
				}
				if err := f.messages.Finish(ctx, fresh.ID, resource.Outcome{Content: "a", Status: domain.StatusComplete}); err != nil {
					return err
				}
				last = fresh.ID
			}
			return nil
		}
		err := swapAgainAndAgain()
		done.Store(true)
		wg.Wait()

		require.NoError(t, err)
		for _, view := range wrong {
			assert.Empty(t, view, "every view holds the question and one reply")
		}
	})
}

func TestRecoverInterrupted(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	streaming := func(t *testing.T) domain.Message {
		t.Helper()
		conv := f.conversation(t, f.alice)
		_, reply, err := f.messages.AppendPair(ctx, f.alice, conv, "q")
		require.NoError(t, err)
		_, err = f.messages.Claim(ctx, f.alice, conv, reply.ID)
		require.NoError(t, err)
		return reply
	}
	age := func(t *testing.T, id uuid.UUID, by time.Duration) {
		t.Helper()
		_, err := f.pool.Exec(ctx, `UPDATE messages SET updated_at = now() - $2::interval WHERE id = $1`, id, by.String())
		require.NoError(t, err)
	}
	status := func(t *testing.T, id uuid.UUID) (domain.Status, string) {
		t.Helper()
		var s, e string
		require.NoError(t, f.pool.QueryRow(ctx, `SELECT status, error FROM messages WHERE id = $1`, id).Scan(&s, &e))
		return domain.Status(s), e
	}

	stale, fresh := streaming(t), streaming(t)
	age(t, stale.ID, 10*time.Minute)
	_, pending, err := f.messages.AppendPair(ctx, f.alice, f.conversation(t, f.alice), "q")
	require.NoError(t, err)
	_, done := f.turn(t, f.alice, f.conversation(t, f.alice), "q", "a")

	n, err := f.messages.RecoverInterrupted(ctx, 5*time.Minute)

	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the stale one")
	s, e := status(t, stale.ID)
	assert.Equal(t, domain.StatusFailed, s)
	assert.Equal(t, domain.FailureInterrupted, e)
	s, _ = status(t, fresh.ID)
	assert.Equal(t, domain.StatusStreaming, s, "a recent reply may still be running")
	s, _ = status(t, pending.ID)
	assert.Equal(t, domain.StatusPending, s, "pending replies can still be run")
	s, _ = status(t, done.ID)
	assert.Equal(t, domain.StatusComplete, s)

	n, err = f.messages.RecoverInterrupted(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "at startup, with no grace, every remaining streaming reply is stale")
	s, _ = status(t, fresh.ID)
	assert.Equal(t, domain.StatusFailed, s)
}

// A call whose context has ended gives up with the context's error and changes
// nothing: the same call then does its work on a context that is alive.
func TestMessageStoreHonorsContext(t *testing.T) {
	f := newFixture(t)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	// A conversation for each call to work on, in the state the call needs.
	empty := f.conversation(t, f.alice)
	read := f.conversation(t, f.alice)
	_, answered := f.turn(t, f.alice, read, "q", "a")
	waiting := f.conversation(t, f.alice)
	_, pending, err := f.messages.AppendPair(t.Context(), f.alice, waiting, "q")
	require.NoError(t, err)
	writing := f.conversation(t, f.alice)
	_, streaming, err := f.messages.AppendPair(t.Context(), f.alice, writing, "q")
	require.NoError(t, err)
	_, err = f.messages.Claim(t.Context(), f.alice, writing, streaming.ID)
	require.NoError(t, err)
	swapped := f.conversation(t, f.alice)
	_, finished := f.turn(t, f.alice, swapped, "q", "a")

	tests := []struct {
		name string
		call func(ctx context.Context) error
	}{
		{"AppendPair", func(ctx context.Context) error {
			_, _, err := f.messages.AppendPair(ctx, f.alice, empty, "q")
			return err
		}},
		{"List", func(ctx context.Context) error {
			_, err := f.messages.List(ctx, f.alice, read)
			return err
		}},
		{"Get", func(ctx context.Context) error {
			_, err := f.messages.Get(ctx, f.alice, read, answered.ID)
			return err
		}},
		{"Claim", func(ctx context.Context) error {
			_, err := f.messages.Claim(ctx, f.alice, waiting, pending.ID)
			return err
		}},
		{"Finish", func(ctx context.Context) error {
			return f.messages.Finish(ctx, streaming.ID, resource.Outcome{Status: domain.StatusComplete})
		}},
		{"ReplaceLastAssistant", func(ctx context.Context) error {
			_, err := f.messages.ReplaceLastAssistant(ctx, f.alice, swapped, finished.ID)
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, tc.call(canceled), context.Canceled)
			require.NoError(t, tc.call(t.Context()), "the same call works on a context that is alive")
		})
	}
}
