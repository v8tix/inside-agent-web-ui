package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource"
)

var _ resource.SessionStore = (*SessionStore)(nil)

// SessionStore is the PostgreSQL implementation of resource.SessionStore.
type SessionStore struct {
	pool *pgxpool.Pool
	sql  sessionSQL
}

// NewSessionStore returns a store backed by pool, with its SQL from sql.
func NewSessionStore(pool *pgxpool.Pool, sql Statements) *SessionStore {
	return &SessionStore{pool: pool, sql: sql.session}
}

// Create implements resource.SessionStore.
func (s *SessionStore) Create(ctx context.Context, sess domain.Session) error {
	if _, err := s.pool.Exec(ctx, s.sql.create, sess.ID, sess.UserID, sess.ExpiresAt); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	return nil
}

// Get implements resource.SessionStore.
func (s *SessionStore) Get(ctx context.Context, id string) (domain.Session, error) {
	var sess domain.Session
	err := s.pool.QueryRow(ctx, s.sql.get, id).
		Scan(&sess.ID, &sess.UserID, &sess.ExpiresAt, &sess.CreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Session{}, domain.ErrNotFound
	case err != nil:
		return domain.Session{}, fmt.Errorf("reading session: %w", err)
	}
	return sess, nil
}

// Delete implements resource.SessionStore.
func (s *SessionStore) Delete(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, s.sql.remove, id); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}

// DeleteExpired implements resource.SessionStore.
func (s *SessionStore) DeleteExpired(ctx context.Context, now time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, s.sql.removeExpired, now)
	if err != nil {
		return 0, fmt.Errorf("deleting expired sessions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
