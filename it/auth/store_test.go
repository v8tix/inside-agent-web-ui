// Integration tests for the user and session stores. Group A: needs Docker.
package auth_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/postgres"
	"github.com/v8tix/inside-agent-web-ui/it/internal/dbtest"
)

func TestUserStore(t *testing.T) {
	pool := dbtest.Pool(t)
	users := postgres.NewUserStore(pool, dbtest.Statements(t))
	ctx := t.Context()

	t.Run("an empty database has no users", func(t *testing.T) {
		n, err := users.Count(ctx)
		require.NoError(t, err)
		assert.Zero(t, n)
	})

	var alice domain.User
	t.Run("create returns the stored user", func(t *testing.T) {
		created, err := users.Create(ctx, domain.User{ID: uuid.NewV7(), Email: "alice@example.com", PasswordHash: "hash-1"})

		require.NoError(t, err)
		assert.False(t, created.CreatedAt.IsZero())
		assert.Equal(t, "alice@example.com", created.Email)
		alice = created
	})

	t.Run("lookups by email and by id", func(t *testing.T) {
		byEmail, err := users.ByEmail(ctx, "alice@example.com")
		require.NoError(t, err)
		assert.Equal(t, alice.ID, byEmail.ID)
		assert.Equal(t, "hash-1", byEmail.PasswordHash)

		byID, err := users.ByID(ctx, alice.ID)
		require.NoError(t, err)
		assert.Equal(t, "alice@example.com", byID.Email)
	})

	t.Run("unknown users are not found", func(t *testing.T) {
		_, err := users.ByEmail(ctx, "ghost@example.com")
		assert.ErrorIs(t, err, domain.ErrNotFound)
		_, err = users.ByID(ctx, uuid.NewV7())
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("the email is unique", func(t *testing.T) {
		_, err := users.Create(ctx, domain.User{ID: uuid.NewV7(), Email: "alice@example.com", PasswordHash: "other"})

		assert.ErrorIs(t, err, domain.ErrEmailTaken)
	})

	t.Run("an email that is not normalized is refused loudly, not stored", func(t *testing.T) {
		_, err := users.Create(ctx, domain.User{ID: uuid.NewV7(), Email: "Bob@Example.com", PasswordHash: "hash"})

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrEmailTaken, "this is a programming error, not a duplicate")
	})

	t.Run("a duplicate id is not reported as a taken email", func(t *testing.T) {
		_, err := users.Create(ctx, domain.User{ID: alice.ID, Email: "different@example.com", PasswordHash: "hash"})

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrEmailTaken)
	})

	t.Run("count follows creation", func(t *testing.T) {
		n, err := users.Count(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
	})

	t.Run("racing creates of one email: exactly one wins", func(t *testing.T) {
		var wins, taken atomic.Int32
		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() {
				_, err := users.Create(ctx, domain.User{ID: uuid.NewV7(), Email: "race@example.com", PasswordHash: "h"})
				switch {
				case err == nil:
					wins.Add(1)
				case errors.Is(err, domain.ErrEmailTaken):
					taken.Add(1)
				}
			})
		}
		wg.Wait()

		assert.EqualValues(t, 1, wins.Load())
		assert.EqualValues(t, 9, taken.Load())
	})
}

func TestSessionStore(t *testing.T) {
	pool := dbtest.Pool(t)
	sessions := postgres.NewSessionStore(pool, dbtest.Statements(t))
	userID := dbtest.SeedUser(t, pool, "alice@example.com")
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)

	create := func(t *testing.T, id string, expires time.Time) {
		t.Helper()
		require.NoError(t, sessions.Create(ctx, domain.Session{ID: id, UserID: userID, ExpiresAt: expires}))
	}

	t.Run("create and get", func(t *testing.T) {
		create(t, "sess-1", now.Add(time.Hour))

		got, err := sessions.Get(ctx, "sess-1")

		require.NoError(t, err)
		assert.Equal(t, "sess-1", got.ID)
		assert.Equal(t, userID, got.UserID)
		assert.WithinDuration(t, now.Add(time.Hour), got.ExpiresAt, time.Millisecond)
		assert.False(t, got.CreatedAt.IsZero())
	})

	t.Run("get does not check expiry: that is the service's decision", func(t *testing.T) {
		create(t, "sess-old", now.Add(-time.Hour))

		got, err := sessions.Get(ctx, "sess-old")

		require.NoError(t, err)
		assert.True(t, got.ExpiresAt.Before(now))
	})

	t.Run("unknown sessions are not found", func(t *testing.T) {
		_, err := sessions.Get(ctx, "nope")
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("a duplicate id is refused", func(t *testing.T) {
		assert.Error(t, sessions.Create(ctx, domain.Session{ID: "sess-1", UserID: userID, ExpiresAt: now.Add(time.Hour)}))
	})

	t.Run("a session needs a real user", func(t *testing.T) {
		assert.Error(t, sessions.Create(ctx, domain.Session{ID: "orphan", UserID: uuid.NewV7(), ExpiresAt: now.Add(time.Hour)}))
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		create(t, "sess-del", now.Add(time.Hour))

		require.NoError(t, sessions.Delete(ctx, "sess-del"))
		require.NoError(t, sessions.Delete(ctx, "sess-del"), "deleting again is not an error")

		_, err := sessions.Get(ctx, "sess-del")
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("delete expired removes only what has expired, inclusive of the boundary", func(t *testing.T) {
		create(t, "exp-past", now.Add(-time.Minute))
		create(t, "exp-edge", now)
		create(t, "exp-future", now.Add(time.Minute))

		n, err := sessions.DeleteExpired(ctx, now)

		require.NoError(t, err)
		assert.Equal(t, 3, n, "sess-old, exp-past and exp-edge")
		_, err = sessions.Get(ctx, "exp-future")
		assert.NoError(t, err)
		_, err = sessions.Get(ctx, "exp-edge")
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("deleting a user ends their sessions", func(t *testing.T) {
		create(t, "sess-cascade", now.Add(time.Hour))

		_, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		require.NoError(t, err)

		_, err = sessions.Get(ctx, "sess-cascade")
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})
}
