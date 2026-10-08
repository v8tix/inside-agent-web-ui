package auth_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/auth"
	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource/mocks"
)

const (
	dummyHash = "dummy-hash"
	ttl       = 24 * time.Hour
)

var now = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

type harness struct {
	users    *mocks.UserStore
	sessions *mocks.SessionStore
	hasher   *mocks.PasswordHasher
	logs     *bytes.Buffer
	svc      *auth.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		users:    mocks.NewUserStore(t),
		sessions: mocks.NewSessionStore(t),
		hasher:   mocks.NewPasswordHasher(t),
		logs:     &bytes.Buffer{},
	}
	h.hasher.On("Hash", mock.Anything, mock.Anything).Return(dummyHash, nil).Once() // the decoy hash made at construction
	svc, err := auth.NewService(auth.Dependencies{
		Users:      h.users,
		Sessions:   h.sessions,
		Hasher:     h.hasher,
		Logger:     slog.New(slog.NewTextHandler(h.logs, nil)),
		SessionTTL: ttl,
		Now:        func() time.Time { return now },
	})
	require.NoError(t, err)
	h.svc = svc
	return h
}

func alice() domain.User {
	return domain.User{ID: uuid.NewV7(), Email: "alice@example.com", PasswordHash: "alice-hash"}
}

func digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func TestNewService(t *testing.T) {
	t.Run("a hasher failure at construction is reported", func(t *testing.T) {
		hasher := mocks.NewPasswordHasher(t)
		hasher.On("Hash", mock.Anything, mock.Anything).Return("", errors.New("no entropy"))

		_, err := auth.NewService(auth.Dependencies{Hasher: hasher, Users: mocks.NewUserStore(t), Sessions: mocks.NewSessionStore(t)})

		require.Error(t, err)
	})
}

func TestLogin_Success(t *testing.T) {
	h := newHarness(t)
	user := alice()
	var stored domain.Session
	h.users.On("ByEmail", mock.Anything, "alice@example.com").Return(user, nil)
	h.hasher.On("Verify", mock.Anything, "correct password", "alice-hash").Return(true, nil)
	h.sessions.On("Create", mock.Anything, mock.MatchedBy(func(s domain.Session) bool { stored = s; return true })).Return(nil)
	h.sessions.On("DeleteExpired", mock.Anything, now).Return(3, nil)

	got, err := h.svc.Login(t.Context(), "  ALICE@Example.com ", "correct password")

	require.NoError(t, err)
	assert.Equal(t, user, got.User)
	assert.Equal(t, now.Add(ttl), got.ExpiresAt)
	assert.Regexp(t, `^[A-Za-z0-9_-]{43}$`, got.Token, "32 random bytes, URL-safe base64")
	assert.Equal(t, digest(got.Token), stored.ID, "only the token's digest is stored, never the token")
	assert.NotEqual(t, got.Token, stored.ID)
	assert.Equal(t, user.ID, stored.UserID)
	assert.Equal(t, now.Add(ttl), stored.ExpiresAt)
}

func TestLogin_TokensAreUnique(t *testing.T) {
	h := newHarness(t)
	h.users.On("ByEmail", mock.Anything, mock.Anything).Return(alice(), nil)
	h.hasher.On("Verify", mock.Anything, mock.Anything, mock.Anything).Return(true, nil)
	h.sessions.On("Create", mock.Anything, mock.Anything).Return(nil)
	h.sessions.On("DeleteExpired", mock.Anything, mock.Anything).Return(0, nil)

	seen := map[string]bool{}
	for range 50 {
		s, err := h.svc.Login(t.Context(), "alice@example.com", "pw")
		require.NoError(t, err)
		require.False(t, seen[s.Token], "token reused")
		seen[s.Token] = true
	}
}

