package service

import (
	"context"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

// AuthSession is what a successful login hands to the HTTP adapter: the cookie
// value to set and when it stops being valid.
type AuthSession struct {
	Token     string
	ExpiresAt time.Time
	User      domain.User
}

// Auth is the sign-in use case.
//
//go:generate mockery --name Auth --filename auth.go
type Auth interface {
	// Login checks the credentials and starts a session. Any failure to
	// authenticate is the same domain.ErrInvalidCredentials, whether the email
	// is unknown or the password is wrong. When the server is too busy to check
	// the password in time, the error wraps domain.ErrBusy, for every email alike.
	Login(ctx context.Context, email, password string) (AuthSession, error)

	// Authenticate resolves a cookie token to its user. An unknown, expired or
	// malformed token is domain.ErrInvalidCredentials.
	Authenticate(ctx context.Context, token string) (domain.User, error)

	// Logout ends the session. An unknown token is not an error.
	Logout(ctx context.Context, token string) error

	// BootstrapAdmin creates the first user when no user exists yet and reports
	// whether it did. Bad input is a *domain.ValidationError.
	BootstrapAdmin(ctx context.Context, email, password string) (created bool, err error)
}
