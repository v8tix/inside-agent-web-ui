// Integration tests of the evaluator's browser driver against the real Web UI:
// Chrome DevTools MCP, headless Chrome, the application, a real PostgreSQL and
// the scripted agent. Group A: needs Docker, Node (npx) and Chrome.
package evaldriver_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/uicheck"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
	"github.com/v8tix/inside-agent-web-ui/it/internal/evalenv"
)

// openDriver starts a browser for the test.
func openDriver(t *testing.T, env *evalenv.Env, mutate ...func(*devtools.DriverConfig)) *devtools.Driver {
	t.Helper()
	cfg := devtools.DriverConfig{BaseURL: env.URL, PollInterval: 100 * time.Millisecond}
	for _, m := range mutate {
		m(&cfg)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	s, err := devtools.Open(ctx, evalenv.BrowserOptions(t))
	require.NoError(t, err)
	d := devtools.NewDriver(s, cfg)
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = d.Close(closeCtx)
	})
	return d
}

// ask puts a question to the agent and waits for the reply to finish.
func ask(t *testing.T, d *devtools.Driver, question string) domain.Reply {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, d.StartChat(ctx))
	require.NoError(t, d.Submit(ctx, question))
	return await(t, d, 60*time.Second)
}

func await(t *testing.T, d *devtools.Driver, within time.Duration) domain.Reply {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), within)
	defer cancel()
	reply, err := d.AwaitReply(ctx)
	require.NoError(t, err)
	return reply
}

func TestDriverSuite(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := evalenv.Start(t, testagent.WithWordDelay(15*time.Millisecond), testagent.WithSlow(100*time.Millisecond, 200))
	d := openDriver(t, env)
	ctx := t.Context()

	t.Run("a wrong password is refused and the right one signs in", func(t *testing.T) {
		other := openDriver(t, env)

		err := other.Login(ctx, env.Email, "not-the-password")

		require.ErrorIs(t, err, domain.ErrLoginFailed)
		require.NoError(t, d.Login(ctx, env.Email, env.Password))
	})

	t.Run("asks a question like a person and reads everything back", func(t *testing.T) {
		reply := ask(t, d, "Tell me about the printing press #markdown")

		assert.Equal(t, domain.ReplyComplete, reply.Status)
		assert.NotEmpty(t, reply.ConversationID)
		assert.NotEmpty(t, reply.MessageID)

		raw, err := d.ReadRaw(ctx, reply)
		require.NoError(t, err)
		assert.Contains(t, raw, "# Report", "the Markdown as the agent wrote it, not as the page shows it")
		assert.Contains(t, raw, "```go", "code fences and all")

		obs, err := d.Collect(ctx)
		require.NoError(t, err)
		for _, r := range obs.Requests {
			t.Logf("request %s %s [%d failed=%v]", r.Method, r.URL, r.Status, r.Failed)
		}
		assert.False(t, obs.ObserverLate, "the observer was in the page before the reply began")
		assert.Positive(t, obs.KeydownAt, "Enter in the composer was seen")
		ttft, ok := obs.TTFTUI()
		require.True(t, ok)
		assert.Positive(t, ttft)
		latency, ok := obs.Latency()
		require.True(t, ok)
		assert.Greater(t, latency, ttft)
		assert.GreaterOrEqual(t, len(obs.Updates), 3, "the reply arrived in pieces")
		assert.Equal(t, domain.ReplyComplete, obs.Message.Status)
		assert.Equal(t, reply.MessageID, obs.Message.ID)
		require.NotNil(t, obs.Message.TTFTMs, "the agent's own time to first token is on the message")
		require.NotNil(t, obs.Message.DurationMs)
		require.NotNil(t, obs.Message.PromptTokens)
		require.NotNil(t, obs.Message.CompletionTokens)
		assert.Contains(t, obs.VisibleText, "Report")
		assert.False(t, obs.Unsafe.Any())
		assert.Empty(t, obs.Console, "nothing in the console")

		// Every passive check, on the real page. U-05 judges one attempt against the
		// limit, and the first request after a browser opens is slower than the rest
		// (see TestMeasureUIOverhead), so here it only has to be a sane measurement.
		findings := uicheck.Evaluate(uicheck.Input{Obs: obs, Answer: "Report"})
		for _, f := range findings {
			t.Logf("%s %s %s", f.ID, f.Result, f.Detail)
		}
		for _, f := range uicheck.Failed(findings) {
			assert.Equal(t, domain.U05, f.ID, "only the overhead check may be over its limit on a cold first request: %s", f.Detail)
		}
		overhead, ok := obs.UIOverhead()
		require.True(t, ok)
		assert.Greater(t, overhead, -50.0)
		assert.Less(t, overhead, 1000.0)
	})

	t.Run("two conversations in one browser do not share what was observed", func(t *testing.T) {
		first := ask(t, d, "first question")
		firstObs, err := d.Collect(ctx)
		require.NoError(t, err)

		second := ask(t, d, "second question")
		secondObs, err := d.Collect(ctx)
		require.NoError(t, err)

		assert.NotEqual(t, first.MessageID, second.MessageID)
		assert.NotEqual(t, first.ConversationID, second.ConversationID)
		assert.Equal(t, second.MessageID, secondObs.Message.ID)
		assert.Greater(t, secondObs.KeydownAt, firstObs.KeydownAt)
		assert.Greater(t, secondObs.Updates[0].At, firstObs.Updates[len(firstObs.Updates)-1].At, "the second record starts where the second question did")
		assert.Contains(t, secondObs.VisibleText, "second question")
		assert.NotContains(t, secondObs.VisibleText, "first question")
	})

	t.Run("a chat is deleted through the sidebar", func(t *testing.T) {
		require.NoError(t, d.DeleteChat(ctx))

		var rows int
		require.NoError(t, d.Eval(ctx, "() => document.querySelectorAll('[data-testid=conversation]').length", &rows))
		t.Logf("conversations left in the sidebar: %d", rows)
	})

	t.Run("a long reply can be stopped and what was written is kept", func(t *testing.T) {
		require.NoError(t, d.StartChat(ctx))
		require.NoError(t, d.Submit(ctx, "#slow"))
		require.Eventually(t, func() bool {
			var status *string
			_ = d.Eval(ctx, "() => { const m = [...document.querySelectorAll('[data-testid=message][data-role=assistant]')].at(-1); return m ? m.dataset.status : null; }", &status)
			return status != nil && *status == "streaming"
		}, 20*time.Second, 100*time.Millisecond)

		require.NoError(t, d.Stop(ctx))
		reply := await(t, d, 10*time.Second)

		assert.Equal(t, domain.ReplyStopped, reply.Status)
		raw, err := d.ReadRaw(ctx, reply)
		require.NoError(t, err)
		assert.Contains(t, raw, "tick 1 ", "the part written before Stop is kept")
		assert.NotContains(t, raw, "tick 200", "and the rest never came")
	})

	t.Run("a screenshot is a PNG", func(t *testing.T) {
		path := t.TempDir() + "/page.png"

		require.NoError(t, d.Screenshot(ctx, path))

		assert.FileExists(t, path)
	})

	t.Run("a Web UI that is not there", func(t *testing.T) {
		lost := openDriver(t, env, func(c *devtools.DriverConfig) { c.BaseURL = "http://127.0.0.1:1" })

		err := lost.Login(ctx, env.Email, env.Password)

		require.Error(t, err)
		assert.ErrorContains(t, err, "opening the Web UI")
	})
}

