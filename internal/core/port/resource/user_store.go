package resource

import (
	"context"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

// UserStore persists user accounts. Emails are stored lowercase.
//
//go:generate mockery --name UserStore --filename user_store.go
type UserStore interface {
	// Create stores u and returns it with its creation time. An existing email
	// is domain.ErrEmailTaken.
	Create(ctx context.Context, u domain.User) (domain.User, error)

	// ByEmail returns the user with this (already normalized) email, or
	// domain.ErrNotFound.
	ByEmail(ctx context.Context, email string) (domain.User, error)

	// ByID returns the user, or domain.ErrNotFound.
	ByID(ctx context.Context, id uuid.UUID) (domain.User, error)

	// Count returns the number of accounts.
	Count(ctx context.Context) (int, error)
}
