// The Web UI in front of a real agent. Group B: skipped unless .env says
// AGENT_LIVE=1, and never part of the default run, because it needs an agent that
// is running, may cost money, and answers differently every time.
//
//	go test ./it/live/
//
// The agent is the one in .env (AGENT_BASE_URL, AGENT_APP_NAME); a variable in the
// environment wins. Docker is needed for PostgreSQL.
package live_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
	"github.com/v8tix/inside-agent-web-ui/it/internal/stack"
)

func TestLiveSuite(t *testing.T) {
	lookup, err := envcfg.EnvLookup("../../.env")
	require.NoError(t, err)
	if live, _ := lookup("AGENT_LIVE"); live != "1" {
		t.Skip("set AGENT_LIVE=1 in .env to talk to the real agent named there (AGENT_BASE_URL, AGENT_APP_NAME)")
	}
	agentURL, _ := lookup("AGENT_BASE_URL")
	appName, _ := lookup("AGENT_APP_NAME")
	require.NotEmpty(t, agentURL, "AGENT_BASE_URL is not set")
	require.NotEmpty(t, appName, "AGENT_APP_NAME is not set")
	settings := []stack.Option{stack.WithExternalAgent(agentURL, appName)}
	if timeout, ok := lookup("AGENT_REQUEST_TIMEOUT"); ok {
		settings = append(settings, stack.WithSetting("AGENT_REQUEST_TIMEOUT", timeout))
	}
	t.Logf("agent: %s, application %q", agentURL, appName)
	s := stack.Start(t, settings...)
	c := s.SignedIn()
	patient := func() context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		t.Cleanup(cancel)
		return ctx
	}

	t.Run("the real agent answers a simple question through the Web UI", func(t *testing.T) {
		conv, reply := c.Start("Reply with the single word OK.")

		events := c.OpenStream(patient(), conv, reply).Drain()

		require.NotEmpty(t, events, "the agent answered")
		assert.Equal(t, "done", events[len(events)-1].Name)
		final := stack.MessagesIn(t, stack.Named(events, "delta")[len(stack.Named(events, "delta"))-1].Data)[0]
		assert.Equal(t, "complete", final.Status, "the reply: %q", final.Text)
		assert.NotEmpty(t, strings.TrimSpace(final.Text))
		assert.NotEmpty(t, stack.Attr(final.Node, "data-ttft-ms"), "the time to the first token was measured")
		assert.Equal(t, strings.TrimSpace(c.Raw(conv, reply).Body), final.Text, "what is stored is what was shown")
		t.Logf("%d updates, reply of %d characters, first token after %s ms", len(stack.Named(events, "delta")), len(final.Text), stack.Attr(final.Node, "data-ttft-ms"))
	})

	t.Run("Stop ends a real reply that is still being written", func(t *testing.T) {
		conv, reply := c.Start("Write a long essay of at least 800 words about the history of the printing press.")
		stream := c.OpenStream(patient(), conv, reply)
		for ev, ok := stream.Next(); ok; ev, ok = stream.Next() {
			if ev.Name == "delta" && stack.MessagesIn(t, ev.Data)[0].Text != "" {
				break
			}
		}

		pressed := time.Now()
		res := c.Stop(conv, reply)

		require.Equal(t, 200, res.Status, res.Body)
		if got := stack.MessagesIn(t, res.Body)[0].Status; got == "complete" {
			t.Skip("the agent finished the reply before Stop could be pressed: use a slower agent to try Stop")
		}
		assert.Equal(t, "stopped", stack.MessagesIn(t, res.Body)[0].Status)
		assert.Less(t, time.Since(pressed), time.Second)
		assert.NotEmpty(t, c.Raw(conv, reply).Body, "the text so far is kept")
	})
}