func TestLogin_Failures(t *testing.T) {
	tests := []struct {
		name         string
		email        string
		password     string
		setup        func(h *harness)
		wantDummy    bool // a decoy verification keeps the timing of "unknown user" close to "wrong password"
		wantNoLookup bool
	}{
		{
			name: "wrong password", email: "alice@example.com", password: "nope",
			setup: func(h *harness) {
				h.users.On("ByEmail", mock.Anything, "alice@example.com").Return(alice(), nil)
				h.hasher.On("Verify", mock.Anything, "nope", "alice-hash").Return(false, nil)
			},
		},
		{
			name: "unknown user", email: "ghost@example.com", password: "pw", wantDummy: true,
			setup: func(h *harness) {
				h.users.On("ByEmail", mock.Anything, "ghost@example.com").Return(domain.User{}, domain.ErrNotFound)
				h.hasher.On("Verify", mock.Anything, "pw", dummyHash).Return(false, nil)
			},
		},
		{
			name: "malformed email", email: "not an email", password: "pw", wantDummy: true, wantNoLookup: true,
			setup: func(h *harness) { h.hasher.On("Verify", mock.Anything, "pw", dummyHash).Return(false, nil) },
		},
		{
			name: "empty email", email: "", password: "pw", wantDummy: true, wantNoLookup: true,
			setup: func(h *harness) { h.hasher.On("Verify", mock.Anything, "pw", dummyHash).Return(false, nil) },
		},
		{
			name: "an absurdly long password is refused without hashing", email: "alice@example.com",
			password: strings.Repeat("x", 5000), wantNoLookup: true,
			setup: func(h *harness) {},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t) // no session may be created on any failure: the mock fails the test if so
			tc.setup(h)

			_, err := h.svc.Login(t.Context(), tc.email, tc.password)

			assert.ErrorIs(t, err, domain.ErrInvalidCredentials)
			if tc.wantNoLookup {
				h.users.AssertNotCalled(t, "ByEmail", mock.Anything, mock.Anything)
			}
			if tc.wantDummy {
				h.hasher.AssertCalled(t, "Verify", mock.Anything, tc.password, dummyHash)
			}
		})
	}

	t.Run("a check that could not be made is an error, not a wrong password, whoever asks", func(t *testing.T) {
		// Otherwise a busy server would tell which emails have an account: those
		// that have one would get an error, and the others a wrong password.
		noTurn := errors.New("no turn came")
		tests := []struct {
			name, email string
			setup       func(h *harness)
		}{
			{"an email with an account", "alice@example.com", func(h *harness) {
				h.users.On("ByEmail", mock.Anything, "alice@example.com").Return(alice(), nil)
				h.hasher.On("Verify", mock.Anything, "pw", "alice-hash").Return(false, noTurn)
			}},
			{"an email with none", "ghost@example.com", func(h *harness) {
				h.users.On("ByEmail", mock.Anything, "ghost@example.com").Return(domain.User{}, domain.ErrNotFound)
				h.hasher.On("Verify", mock.Anything, "pw", dummyHash).Return(false, noTurn)
			}},
			{"a malformed email", "not an email", func(h *harness) {
				h.hasher.On("Verify", mock.Anything, "pw", dummyHash).Return(false, noTurn)
			}},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				h := newHarness(t)
				tc.setup(h)

				_, err := h.svc.Login(t.Context(), tc.email, "pw")

				require.ErrorIs(t, err, noTurn)
				assert.NotErrorIs(t, err, domain.ErrInvalidCredentials)
			})
		}
	})

	t.Run("a broken user store is an error, not a wrong password", func(t *testing.T) {
		h := newHarness(t)
		h.users.On("ByEmail", mock.Anything, mock.Anything).Return(domain.User{}, errors.New("db down"))

		_, err := h.svc.Login(t.Context(), "alice@example.com", "pw")

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrInvalidCredentials)
	})

	t.Run("a session that cannot be saved is an error", func(t *testing.T) {
		h := newHarness(t)
		h.users.On("ByEmail", mock.Anything, mock.Anything).Return(alice(), nil)
		h.hasher.On("Verify", mock.Anything, mock.Anything, mock.Anything).Return(true, nil)
		h.sessions.On("Create", mock.Anything, mock.Anything).Return(errors.New("disk full"))

		_, err := h.svc.Login(t.Context(), "alice@example.com", "pw")

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrInvalidCredentials)
	})

	t.Run("a failed cleanup of old sessions does not fail the login", func(t *testing.T) {
		h := newHarness(t)
		h.users.On("ByEmail", mock.Anything, mock.Anything).Return(alice(), nil)
		h.hasher.On("Verify", mock.Anything, mock.Anything, mock.Anything).Return(true, nil)
		h.sessions.On("Create", mock.Anything, mock.Anything).Return(nil)
		h.sessions.On("DeleteExpired", mock.Anything, mock.Anything).Return(0, errors.New("lock timeout"))

		_, err := h.svc.Login(t.Context(), "alice@example.com", "pw")

		require.NoError(t, err)
		assert.Contains(t, h.logs.String(), "lock timeout")
	})
}

