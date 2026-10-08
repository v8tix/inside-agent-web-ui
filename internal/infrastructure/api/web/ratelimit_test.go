package web

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRateLimiter(t *testing.T) {
	// takes counts how many of n requests in a row are allowed.
	takes := func(l *rateLimiter, key string, n int) (allowed int) {
		for range n {
			if ok, _ := l.allow(key); ok {
				allowed++
			}
		}
		return allowed
	}

	t.Run("a new key starts with a full bucket, and then must wait", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newRateLimiter(2, 3)

			assert.Equal(t, 3, takes(l, "u", 3), "the burst")
			ok, wait := l.allow("u")

			assert.False(t, ok)
			assert.Equal(t, 500*time.Millisecond, wait, "one token at 2 a second")
		})
	})

	t.Run("tokens come back at the set rate", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newRateLimiter(2, 3)
			takes(l, "u", 3)

			time.Sleep(499 * time.Millisecond)
			ok, wait := l.allow("u")
			assert.False(t, ok, "not quite yet")
			assert.Equal(t, time.Millisecond, wait, "and it says how much is left")

			time.Sleep(time.Millisecond)
			ok, _ = l.allow("u")
			assert.True(t, ok, "half a second after the last one")
			ok, _ = l.allow("u")
			assert.False(t, ok, "and only one")
		})
	})

	t.Run("a long rest never banks more than the burst", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newRateLimiter(2, 3)
			takes(l, "u", 3)

			time.Sleep(time.Hour)

			assert.Equal(t, 3, takes(l, "u", 10))
		})
	})

	t.Run("keys do not share a bucket", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newRateLimiter(1, 2)

			assert.Equal(t, 2, takes(l, "alice", 5))
			assert.Equal(t, 2, takes(l, "bob", 5), "bob is untouched by alice")
			assert.Equal(t, 0, takes(l, "alice", 1))
		})
	})

	t.Run("buckets nobody uses are forgotten, busy ones are not", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newRateLimiter(1, 5) // a full refill takes 5 seconds
			for _, key := range []string{"a", "b", "c", "d"} {
				l.allow(key)
			}
			require.Len(t, l.buckets, 4)

			time.Sleep(sweepEvery)
			l.allow("b") // b is used again; the others have been idle for a minute

			assert.Len(t, l.buckets, 1, "the sweep ran, and kept the one in use")
			assert.Contains(t, l.buckets, "b")
		})
	})

	t.Run("forgetting a bucket changes nothing for its key", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newRateLimiter(1, 5)
			takes(l, "u", 5)
			time.Sleep(sweepEvery)
			l.allow("other") // triggers the sweep

			assert.Equal(t, 5, takes(l, "u", 10), "full again, as it would have been anyway")
		})
	})

	t.Run("a crowd at one instant gets exactly the burst", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			l := newRateLimiter(1, 7)
			var allowed atomic.Int32
			var wg sync.WaitGroup
			for range 200 {
				wg.Go(func() {
					if ok, _ := l.allow("shared"); ok {
						allowed.Add(1)
					}
				})
			}
			wg.Wait()

			assert.EqualValues(t, 7, allowed.Load())
		})
	})

	t.Run("without positive numbers there is no limiter", func(t *testing.T) {
		assert.Nil(t, newRateLimiter(0, 10))
		assert.Nil(t, newRateLimiter(2, 0))
		assert.Nil(t, newRateLimiter(-1, -1))
	})
}

func TestRetrySeconds(t *testing.T) {
	tests := []struct {
		wait time.Duration
		want int
	}{
		{0, 1},
		{time.Millisecond, 1},
		{999 * time.Millisecond, 1},
		{time.Second, 1},
		{1200 * time.Millisecond, 2},
		{59500 * time.Millisecond, 60},
	}
	for _, tc := range tests {
		t.Run(tc.wait.String(), func(t *testing.T) {
			assert.Equal(t, tc.want, retrySeconds(tc.wait), "never zero, and never short")
		})
	}
}

// Whoever holds an IPv6 network holds a /64 of addresses, 2^64 of them, and a
// bucket for each would be no limit at all. The network is the client.
func TestByClient(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{"an IPv4 address and its port", "203.0.113.7:51234", "203.0.113.7"},
		{"the same IPv4 address from another port", "203.0.113.7:2", "203.0.113.7"},
		{"an IPv6 address is its /64", "[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:51234", "2001:db8:1:2::"},
		{"another address in that /64 is the same client", "[2001:db8:1:2:1111:2222:3333:4444]:80", "2001:db8:1:2::"},
		{"a different /64 is a different client", "[2001:db8:1:3::1]:80", "2001:db8:1:3::"},
		{"an IPv4 address in IPv6 clothes is the IPv4 address", "[::ffff:203.0.113.7]:80", "203.0.113.7"},
		{"a zone is left out", "[fe80::1%en0]:80", "fe80::"},
		{"loopback", "[::1]:80", "::"},
		{"something that is not an address stays as it is", "pipe", "pipe"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/login", nil)
			r.RemoteAddr = tc.remoteAddr

			assert.Equal(t, tc.want, byClient(r))
		})
	}
}
