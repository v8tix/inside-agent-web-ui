package resource

import "context"

// PasswordHasher hashes and verifies passwords.
//
//go:generate mockery --name PasswordHasher --filename password_hasher.go
type PasswordHasher interface {
	// Hash returns a self-describing, salted hash of password. If the hasher is
	// too busy to start within its patience the error wraps domain.ErrBusy, and if
	// ctx ends first it is ctx's error.
	Hash(ctx context.Context, password string) (string, error)

	// Verify reports whether password matches hash. A malformed hash never
	// matches. The error is not about the password: it says the check was not
	// made, and then the answer means nothing. It wraps domain.ErrBusy or is ctx's
	// error, as for Hash.
	Verify(ctx context.Context, password, hash string) (bool, error)
}
