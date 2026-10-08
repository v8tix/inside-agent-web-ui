// Integration tests for the conversation store. Group A: needs Docker.
package conversation_test

import (
	"context"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/postgres"
	"github.com/v8tix/inside-agent-web-ui/it/internal/dbtest"
)

func TestConversationStoreSuite(t *testing.T) {
	pool := dbtest.Pool(t)
	store := postgres.NewConversationStore(pool, dbtest.Statements(t))
	alice := dbtest.SeedUser(t, pool, "alice@example.com")
	bob := dbtest.SeedUser(t, pool, "bob@example.com")
	ctx := t.Context()

	create := func(t *testing.T, owner uuid.UUID, title string) domain.Conversation {
		t.Helper()
		c, err := store.Create(ctx, domain.Conversation{ID: uuid.NewV7(), UserID: owner, Title: title})
		require.NoError(t, err)
		return c
	}
	touch := func(t *testing.T, id uuid.UUID, ago time.Duration) {
		t.Helper()
		_, err := pool.Exec(ctx, `UPDATE conversations SET updated_at = now() - $2::interval WHERE id = $1`,
			id, ago.String())
		require.NoError(t, err)
	}

	t.Run("create returns the stored row and get reads it back", func(t *testing.T) {
		created := create(t, alice, "Hello")

		assert.False(t, created.CreatedAt.IsZero())
		assert.False(t, created.UpdatedAt.IsZero())
		got, err := store.Get(ctx, alice, created.ID)
		require.NoError(t, err)
		assert.Equal(t, created.ID, got.ID)
		assert.Equal(t, alice, got.UserID)
		assert.Equal(t, "Hello", got.Title)
		assert.WithinDuration(t, created.CreatedAt, got.CreatedAt, time.Millisecond)
	})

	t.Run("create rejects a duplicate id and a title the database refuses", func(t *testing.T) {
		c := create(t, alice, "First")

		_, err := store.Create(ctx, domain.Conversation{ID: c.ID, UserID: alice, Title: "Again"})
		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrNotFound)

		_, err = store.Create(ctx, domain.Conversation{ID: uuid.NewV7(), UserID: alice, Title: strings.Repeat("x", 121)})
		require.Error(t, err)
	})

	t.Run("another user's conversation looks like a missing one", func(t *testing.T) {
		c := create(t, alice, "Private")

		tests := []struct {
			name string
			run  func() error
		}{
			{"get", func() error { _, err := store.Get(ctx, bob, c.ID); return err }},
			{"rename", func() error { return store.Rename(ctx, bob, c.ID, "Mine now") }},
			{"delete", func() error { return store.Delete(ctx, bob, c.ID) }},
			{"get unknown id", func() error { _, err := store.Get(ctx, alice, uuid.NewV7()); return err }},
			{"rename unknown id", func() error { return store.Rename(ctx, alice, uuid.NewV7(), "x") }},
			{"delete unknown id", func() error { return store.Delete(ctx, alice, uuid.NewV7()) }},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				assert.ErrorIs(t, tc.run(), domain.ErrNotFound)
			})
		}

		got, err := store.Get(ctx, alice, c.ID)
		require.NoError(t, err, "the owner's conversation is untouched")
		assert.Equal(t, "Private", got.Title)
	})

	t.Run("list is the user's own, most recently active first, and respects the limit", func(t *testing.T) {
		carol := dbtest.SeedUser(t, pool, "carol@example.com")
		old := create(t, carol, "old")
		fresh := create(t, carol, "fresh")
		middle := create(t, carol, "middle")
		create(t, bob, "not carol's")
		touch(t, old.ID, 3*time.Hour)
		touch(t, middle.ID, 1*time.Hour)

		all, err := store.List(ctx, carol, 100)
		require.NoError(t, err)
		titles := make([]string, len(all))
		for i, c := range all {
			titles[i] = c.Title
		}
		assert.Equal(t, []string{"fresh", "middle", "old"}, titles)
		assert.Equal(t, fresh.ID, all[0].ID)

		limited, err := store.List(ctx, carol, 2)
		require.NoError(t, err)
		assert.Len(t, limited, 2)

		none, err := store.List(ctx, dbtest.SeedUser(t, pool, "dave@example.com"), 10)
		require.NoError(t, err)
		assert.Empty(t, none)
	})

	t.Run("rename changes the title but not the order", func(t *testing.T) {
		erin := dbtest.SeedUser(t, pool, "erin@example.com")
		first := create(t, erin, "first")
		second := create(t, erin, "second")
		touch(t, first.ID, time.Hour)
		before, err := store.Get(ctx, erin, first.ID)
		require.NoError(t, err)

		require.NoError(t, store.Rename(ctx, erin, first.ID, "renamed"))

		after, err := store.Get(ctx, erin, first.ID)
		require.NoError(t, err)
		assert.Equal(t, "renamed", after.Title)
		assert.True(t, before.UpdatedAt.Equal(after.UpdatedAt), "activity time is unchanged")
		list, err := store.List(ctx, erin, 10)
		require.NoError(t, err)
		assert.Equal(t, second.ID, list[0].ID)

		assert.Error(t, store.Rename(ctx, erin, first.ID, ""), "the database refuses an empty title")
	})

	t.Run("delete removes the conversation and its messages", func(t *testing.T) {
		c := create(t, alice, "to delete")
		_, err := pool.Exec(ctx,
			`INSERT INTO messages (id, conversation_id, seq, role, status) VALUES ($1, $2, 1, 'user', 'complete')`,
			uuid.NewV7(), c.ID)
		require.NoError(t, err)

		require.NoError(t, store.Delete(ctx, alice, c.ID))

		_, err = store.Get(ctx, alice, c.ID)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE conversation_id = $1`, c.ID).Scan(&n))
		assert.Zero(t, n)
		assert.ErrorIs(t, store.Delete(ctx, alice, c.ID), domain.ErrNotFound, "deleting twice")
	})

	t.Run("a canceled context is an error, not a not-found", func(t *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()

		_, err := store.Get(canceled, alice, uuid.NewV7())

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrNotFound)
		assert.ErrorIs(t, err, context.Canceled)
	})
}