func TestHumanTyping(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := evalenv.Start(t, testagent.WithWordDelay(5*time.Millisecond))
	d := openDriver(t, env, func(c *devtools.DriverConfig) { c.HumanTyping, c.TypingDelay = true, 15*time.Millisecond })
	ctx := t.Context()
	require.NoError(t, d.Login(ctx, env.Email, env.Password))
	started := time.Now()

	reply := ask(t, d, "what does a person type here")

	assert.Equal(t, domain.ReplyComplete, reply.Status)
	raw, err := d.ReadRaw(ctx, reply)
	require.NoError(t, err)
	assert.Equal(t, "You said: what does a person type here", raw, "the keystrokes made the same text")
	obs, err := d.Collect(ctx)
	require.NoError(t, err)
	assert.Positive(t, obs.KeydownAt, "Enter was seen after the typing")
	assert.Greater(t, time.Since(started), 150*time.Millisecond, "typing takes time")
}

// The composer sends on Enter, and the browser's keyboard types a line break as
// Enter. A prompt of several lines, such as the evaluator's own, would be sent at
// its first line and the rest of it typed into a composer that ignores Enter
// while a reply is coming: the agent would never see the question.
func TestHumanTyping_ALineBreakIsNotASend(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := evalenv.Start(t, testagent.WithWordDelay(2*time.Millisecond))
	d := openDriver(t, env, func(c *devtools.DriverConfig) { c.HumanTyping, c.TypingDelay = true, time.Millisecond })
	ctx := t.Context()
	require.NoError(t, d.Login(ctx, env.Email, env.Password))
	prompt := "Answer briefly.\nEnd with the final answer.\n\nQuestion: what does a person type here?"

	reply := ask(t, d, prompt)

	assert.Equal(t, domain.ReplyComplete, reply.Status)
	raw, err := d.ReadRaw(ctx, reply)
	require.NoError(t, err)
	assert.Equal(t, "You said: "+prompt, raw, "all of it reached the agent, in one message")
	assert.EqualValues(t, 1, env.Model.Probe().Runs(), "and it was one message, not one for the first line")
}
