package resource

import (
	"context"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

// SessionStore persists login sessions, keyed by the digest of the cookie token.
//
//go:generate mockery --name SessionStore --filename session_store.go
type SessionStore interface {
	// Create stores a session.
	Create(ctx context.Context, s domain.Session) error

	// Get returns the session, or domain.ErrNotFound. It does not check expiry.
	Get(ctx context.Context, id string) (domain.Session, error)

	// Delete removes a session. Removing a missing one is not an error.
	Delete(ctx context.Context, id string) error

	// DeleteExpired removes sessions that expired at or before now and returns
	// how many it removed.
	DeleteExpired(ctx context.Context, now time.Time) (int, error)
}
