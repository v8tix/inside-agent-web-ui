package web

import (
	"fmt"
	"maps"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
	"uuid"
)

// sweepEvery is the shortest time between two sweeps: buckets nobody uses are
// forgotten at most this often, when a limited request arrives (see allow).
// Nothing sweeps on a timer, so while no such request comes they stay.
const sweepEvery = time.Minute

// rateLimiter is a set of token buckets, one per key: a user, or a client
// address. A bucket holds up to burst tokens and gains rate tokens a second; a
// request takes one. Memory is the right place for it: the limits are about
// this process's load.
type rateLimiter struct {
	rate  float64 // tokens a second
	burst float64

	mu        sync.Mutex
	buckets   map[string]*bucket
	nextSweep time.Time
}

type bucket struct {
	tokens float64
	at     time.Time // when tokens was last worked out
}

// newRateLimiter returns nil, which means "no limit", unless both numbers are
// positive.
func newRateLimiter(rps float64, burst int) *rateLimiter {
	if rps <= 0 || burst < 1 {
		return nil
	}
	return &rateLimiter{rate: rps, burst: float64(burst), buckets: map[string]*bucket{}}
}

// allow takes a token from key's bucket. With none left it says how long until
// there is one.
//
// Pattern: Lazy-Refill Bucket
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.rate)
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// sweep forgets buckets nobody has used for as long as a full refill takes.
// They are full again, so forgetting them changes nothing.
func (l *rateLimiter) sweep(now time.Time) {
	if now.Before(l.nextSweep) {
		return
	}
	l.nextSweep = now.Add(sweepEvery)
	idle := time.Duration(l.burst / l.rate * float64(time.Second))
	maps.DeleteFunc(l.buckets, func(_ string, b *bucket) bool { return now.Sub(b.at) >= idle })
}

// retrySeconds is the wait as whole seconds for a Retry-After header: never
// zero, so a client does not retry at once.
func retrySeconds(wait time.Duration) int {
	return max(1, int(math.Ceil(wait.Seconds())))
}

// keyFunc says whose bucket a request draws from.
type keyFunc func(*http.Request) string

// byUser limits per signed-in user.
func byUser(r *http.Request) string {
	user, _ := UserFrom(r.Context())
	return user.ID.String()
}

// byClient limits per client. An IPv4 address is a client; so is a whole IPv6
// /64, because that is what one network is given and its owner can use any
// address in it: a bucket per address would be no limit at all. Behind a reverse
// proxy every visitor shares the proxy's address, so the proxy should limit
// sign-ins itself.
func byClient(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().Addr().String() // a prefix has no zone
	}
	return addr.String()
}

// limited wraps h: each request takes a token from the bucket of its key, and
// one over the limit gets 429 with Retry-After from refuse. A nil limiter lets
// everything through.
func limited(l *rateLimiter, key keyFunc, refuse func(w http.ResponseWriter, r *http.Request, wait time.Duration)) func(http.HandlerFunc) http.HandlerFunc {
	return func(h http.HandlerFunc) http.HandlerFunc {
		if l == nil {
			return h
		}
		return func(w http.ResponseWriter, r *http.Request) {
			if ok, wait := l.allow(key(r)); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(wait)))
				refuse(w, r, wait)
				return
			}
			h(w, r)
		}
	}
}

// tooManySends answers a message, a new chat or a regenerate that is over the
// limit. The text the visitor typed is kept.
func (a *App) tooManySends(w http.ResponseWriter, r *http.Request, wait time.Duration) {
	message := fmt.Sprintf("You're sending messages too quickly. Try again in %s.", seconds(retrySeconds(wait)))
	convID, ok := parseID(r)
	if !ok {
		convID = uuid.Nil() // a new chat has no conversation yet
	}
	a.composerProblem(w, r, convID, http.StatusTooManyRequests, r.PostFormValue("content"), message)
}

// tooManyLogins answers a sign-in that is over the limit.
func (a *App) tooManyLogins(w http.ResponseWriter, r *http.Request, wait time.Duration) {
	a.renderLogin(w, r, http.StatusTooManyRequests, loginPage{
		Email: r.PostFormValue("email"),
		Next:  safeNext(r.PostFormValue("next")),
		Error: fmt.Sprintf("Too many sign-in attempts. Try again in %s.", seconds(retrySeconds(wait))),
	})
}

func seconds(n int) string {
	if n == 1 {
		return "1 second"
	}
	return strconv.Itoa(n) + " seconds"
}
