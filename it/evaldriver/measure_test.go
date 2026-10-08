package evaldriver_test

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/report"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
	"github.com/v8tix/inside-agent-web-ui/it/internal/evalenv"
)

// TestMeasureUIOverhead is not a check: it measures where the time between the
// Enter key and the first text goes, on this machine, and prints it. It runs
// only when EVAL_MEASURE in .env is the number of attempts.
func TestMeasureUIOverhead(t *testing.T) {
	lookup, err := envcfg.EnvLookup("../../.env")
	require.NoError(t, err)
	measure, _ := lookup("EVAL_MEASURE")
	n, _ := strconv.Atoi(measure)
	if n == 0 {
		t.Skip("set EVAL_MEASURE=<attempts> in .env to measure")
	}
	evalenv.RequireBrowser(t)
	env := evalenv.Start(t, testagent.WithWordDelay(15*time.Millisecond))
	d := openDriver(t, env)
	ctx := t.Context()
	require.NoError(t, d.Login(ctx, env.Email, env.Password))

	var pending, streaming, firstText, ttftAgent, overhead []float64
	for i := range n {
		require.NoError(t, d.StartChat(ctx))
		require.NoError(t, d.Submit(ctx, fmt.Sprintf("question number %d", i)))
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := d.AwaitReply(waitCtx)
		cancel()
		require.NoError(t, err)
		obs, err := d.Collect(ctx)
		require.NoError(t, err)

		at := func(s domain.ReplyStatus) float64 {
			for _, c := range obs.Statuses {
				if c.Status == s {
					return c.At - obs.KeydownAt
				}
			}
			return -1
		}
		ui, _ := obs.TTFTUI()
		oh, _ := obs.UIOverhead()
		pending = append(pending, at(domain.ReplyPending))
		streaming = append(streaming, at(domain.ReplyStreaming))
		firstText = append(firstText, ui)
		if obs.Message.TTFTMs != nil {
			ttftAgent = append(ttftAgent, float64(*obs.Message.TTFTMs))
		}
		overhead = append(overhead, oh)
		t.Logf("#%02d keydown→pending %6.1f  →streaming %6.1f  →first text %6.1f  agent ttft %4v  overhead %6.1f",
			i, at(domain.ReplyPending), at(domain.ReplyStreaming), ui, ptr(obs.Message.TTFTMs), oh)
		require.NoError(t, d.DeleteChat(ctx))
	}
	row := func(name string, v []float64) {
		q := report.Summarize(v)
		t.Logf("%-22s n=%d  p50 %6.1f  p95 %6.1f  max %6.1f", name, q.N, q.P50, q.P95, q.Max)
	}
	row("keydown→pending", pending)
	row("keydown→streaming", streaming)
	row("keydown→first text", firstText)
	row("agent ttft (data)", ttftAgent)
	row("UI overhead", overhead)
}

func ptr(v *int64) any {
	if v == nil {
		return "-"
	}
	return *v
}
