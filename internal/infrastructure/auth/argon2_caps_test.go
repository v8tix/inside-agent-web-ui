package auth

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A stored hash names its own cost, and Verify pays it. A hash that asks for more
// than a cap, or for nothing at all, is refused before any work is done. Here the
// key function answers with zeros and the stored key is zeros, so a hash that is
// run matches: only the refusal keeps the answer at "no match".
func TestArgon2_ParametersOverTheCapsAreNeverRun(t *testing.T) {
	hashWith := func(params string) string {
		salt, zeros := b64.EncodeToString(make([]byte, saltBytes)), b64.EncodeToString(make([]byte, keyBytes))
		return "$argon2id$v=19$" + params + "$" + salt + "$" + zeros
	}
	// counting is a hasher that notes the cost of every hash it is made to run.
	counting := func() (h *Argon2, ran *[]string) {
		ran = new([]string)
		h = NewArgon2()
		h.idKey = func(_, _ []byte, iterations, memory uint32, threads uint8, keyLen uint32) []byte {
			*ran = append(*ran, fmt.Sprintf("m=%d,t=%d,p=%d", memory, iterations, threads))
			return make([]byte, keyLen)
		}
		return h, ran
	}

	refused := []struct {
		name   string
		params string
	}{
		{"no memory", "m=0,t=2,p=1"},
		{"one KiB over the memory cap", "m=262145,t=2,p=1"},
		{"no iterations", "m=19456,t=0,p=1"},
		{"one iteration over the cap", "m=19456,t=11,p=1"},
		{"no threads", "m=19456,t=2,p=0"},
		{"one thread over the cap", "m=19456,t=2,p=17"},
	}
	for _, tc := range refused {
		t.Run(tc.name+" is refused", func(t *testing.T) {
			h, ran := counting()

			ok, err := h.Verify(t.Context(), "a password", hashWith(tc.params))

			require.NoError(t, err)
			assert.False(t, ok)
			assert.Empty(t, *ran, "the key function was not called")
		})
	}

	t.Run("parameters at the caps are run", func(t *testing.T) {
		h, ran := counting()

		ok, err := h.Verify(t.Context(), "a password", hashWith("m=262144,t=10,p=16"))

		require.NoError(t, err)
		assert.True(t, ok, "zeros match zeros: a hash that is run matches")
		assert.Equal(t, []string{"m=262144,t=10,p=16"}, *ran)
	})
}