// A hash waits for its turn as long as the request that asked for it, and not
// longer, so the request's own context is the one the hasher gets.
func TestService_TheHasherGetsTheContextOfTheRequest(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(t.Context(), key{}, "this request")

	t.Run("signing in", func(t *testing.T) {
		tests := []struct {
			name, email string
			setup       func(h *harness)
		}{
			{"an email with an account", "alice@example.com", func(h *harness) {
				h.users.On("ByEmail", ctx, "alice@example.com").Return(alice(), nil)
				h.hasher.On("Verify", ctx, "pw", "alice-hash").Return(false, nil)
			}},
			{"an email with none, against the decoy", "ghost@example.com", func(h *harness) {
				h.users.On("ByEmail", ctx, "ghost@example.com").Return(domain.User{}, domain.ErrNotFound)
				h.hasher.On("Verify", ctx, "pw", dummyHash).Return(false, nil)
			}},
			{"a malformed email, against the decoy", "not an email", func(h *harness) {
				h.hasher.On("Verify", ctx, "pw", dummyHash).Return(false, nil)
			}},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				h := newHarness(t)
				tc.setup(h)

				_, err := h.svc.Login(ctx, tc.email, "pw")

				assert.ErrorIs(t, err, domain.ErrInvalidCredentials)
			})
		}
	})
	t.Run("creating the first user", func(t *testing.T) {
		h := newHarness(t)
		h.users.On("Count", ctx).Return(0, nil)
		h.hasher.On("Hash", ctx, "a-long-enough-password").Return("admin-hash", nil)
		h.users.On("Create", ctx, mock.Anything).Return(domain.User{}, nil)

		created, err := h.svc.BootstrapAdmin(ctx, "admin@example.com", "a-long-enough-password")

		require.NoError(t, err)
		assert.True(t, created)
	})
}

func TestAuthenticate(t *testing.T) {
	const token = "a-valid-looking-token-with-enough-length-0123456789"
	user := alice()
	sessionFor := func(expires time.Time) domain.Session {
		return domain.Session{ID: digest(token), UserID: user.ID, ExpiresAt: expires}
	}

	t.Run("a live session returns its user", func(t *testing.T) {
		h := newHarness(t)
		h.sessions.On("Get", mock.Anything, digest(token)).Return(sessionFor(now.Add(time.Hour)), nil)
		h.users.On("ByID", mock.Anything, user.ID).Return(user, nil)

		got, err := h.svc.Authenticate(t.Context(), token)

		require.NoError(t, err)
		assert.Equal(t, user, got)
	})

	t.Run("an expired session is removed and refused", func(t *testing.T) {
		for _, expires := range []time.Time{now.Add(-time.Second), now} {
			h := newHarness(t)
			h.sessions.On("Get", mock.Anything, digest(token)).Return(sessionFor(expires), nil)
			h.sessions.On("Delete", mock.Anything, digest(token)).Return(nil)

			_, err := h.svc.Authenticate(t.Context(), token)

			assert.ErrorIs(t, err, domain.ErrInvalidCredentials, "expiry is exclusive: %v", expires)
		}
	})

	t.Run("an unknown token is refused", func(t *testing.T) {
		h := newHarness(t)
		h.sessions.On("Get", mock.Anything, digest(token)).Return(domain.Session{}, domain.ErrNotFound)

		_, err := h.svc.Authenticate(t.Context(), token)

		assert.ErrorIs(t, err, domain.ErrInvalidCredentials)
	})

	t.Run("a deleted user's session is removed and refused", func(t *testing.T) {
		h := newHarness(t)
		h.sessions.On("Get", mock.Anything, digest(token)).Return(sessionFor(now.Add(time.Hour)), nil)
		h.users.On("ByID", mock.Anything, user.ID).Return(domain.User{}, domain.ErrNotFound)
		h.sessions.On("Delete", mock.Anything, digest(token)).Return(nil)

		_, err := h.svc.Authenticate(t.Context(), token)

		assert.ErrorIs(t, err, domain.ErrInvalidCredentials)
	})

	for name, bad := range map[string]string{"empty": "", "oversized": strings.Repeat("x", 500), "whitespace": "   "} {
		t.Run(name+" token never reaches the store", func(t *testing.T) {
			h := newHarness(t)

			_, err := h.svc.Authenticate(t.Context(), bad)

			assert.ErrorIs(t, err, domain.ErrInvalidCredentials)
		})
	}

	t.Run("store failures are errors, not refusals", func(t *testing.T) {
		h := newHarness(t)
		h.sessions.On("Get", mock.Anything, digest(token)).Return(domain.Session{}, errors.New("db down"))

		_, err := h.svc.Authenticate(t.Context(), token)

		require.Error(t, err)
		assert.NotErrorIs(t, err, domain.ErrInvalidCredentials)
	})
}

