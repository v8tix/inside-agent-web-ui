package auth

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

// Each hash holds 19 MiB while it runs, and signing in is open to anyone. A flood
// of attempts from as many addresses as the sender has would hold that many times
// 19 MiB at once, and the process would be killed for it. Only as many hashes as
// there are processors run at a time; the rest wait their turn.
func TestArgon2_OnlyAsManyHashesRunAtOnceAsThereIsRoomFor(t *testing.T) {
	var running, peak atomic.Int64
	slow := func(password, salt []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		now := running.Add(1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		running.Add(-1)
		return make([]byte, keyLen)
	}
	h := NewArgon2(WithConcurrency(3))
	h.idKey = slow
	hash, err := h.Hash(t.Context(), "a password")
	require.NoError(t, err)
	peak.Store(0)

	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			_, _ = h.Verify(t.Context(), "another one", hash)
			_, _ = h.Hash(t.Context(), "and another")
		})
	}
	wg.Wait()

	assert.EqualValues(t, 3, peak.Load(), "three at a time, and the other twenty-one waited")
}

func TestArgon2_TheDefaultIsAsManyAsThereAreProcessors(t *testing.T) {
	assert.Equal(t, runtime.GOMAXPROCS(0), cap(NewArgon2().slots))
	assert.Equal(t, 1, cap(NewArgon2(WithConcurrency(0)).slots), "never fewer than one, or nothing could be checked")
	assert.Equal(t, 7, cap(NewArgon2(WithConcurrency(7)).slots))
}

func TestArgon2_AWaitingHashDoesNotHoldAnotherUp(t *testing.T) {
	h := NewArgon2(WithConcurrency(1))
	released := make(chan struct{})
	h.idKey = func(_, _ []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		<-released
		return make([]byte, keyLen)
	}
	done := make(chan struct{})
	go func() {
		_, _ = h.Hash(t.Context(), "x")
		close(done)
	}()
	close(released) // no hash waits for anything now

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a slot was not given back")
	}
	for range 3 { // and a slot that was given back can be taken again
		_, err := h.Hash(t.Context(), "y")
		require.NoError(t, err)
	}
}

// holdTheOnlySlot makes a hasher with one slot and starts a hash that keeps it
// until the release function is called, which the test's cleanup does too, so a
// failing assertion does not leave that hash blocked. valid is a hash it made
// before, for Verify to ask about.
func holdTheOnlySlot(t *testing.T, opts ...Option) (h *Argon2, valid string, release func()) {
	t.Helper()
	h = NewArgon2(append([]Option{WithConcurrency(1)}, opts...)...)
	var hold atomic.Bool
	free := make(chan struct{})
	h.idKey = func(_, _ []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		if hold.Load() {
			<-free
		}
		return make([]byte, keyLen)
	}
	valid, err := h.Hash(t.Context(), "a password")
	require.NoError(t, err)
	hold.Store(true)
	go func() { _, _ = h.Hash(t.Context(), "keeps the slot") }()
	synctest.Wait() // it holds the slot now
	release = sync.OnceFunc(func() { close(free); synctest.Wait() })
	t.Cleanup(release)
	return h, valid, release
}

// The wait for a slot is not for ever. Without a limit a flood of sign-ins
// would queue as many waiting hashes as it has connections, and serve them long
// after the visitors who sent them had left.
func TestArgon2_AHashThatWaitsTooLongGivesUp(t *testing.T) {
	asks := map[string]func(h *Argon2, ctx context.Context, valid string) error{
		"Hash": func(h *Argon2, ctx context.Context, _ string) error {
			_, err := h.Hash(ctx, "waits")
			return err
		},
		"Verify": func(h *Argon2, ctx context.Context, valid string) error {
			_, err := h.Verify(ctx, "waits", valid)
			return err
		},
	}
	for name, ask := range asks {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, valid, release := holdTheOnlySlot(t, WithMaxWait(2*time.Second))
				start := time.Now()

				err := ask(h, t.Context(), valid)

				require.ErrorIs(t, err, domain.ErrBusy)
				assert.Equal(t, 2*time.Second, time.Since(start), "it waited as long as it was told to, and no longer")

				release()
				_, err = h.Hash(t.Context(), "and the slot is still there to be taken")
				require.NoError(t, err)
			})
		})
	}
}

func TestArgon2_AWaitEndsWhenTheRequestDoes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, valid, _ := holdTheOnlySlot(t) // the default wait is longer than the visitor stays
		ctx, leave := context.WithCancel(t.Context())
		time.AfterFunc(time.Second, leave)
		start := time.Now()

		ok, err := h.Verify(ctx, "waits", valid)

		require.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, domain.ErrBusy, "the visitor left; the server was not too busy")
		assert.False(t, ok)
		assert.Equal(t, time.Second, time.Since(start))
	})
}

func TestArgon2_ARequestThatIsGoneIsNotHashed(t *testing.T) {
	var runs atomic.Int64
	h := NewArgon2()
	h.idKey = func(_, _ []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		runs.Add(1)
		return make([]byte, keyLen)
	}
	gone, leave := context.WithCancel(t.Context())
	leave()

	for range 50 { // a free slot and a finished context are both ready, and a select picks either
		_, err := h.Hash(gone, "x")
		require.ErrorIs(t, err, context.Canceled)
	}

	assert.Zero(t, runs.Load(), "not one of them was run")
}

func TestArgon2_AFreeSlotIsTakenWhateverTheWait(t *testing.T) {
	// A wait is for a slot that is not free. A hasher that nobody else uses never
	// turns anyone away, however short the wait is.
	h := NewArgon2(WithMaxWait(time.Nanosecond))
	h.idKey = func(_, _ []byte, _, _ uint32, _ uint8, keyLen uint32) []byte { return make([]byte, keyLen) }

	for range 500 {
		_, err := h.Hash(t.Context(), "x")
		require.NoError(t, err)
	}
}

func TestArgon2_TheDefaultWaitIsFiveSeconds(t *testing.T) {
	assert.Equal(t, 5*time.Second, NewArgon2().maxWait)
	assert.Equal(t, 5*time.Second, NewArgon2(WithMaxWait(0)).maxWait, "zero means the default")
	assert.Equal(t, 5*time.Second, NewArgon2(WithMaxWait(-time.Second)).maxWait, "so does less than zero")
	assert.Equal(t, 750*time.Millisecond, NewArgon2(WithMaxWait(750*time.Millisecond)).maxWait)
}
