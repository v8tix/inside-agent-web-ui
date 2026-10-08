package evaldriver_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/uicheck"
	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
	"github.com/v8tix/inside-agent-web-ui/it/internal/evalenv"
)

func result(findings []uicheck.Finding, id domain.CheckID) domain.CheckResult {
	for _, f := range findings {
		if f.ID == id {
			return f.Result
		}
	}
	return ""
}

// The observer is the evaluator's own instrument, so these tests check the
// instrument: that it reads the page's clock, sees what a reader sees, and that
// the checks built on it can fail when the page misbehaves (negative controls).
func TestObserverSuite(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := evalenv.Start(t, testagent.WithWordDelay(15*time.Millisecond), testagent.WithSlow(30*time.Millisecond, 400))
	d := openDriver(t, env)
	ctx := t.Context()
	require.NoError(t, d.Login(ctx, env.Email, env.Password))

	t.Run("times are on the clock of the machine, in milliseconds since the epoch", func(t *testing.T) {
		before := time.Now()
		ask(t, d, "what time is it")
		after := time.Now()

		obs, err := d.Collect(ctx)

		require.NoError(t, err)
		keydown := time.UnixMilli(int64(obs.KeydownAt))
		assert.WithinDuration(t, before, keydown, 5*time.Second, "Enter was pressed between the two readings of the host's clock")
		assert.True(t, keydown.After(before.Add(-time.Second)) && keydown.Before(after.Add(time.Second)))
		for i := 1; i < len(obs.Updates); i++ {
			assert.GreaterOrEqual(t, obs.Updates[i].At, obs.Updates[i-1].At, "updates are in order")
			assert.GreaterOrEqual(t, obs.Updates[i].Chars, obs.Updates[i-1].Chars, "and a reply only grows")
		}
		var order []domain.ReplyStatus
		for _, s := range obs.Statuses {
			order = append(order, s.Status)
		}
		assert.Equal(t, []domain.ReplyStatus{domain.ReplyPending, domain.ReplyStreaming, domain.ReplyComplete}, order)
	})

	t.Run("a typing indicator is not text", func(t *testing.T) {
		ask(t, d, "hello")

		obs, err := d.Collect(ctx)

		require.NoError(t, err)
		require.NotEmpty(t, obs.Updates)
		assert.Greater(t, obs.Updates[0].Chars, 1, "the first update has words, not the single ellipsis of the typing indicator")
		var pendingAt float64
		for _, s := range obs.Statuses {
			if s.Status == domain.ReplyPending {
				pendingAt = s.At
			}
		}
		require.Positive(t, pendingAt, "the pending bubble was seen")
		assert.Greater(t, obs.Updates[0].At, pendingAt, "and the first text came after it")
	})

	t.Run("a long answer streams in many steps and the page follows it", func(t *testing.T) {
		require.NoError(t, d.StartChat(ctx))
		require.NoError(t, d.Submit(ctx, "write a lot #slow"))
		reply := await(t, d, 60*time.Second)
		require.Equal(t, domain.ReplyComplete, reply.Status)

		obs, err := d.Collect(ctx)
		require.NoError(t, err)

		assert.Greater(t, obs.DistinctUpdates(), 100, "400 ticks arrive in far more than 3 steps")
		assert.GreaterOrEqual(t, obs.FinalChars(), 200)
		gap, ok := obs.MaxGap()
		require.True(t, ok)
		assert.Less(t, gap, 2000.0)
		var worst float64
		scrollable := 0
		for _, s := range obs.Scrolls {
			worst = max(worst, s.FromBottomPx)
			if s.Scrollable {
				scrollable++
			}
		}
		t.Logf("scroll samples=%d (%d of a list that could scroll), furthest from the bottom=%.1f px", len(obs.Scrolls), scrollable, worst)
		assert.Positive(t, scrollable, "the reply outgrew the window, so following it means something")
		findings := uicheck.Evaluate(uicheck.Input{Obs: obs, Answer: "tick 400"})
		assert.Equal(t, domain.Pass, result(findings, domain.U04))
		assert.Equal(t, domain.Pass, result(findings, domain.U07), "the page keeps the end of the reply in view")
		assert.Equal(t, domain.Pass, result(findings, domain.U06))
	})

	t.Run("a reader who scrolls away is not followed, and U-07 notices", func(t *testing.T) {
		require.NoError(t, d.StartChat(ctx))
		require.NoError(t, d.Submit(ctx, "write a lot #slow"))
		require.Eventually(t, func() bool {
			var taller bool // the reply is more than a screenful, so there is something to scroll away from
			_ = d.Eval(ctx, "() => { const m = document.querySelector('#messages'); return !!m && m.scrollHeight > m.clientHeight + 150; }", &taller)
			return taller
		}, 30*time.Second, 100*time.Millisecond)

		// scroll to the top, the way a reader would to look at the start
		require.NoError(t, d.Eval(ctx, "() => { document.querySelector('#messages').scrollTop = 0; return true; }", nil))
		reply := await(t, d, 60*time.Second)
		require.Equal(t, domain.ReplyComplete, reply.Status)

		obs, err := d.Collect(ctx)
		require.NoError(t, err)

		var worst float64
		for _, s := range obs.Scrolls {
			worst = max(worst, s.FromBottomPx)
		}
		t.Logf("furthest from the bottom=%.1f px", worst)
		assert.Greater(t, worst, 80.0, "the view stayed where the reader left it")
		assert.Equal(t, domain.Fail, result(uicheck.Evaluate(uicheck.Input{Obs: obs, Answer: "tick 400"}), domain.U07),
			"the check can fail: a view that does not follow is told apart from one that does")
	})

	t.Run("a console error is seen, and U-01 notices", func(t *testing.T) {
		ask(t, d, "console please")
		require.NoError(t, d.Eval(ctx, "() => { console.error('spike: simulated console error'); return true; }", nil))

		obs, err := d.Collect(ctx)

		require.NoError(t, err)
		assert.Equal(t, 1, obs.ConsoleErrors())
		assert.Equal(t, domain.Fail, result(uicheck.Evaluate(uicheck.Input{Obs: obs, Answer: "console please"}), domain.U01))
	})

	t.Run("a request the page makes that fails is seen, and U-02 notices", func(t *testing.T) {
		ask(t, d, "network please")
		require.NoError(t, d.Eval(ctx, "async () => { await fetch('/no-such-page'); return true; }", nil))

		obs, err := d.Collect(ctx)

		require.NoError(t, err)
		assert.GreaterOrEqual(t, obs.FailedRequests(), 1)
		assert.Equal(t, domain.Fail, result(uicheck.Evaluate(uicheck.Input{Obs: obs, Answer: "network please"}), domain.U02))
	})

	t.Run("hostile HTML in a reply is rendered harmless", func(t *testing.T) {
		ask(t, d, "show me #html")

		obs, err := d.Collect(ctx)

		require.NoError(t, err)
		assert.False(t, obs.Unsafe.Any(), "the Web UI sanitizes the reply: %+v", obs.Unsafe)
		assert.Equal(t, domain.Pass, result(uicheck.Evaluate(uicheck.Input{Obs: obs, Answer: "Here is some content."}), domain.U08))
	})

	t.Run("an unsafe reply body would be found, and U-08 notices", func(t *testing.T) {
		ask(t, d, "plain please")
		require.NoError(t, d.Eval(ctx, `() => {
			const body = [...document.querySelectorAll('[data-testid=message-body]')].at(-1);
			body.insertAdjacentHTML('beforeend', '<script>1</script><img src="https://evil.example/p.png" onerror="1">');
			return true;
		}`, nil))

		obs, err := d.Collect(ctx)

		require.NoError(t, err)
		assert.Equal(t, 1, obs.Unsafe.Scripts)
		assert.Equal(t, 1, obs.Unsafe.EventHandlerAttrs)
		assert.Equal(t, 1, obs.Unsafe.ExternalImages)
		assert.Equal(t, domain.Fail, result(uicheck.Evaluate(uicheck.Input{Obs: obs, Answer: "plain please"}), domain.U08))
	})

	t.Run("a page opened without the init script is observed late and says so", func(t *testing.T) {
		// Go to the start page the way a person would: no observer is put in.
		_, err := d.Tool(ctx, "navigate_page", map[string]any{"type": "url", "url": env.URL + "/"})
		require.NoError(t, err)
		require.NoError(t, d.Submit(ctx, "no observer yet"))
		await(t, d, 60*time.Second)

		obs, err := d.Collect(ctx)

		require.NoError(t, err)
		assert.True(t, obs.ObserverLate)
		assert.LessOrEqual(t, len(obs.Updates), 1, "it saw none of the reply grow, only how it ended")
		findings := uicheck.Evaluate(uicheck.Input{Obs: obs, Answer: "no observer yet"})
		assert.Equal(t, domain.NA, result(findings, domain.U04), "so the streaming check is not claimed")
		assert.Equal(t, domain.NA, result(findings, domain.U05), "nor the overhead one")
		assert.Contains(t, obs.VisibleText, "no observer yet", "though the final text is still read")
	})
}