func TestLogout(t *testing.T) {
	t.Run("deletes the session by the token's digest", func(t *testing.T) {
		h := newHarness(t)
		h.sessions.On("Delete", mock.Anything, digest("tok")).Return(nil)

		require.NoError(t, h.svc.Logout(t.Context(), "tok"))
	})
	t.Run("an empty token is a no-op", func(t *testing.T) {
		h := newHarness(t)

		require.NoError(t, h.svc.Logout(t.Context(), ""))
	})
	t.Run("deletion errors are returned", func(t *testing.T) {
		h := newHarness(t)
		h.sessions.On("Delete", mock.Anything, mock.Anything).Return(errors.New("db down"))

		require.Error(t, h.svc.Logout(t.Context(), "tok"))
	})
}

func TestBootstrapAdmin(t *testing.T) {
	const password = "a-long-enough-password"

	t.Run("creates the first user", func(t *testing.T) {
		h := newHarness(t)
		h.users.On("Count", mock.Anything).Return(0, nil)
		h.hasher.On("Hash", mock.Anything, password).Return("admin-hash", nil)
		h.users.On("Create", mock.Anything, mock.MatchedBy(func(u domain.User) bool {
			return u.Email == "admin@example.com" && u.PasswordHash == "admin-hash" && u.ID != uuid.Nil()
		})).Return(domain.User{}, nil)

		created, err := h.svc.BootstrapAdmin(t.Context(), "  Admin@Example.com ", password)

		require.NoError(t, err)
		assert.True(t, created)
	})

	t.Run("does nothing when users already exist", func(t *testing.T) {
		h := newHarness(t)
		h.users.On("Count", mock.Anything).Return(2, nil)

		created, err := h.svc.BootstrapAdmin(t.Context(), "admin@example.com", password)

		require.NoError(t, err)
		assert.False(t, created)
	})

	t.Run("losing a race to another instance is fine", func(t *testing.T) {
		h := newHarness(t)
		h.users.On("Count", mock.Anything).Return(0, nil)
		h.hasher.On("Hash", mock.Anything, password).Return("admin-hash", nil)
		h.users.On("Create", mock.Anything, mock.Anything).Return(domain.User{}, domain.ErrEmailTaken)

		created, err := h.svc.BootstrapAdmin(t.Context(), "admin@example.com", password)

		require.NoError(t, err)
		assert.False(t, created)
	})

	t.Run("validates before touching anything", func(t *testing.T) {
		tests := []struct {
			name, email, password string
		}{
			{"bad email", "nope", password},
			{"short password", "admin@example.com", "short"},
			{"empty password", "admin@example.com", ""},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				h := newHarness(t)

				_, err := h.svc.BootstrapAdmin(t.Context(), tc.email, tc.password)

				var ve *domain.ValidationError
				require.ErrorAs(t, err, &ve)
			})
		}
	})

	t.Run("store and hasher failures are errors", func(t *testing.T) {
		h := newHarness(t)
		h.users.On("Count", mock.Anything).Return(0, errors.New("db down"))
		_, err := h.svc.BootstrapAdmin(t.Context(), "admin@example.com", password)
		require.Error(t, err)

		h = newHarness(t)
		h.users.On("Count", mock.Anything).Return(0, nil)
		h.hasher.On("Hash", mock.Anything, password).Return("", errors.New("no entropy"))
		_, err = h.svc.BootstrapAdmin(t.Context(), "admin@example.com", password)
		require.Error(t, err)
	})
}
