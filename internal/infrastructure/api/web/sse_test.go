package web

import (
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deadlineRecorder is a response writer that takes a write deadline, as a real
// connection does, and keeps how far ahead each one was set.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	ahead []time.Duration
}

func (d *deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	d.ahead = append(d.ahead, time.Until(deadline))
	return nil
}

// A client that stopped reading must not hold a connection, and the run behind
// it, for longer than this. The server has no write timeout of its own, so every
// write of a stream sets one, counted from that write.
func TestSSEWriter_EveryWriteHasFifteenSeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}

		w, err := newSSEWriter(rec)
		require.NoError(t, err)
		time.Sleep(time.Minute)
		require.NoError(t, w.event("delta", "text"))
		time.Sleep(time.Minute)
		require.NoError(t, w.comment("keep-alive"))
		time.Sleep(time.Minute)
		require.NoError(t, w.retry(time.Second))

		assert.Equal(t, []time.Duration{15 * time.Second, 15 * time.Second, 15 * time.Second, 15 * time.Second}, rec.ahead,
			"the opening comment, an event, a keep-alive and a retry")
	})
}
