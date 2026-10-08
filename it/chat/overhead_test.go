package chat_test

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/it/internal/leak"
	"github.com/v8tix/inside-agent-web-ui/it/internal/stack"
)

// SC-13: what the Web UI adds between the agent's first word and the reader's
// first update stays within 100 ms at the 95th percentile.
//
// The agent says when its first word came (data-ttft-ms, measured by the Web UI
// from the moment it asked the agent), and the client knows when the first update
// reached it. The difference is the work the Web UI did around the agent: the
// database, the session, the rendering, the flush. A browser adds its own time
// on top; the evaluator's U-05 check measures that part with a real one.
func TestUIOverhead(t *testing.T) {
	leak.Check(t)
	const (
		warmup  = 3 // the first requests find caches and connections cold
		samples = 40
		limit   = 100.0 // ms
	)
	s := stack.Start(t,
		stack.WithSetting("RATE_LIMIT_RPS", "1000"), stack.WithSetting("RATE_LIMIT_BURST", "1000"))
	c := s.SignedIn()

	var overheads []float64
	for i := range warmup + samples {
		conv, reply := c.Start(fmt.Sprintf("question number %d", i+1))
		opened := time.Now()
		stream := c.OpenStream(within(t, 20*time.Second), conv, reply)

		var firstWord time.Time
		var final stack.Message
		for ev, ok := stream.Next(); ok; ev, ok = stream.Next() {
			if ev.Name != "delta" {
				continue
			}
			m := stack.MessagesIn(t, ev.Data)[0]
			if firstWord.IsZero() && m.Text != "" {
				firstWord = ev.At
			}
			final = m
		}

		require.False(t, firstWord.IsZero(), "an update with text arrived")
		agentFirstWord, err := strconv.ParseFloat(stack.Attr(final.Node, "data-ttft-ms"), 64)
		require.NoError(t, err, "the Web UI measured the agent's time to first word")
		if i >= warmup {
			overheads = append(overheads, float64(firstWord.Sub(opened).Microseconds())/1000-agentFirstWord)
		}
	}

	slices.Sort(overheads)
	p95 := overheads[int(math.Ceil(0.95*float64(len(overheads))))-1] // nearest rank
	t.Logf("%d replies: Web UI overhead median %.1f ms, 95th percentile %.1f ms, worst %.1f ms (limit %.0f ms)",
		len(overheads), overheads[len(overheads)/2], p95, overheads[len(overheads)-1], limit)
	assert.LessOrEqual(t, p95, limit)
}
