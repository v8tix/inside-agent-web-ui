package postgres_test

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/postgres"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
	"github.com/v8tix/inside-agent-web-ui/it/internal/pgenv"
)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := postgres.NewPool(t.Context(), config.DB{URL: envcfg.Secret(pgenv.NewDatabase(t)), MaxConns: 5})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func tableNames(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name <> 'schema_migrations'
		ORDER BY table_name`)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	return names
}

func TestMigrateSuite(t *testing.T) {
	t.Run("up applies on an empty database, down reverts, up again works", func(t *testing.T) {
		pool := newPool(t)
		m, err := postgres.NewMigrator(pool)
		require.NoError(t, err)
		defer func() { require.NoError(t, m.Close()) }()

		require.NoError(t, m.Up())
		version, dirty, err := m.Version()
		require.NoError(t, err)
		assert.Equal(t, uint(1), version)
		assert.False(t, dirty)
		assert.Equal(t, []string{"conversations", "messages", "sessions", "users"}, tableNames(t, pool))

		require.NoError(t, m.Up(), "running Up with nothing pending is not an error")

		require.NoError(t, m.Down())
		assert.Empty(t, tableNames(t, pool))
		version, _, err = m.Version()
		require.NoError(t, err)
		assert.Zero(t, version)

		require.NoError(t, m.Up())
		assert.Len(t, tableNames(t, pool), 4)
	})

	t.Run("two instances starting together both succeed", func(t *testing.T) {
		pool := newPool(t)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Go(func() { errs[i] = postgres.Migrate(pool) })
		}
		wg.Wait()

		assert.NoError(t, errs[0])
		assert.NoError(t, errs[1])
		assert.Len(t, tableNames(t, pool), 4)
	})
}

func TestSchemaConstraints(t *testing.T) {
	pool := newPool(t)
	require.NoError(t, postgres.Migrate(pool))
	ctx := t.Context()

	const (
		userID = "00000000-0000-0000-0000-000000000001"
		convID = "00000000-0000-0000-0000-0000000000c1"
	)
	exec := func(sql string, args ...any) error {
		_, err := pool.Exec(ctx, sql, args...)
		return err
	}
	require.NoError(t, exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, 'a@example.com', 'hash')`, userID))
	require.NoError(t, exec(`INSERT INTO conversations (id, user_id, title) VALUES ($1, $2, 'First')`, convID, userID))

	insertMessage := func(id string, seq int, role, status string) error {
		return exec(`INSERT INTO messages (id, conversation_id, seq, role, status) VALUES ($1, $2, $3, $4, $5)`,
			id, convID, seq, role, status)
	}
	require.NoError(t, insertMessage("00000000-0000-0000-0000-00000000a001", 1, "user", "complete"))

	tests := []struct {
		name      string
		run       func() error
		wantInErr string
	}{
		{"email must be lowercase", func() error {
			return exec(`INSERT INTO users (id, email, password_hash) VALUES (gen_random_uuid(), 'B@Example.com', 'hash')`)
		}, "users_email_normalized"},
		{"email must be unique", func() error {
			return exec(`INSERT INTO users (id, email, password_hash) VALUES (gen_random_uuid(), 'a@example.com', 'hash')`)
		}, "users_email_key"},
		{"password hash must be present", func() error {
			return exec(`INSERT INTO users (id, email, password_hash) VALUES (gen_random_uuid(), 'c@example.com', '')`)
		}, "users_password_hash_present"},
		{"conversation title cannot be empty", func() error {
			return exec(`INSERT INTO conversations (id, user_id, title) VALUES (gen_random_uuid(), $1, '')`, userID)
		}, "conversations_title_length"},
		{"conversation title is capped at 120 characters", func() error {
			return exec(`INSERT INTO conversations (id, user_id, title) VALUES (gen_random_uuid(), $1, repeat('x', 121))`, userID)
		}, "conversations_title_length"},
		{"message role must be known", func() error {
			return insertMessage("00000000-0000-0000-0000-00000000a002", 2, "system", "complete")
		}, "messages_role_known"},
		{"message status must be known", func() error {
			return insertMessage("00000000-0000-0000-0000-00000000a003", 2, "assistant", "thinking")
		}, "messages_status_known"},
		{"message seq must be positive", func() error {
			return insertMessage("00000000-0000-0000-0000-00000000a004", 0, "assistant", "pending")
		}, "messages_seq_positive"},
		{"message seq is unique per conversation", func() error {
			return insertMessage("00000000-0000-0000-0000-00000000a005", 1, "assistant", "pending")
		}, "messages_conversation_seq_key"},
		{"a message needs its conversation", func() error {
			return exec(`INSERT INTO messages (id, conversation_id, seq, role, status) VALUES (gen_random_uuid(), gen_random_uuid(), 1, 'user', 'complete')`)
		}, "violates foreign key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantInErr)
		})
	}

	t.Run("deleting a user removes their sessions, conversations and messages", func(t *testing.T) {
		require.NoError(t, exec(`INSERT INTO sessions (id, user_id, expires_at) VALUES ('abc', $1, now() + interval '1 hour')`, userID))
		require.NoError(t, exec(`DELETE FROM users WHERE id = $1`, userID))

		for _, table := range []string{"sessions", "conversations", "messages"} {
			var n int
			require.NoError(t, pool.QueryRow(context.WithoutCancel(ctx), `SELECT count(*) FROM `+table).Scan(&n))
			assert.Zero(t, n, table)
		}
	})
}
