// Package lazybucket limits a rate with a token bucket that needs no timer.
package lazybucket

import (
	"sync"
	"time"
)

// Bucket holds up to Burst tokens and gains Rate tokens a second.
type Bucket struct {
	Rate, Burst float64
	mu          sync.Mutex
	tokens      float64
	at          time.Time
}

// Allow takes one token at the instant now. With none left it says how long
// until there is one. The tokens that came back since the last call are worked
// out here, from the time that has passed, so nothing runs between two calls.
func (b *Bucket) Allow(now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.at.IsZero() {
		b.tokens, b.at = b.Burst, now
	}
	b.tokens = min(b.Burst, b.tokens+now.Sub(b.at).Seconds()*b.Rate)
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / b.Rate * float64(time.Second))
}
