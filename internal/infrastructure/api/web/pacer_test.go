package web

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The pacer is judged on a clock that only moves when the test says.
func TestPacer(t *testing.T) {
	const base = 50 * time.Millisecond

	t.Run("the first text goes at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newPacer(base)
			defer p.timer.Stop()

			assert.True(t, p.update("one"))
		})
	})
	t.Run("text that comes too soon waits for the rest of the interval, with the newest words", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newPacer(base)
			defer p.timer.Stop()
			require.True(t, p.update("one"))
			p.sent(time.Millisecond)

			time.Sleep(10 * time.Millisecond)
			assert.False(t, p.update("one two"), "10 ms after the last: too soon")
			assert.False(t, p.update("one two three"), "and again; the timer is already set")
			start := time.Now()
			<-p.timer.C

			assert.Equal(t, 40*time.Millisecond, time.Since(start), "the rest of the 50 ms, once")
			assert.True(t, p.due())
			assert.Equal(t, "one two three", p.text, "what goes out is the newest")
		})
	})
	t.Run("text that comes after the interval goes at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newPacer(base)
			defer p.timer.Stop()
			require.True(t, p.update("one"))
			p.sent(time.Millisecond)

			time.Sleep(base)

			assert.True(t, p.update("two"))
		})
	})
	t.Run("a timer that fires with nothing to send sends nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newPacer(base)
			defer p.timer.Stop()
			require.True(t, p.update("one"))
			p.sent(time.Millisecond)
			time.Sleep(time.Millisecond)
			require.False(t, p.update("two"))
			p.sent(time.Millisecond) // sent some other way before the timer fired

			assert.False(t, p.due())
		})
	})
	t.Run("an update that is slow to make slows the next ones", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newPacer(base)
			defer p.timer.Stop()
			require.True(t, p.update("one"))
			p.sent(100 * time.Millisecond) // four times that is 400 ms

			time.Sleep(300 * time.Millisecond)
			assert.False(t, p.update("two"), "300 ms after the last is still too soon")
			time.Sleep(100 * time.Millisecond)
			assert.True(t, p.update("three"), "400 ms is not")
		})
	})
	t.Run("a flush interval of zero means the default", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := newPacer(0)
			defer p.timer.Stop()

			assert.Equal(t, defaultFlushInterval, p.base)
		})
	})
}
