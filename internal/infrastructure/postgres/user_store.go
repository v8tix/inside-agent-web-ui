package postgres

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource"
)

var _ resource.UserStore = (*UserStore)(nil)

// UserStore is the PostgreSQL implementation of resource.UserStore.
type UserStore struct {
	pool *pgxpool.Pool
	sql  userSQL
}

// NewUserStore returns a store backed by pool, with its SQL from sql.
func NewUserStore(pool *pgxpool.Pool, sql Statements) *UserStore {
	return &UserStore{pool: pool, sql: sql.user}
}

func scanUser(row pgx.Row) (domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	return u, err
}

// Create implements resource.UserStore.
//
// Pattern: Named Constraints
func (s *UserStore) Create(ctx context.Context, u domain.User) (domain.User, error) {
	created, err := scanUser(s.pool.QueryRow(ctx, s.sql.create, u.ID, u.Email, u.PasswordHash))
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.ConstraintName == "users_email_key" {
		return domain.User{}, domain.ErrEmailTaken
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("creating user: %w", err)
	}
	return created, nil
}

// ByEmail implements resource.UserStore.
func (s *UserStore) ByEmail(ctx context.Context, email string) (domain.User, error) {
	return s.one(ctx, s.sql.byEmail, email)
}

// ByID implements resource.UserStore.
func (s *UserStore) ByID(ctx context.Context, id uuid.UUID) (domain.User, error) {
	return s.one(ctx, s.sql.byID, id)
}

func (s *UserStore) one(ctx context.Context, query string, arg any) (domain.User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, query, arg))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.User{}, domain.ErrNotFound
	case err != nil:
		return domain.User{}, fmt.Errorf("reading user: %w", err)
	}
	return u, nil
}

// Count implements resource.UserStore.
func (s *UserStore) Count(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, s.sql.count).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting users: %w", err)
	}
	return n, nil
}
