package auth_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/auth"
)

// verify asks the hasher and takes an error as a failure of the test: these tests
// are about the answer, and nothing in them keeps a hash waiting.
func verify(t *testing.T, h *auth.Argon2, password, hash string) bool {
	t.Helper()
	ok, err := h.Verify(t.Context(), password, hash)
	require.NoError(t, err)
	return ok
}

func TestArgon2_HashAndVerify(t *testing.T) {
	h := auth.NewArgon2()

	tests := []struct {
		name     string
		password string
	}{
		{"ordinary", "correct horse battery staple"},
		{"unicode", "pässwörd-日本語-🔐"},
		{"very long", strings.Repeat("x", 1024)},
		{"with spaces and quotes", ` a "b" 'c' `},
		{"empty", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hash, err := h.Hash(t.Context(), tc.password)
			require.NoError(t, err)

			assert.True(t, verify(t, h, tc.password, hash))
			assert.False(t, verify(t, h, tc.password+"x", hash), "a longer password")
			assert.False(t, verify(t, h, strings.ToUpper(tc.password)+"!", hash), "a different password")
		})
	}
}

func TestArgon2_Format(t *testing.T) {
	h := auth.NewArgon2()

	hash, err := h.Hash(t.Context(), "secret")

	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`), hash,
		"PHC string format: 16-byte salt and 32-byte key, unpadded base64")

	again, err := h.Hash(t.Context(), "secret")
	require.NoError(t, err)
	assert.NotEqual(t, hash, again, "every hash gets its own salt")
	assert.True(t, verify(t, h, "secret", again))
}

func TestArgon2_MalformedHashesNeverMatch(t *testing.T) {
	h := auth.NewArgon2()
	good, err := h.Hash(t.Context(), "secret")
	require.NoError(t, err)
	parts := strings.Split(good, "$") // "", argon2id, v=19, params, salt, key

	join := func(p ...string) string { return strings.Join(p, "$") }
	tests := []struct {
		name string
		hash string
	}{
		{"empty", ""},
		{"plain text", "secret"},
		{"a bcrypt hash", "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"},
		{"argon2i instead of argon2id", join("", "argon2i", parts[2], parts[3], parts[4], parts[5])},
		{"an unsupported version", join("", "argon2id", "v=18", parts[3], parts[4], parts[5])},
		{"missing parts", join("", "argon2id", parts[2], parts[3], parts[4])},
		{"extra parts", good + "$extra"},
		{"params not numbers", join("", "argon2id", parts[2], "m=a,t=b,p=c", parts[4], parts[5])},
		{"params missing", join("", "argon2id", parts[2], "m=19456", parts[4], parts[5])},
		{"zero iterations", join("", "argon2id", parts[2], "m=19456,t=0,p=1", parts[4], parts[5])},
		{"zero parallelism", join("", "argon2id", parts[2], "m=19456,t=2,p=0", parts[4], parts[5])},
		{"memory beyond the sanity cap", join("", "argon2id", parts[2], "m=4194304,t=2,p=1", parts[4], parts[5])},
		{"iterations beyond the sanity cap", join("", "argon2id", parts[2], "m=19456,t=1000,p=1", parts[4], parts[5])},
		{"salt not base64", join("", "argon2id", parts[2], parts[3], "!!!", parts[5])},
		{"key not base64", join("", "argon2id", parts[2], parts[3], parts[4], "!!!")},
		{"empty salt", join("", "argon2id", parts[2], parts[3], "", parts[5])},
		{"empty key", join("", "argon2id", parts[2], parts[3], parts[4], "")},
		{"only dollars", "$$$$$"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.NotPanics(t, func() {
				assert.False(t, verify(t, h, "secret", tc.hash))
			})
		})
	}
}

func TestArgon2_VerifiesHashesMadeWithOtherParameters(t *testing.T) {
	// A hash carries its own parameters, so raising the defaults later does not
	// lock anyone out.
	weak := auth.NewArgon2(auth.WithParams(8*1024, 1, 1))
	current := auth.NewArgon2()
	hash, err := weak.Hash(t.Context(), "secret")
	require.NoError(t, err)

	assert.True(t, verify(t, current, "secret", hash))
	assert.False(t, verify(t, current, "other", hash))
}
