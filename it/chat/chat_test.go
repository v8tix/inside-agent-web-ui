// The Web UI end to end over real HTTP: the application as the program runs it,
// a real PostgreSQL, the scripted ADK agent, and a client that reads the event
// streams the way a browser does. Every case starts its own stack and ends with
// a check that nothing of the application is left running.
//
// Group A: needs Docker.
package chat_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
	"github.com/v8tix/inside-agent-web-ui/it/internal/leak"
	"github.com/v8tix/inside-agent-web-ui/it/internal/stack"
)

// ticks is what the #slow script says: numbered chunks, each followed by a space.
func ticks(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "tick %d ", i+1)
	}
	return b.String()
}

// within bounds what a case waits for, so a stuck stream fails the case instead
// of hanging the run.
func within(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return ctx
}

// replyOf is the assistant article of an event.
func replyOf(t *testing.T, ev stack.Event) stack.Message {
	t.Helper()
	messages := stack.MessagesIn(t, ev.Data)
	require.Len(t, messages, 1, ev.Data)
	return messages[0]
}

// the deltas of a stream, as messages.
func deltas(t *testing.T, events []stack.Event) []stack.Message {
	t.Helper()
	var out []stack.Message
	for _, ev := range stack.Named(events, "delta") {
		out = append(out, replyOf(t, ev))
	}
	return out
}

func lastOf(t *testing.T, messages []stack.Message) stack.Message {
	t.Helper()
	require.NotEmpty(t, messages)
	return messages[len(messages)-1]
}

func TestChatSuite(t *testing.T) {
	t.Run("a reply arrives in pieces and is saved as it was shown (SC-1)", func(t *testing.T) {
		leak.Check(t)
		s := stack.Start(t, stack.WithModel(testagent.WithSlow(15*time.Millisecond, 30)))
		c := s.SignedIn()
		conv, reply := c.Start("Tell me something #slow")

		events := c.OpenStream(within(t, 20*time.Second), conv, reply).Drain()

		require.NotEmpty(t, events)
		assert.Equal(t, "done", events[len(events)-1].Name, "the stream ends with done")
		assert.Len(t, stack.Named(events, "done"), 1)
		shown := deltas(t, events)
		var streaming int
		for i, m := range shown {
			if m.Status == "streaming" {
				streaming++
			}
			if i > 0 {
				assert.True(t, strings.HasPrefix(m.Text, shown[i-1].Text), "each update holds the reply so far: %q then %q", shown[i-1].Text, m.Text)
			}
		}
		assert.GreaterOrEqual(t, streaming, 2, "at least two incremental updates before the end")
		final := lastOf(t, shown)
		assert.Equal(t, "complete", final.Status)

		saved := c.Raw(conv, reply)
		assert.Equal(t, 200, saved.Status)
		assert.Equal(t, ticks(30), saved.Body, "what is stored is what the agent said")
		assert.Equal(t, strings.TrimSpace(saved.Body), final.Text, "and the last update shows it")

		reloaded := c.Messages(conv)
		require.Len(t, reloaded, 2)
		assert.Equal(t, "user", reloaded[0].Role)
		assert.Equal(t, "Tell me something #slow", reloaded[0].Text)
		assert.Equal(t, final.Text, reloaded[1].Text, "a reload shows the same conversation")
		assert.Equal(t, "complete", reloaded[1].Status)
		for _, attr := range []string{"data-ttft-ms", "data-duration-ms", "data-prompt-tokens", "data-completion-tokens"} {
			assert.NotEmpty(t, stack.Attr(final.Node, attr), "%s is set once the reply is finished (SC-22)", attr)
		}
		assert.Equal(t, int64(1), s.Model.Probe().Runs())
	})

	t.Run("CHAT_STREAM_FLUSH_INTERVAL sets how often the page is updated", func(t *testing.T) {
		leak.Check(t)
		// The same reply, forty pieces ten milliseconds apart, under two settings.
		updates := func(interval string) int {
			s := stack.Start(t, stack.WithSetting("CHAT_STREAM_FLUSH_INTERVAL", interval), stack.WithModel(testagent.WithSlow(10*time.Millisecond, 40)))
			c := s.SignedIn()
			conv, reply := c.Start("Tell me something #slow")
			return len(deltas(t, c.OpenStream(within(t, 20*time.Second), conv, reply).Drain()))
		}

		often, rarely := updates("5ms"), updates("300ms")

		assert.Greater(t, often, 3*rarely, "%d updates every 5 ms against %d every 300 ms", often, rarely)
		assert.GreaterOrEqual(t, rarely, 2, "the first and the last, at least")
	})

	t.Run("an answer the agent's server cuts off is kept as far as it got, and the page says so", func(t *testing.T) {
		leak.Check(t)
		// Fifty chunks 20 ms apart take a second; the agent's server lets a reply
		// stream for 300 ms. A real agent does this to a long answer after two
		// minutes (the launcher's --sse-write-timeout).
		s := stack.Start(t, stack.WithModel(testagent.WithSlow(20*time.Millisecond, 50)),
			stack.WithAgentServer(testagent.WithSSEWriteTimeout(300*time.Millisecond)))
		c := s.SignedIn()
		conv, reply := c.Start("Tell me a long story #slow")

		events := c.OpenStream(within(t, 20*time.Second), conv, reply).Drain()

		final := lastOf(t, deltas(t, events))
		assert.Equal(t, "failed", final.Status)
		problem := stack.Find(final.Node, stack.TestID("error"))
		require.NotNil(t, problem, "an error bubble")
		assert.Equal(t, domain.FailureIncomplete, strings.TrimSpace(stack.Text(problem)))
		assert.True(t, strings.HasPrefix(final.Text, "tick 1 "), "what arrived is still there: %q", final.Text)
		assert.Less(t, len(final.Text), len(strings.TrimSpace(ticks(50))), "and it is not the whole reply")
		assert.NotNil(t, stack.Find(final.Node, stack.TestID("retry")), "with a Retry button")

		saved := c.Raw(conv, reply)
		require.Equal(t, 200, saved.Status)
		assert.Equal(t, strings.TrimSpace(saved.Body), final.Text, "a reload shows what the stream showed")
		var status string
		require.NoError(t, s.Pool.QueryRow(t.Context(), `SELECT status FROM messages WHERE id = $1`, reply).Scan(&status))
		assert.Equal(t, "failed", status)
	})

	t.Run("Stop ends the agent's run within a second and keeps the text so far (SC-2)", func(t *testing.T) {
		leak.Check(t)
		s := stack.Start(t, stack.WithModel(testagent.WithSlow(20*time.Millisecond, 400)))
		c := s.SignedIn()
		conv, reply := c.Start("Go on and on #slow")
		stream := c.OpenStream(within(t, 20*time.Second), conv, reply)
		for ev, ok := stream.Next(); ok; ev, ok = stream.Next() {
			if ev.Name == "delta" && replyOf(t, ev).Text != "" {
				break // some of the reply is on the page
			}
		}

		pressed := time.Now()
		res := c.Stop(conv, reply)

		require.Equal(t, 200, res.Status, res.Body)
		require.Equal(t, "stopped", stack.MessagesIn(t, res.Body)[0].Status, "the answer to Stop is the reply as saved")
		require.Eventually(t, func() bool { return s.Model.Probe().Cancelled() == 1 }, time.Second, 5*time.Millisecond,
			"the agent saw its run cancelled")
		assert.Less(t, time.Since(pressed), time.Second)
		rest := stream.Drain()
		assert.Equal(t, "stopped", lastOf(t, deltas(t, rest)).Status, "the open stream is told, and then ends")
		assert.Equal(t, "done", rest[len(rest)-1].Name)

		kept := c.Raw(conv, reply)
		require.Equal(t, 200, kept.Status)
		assert.True(t, strings.HasPrefix(kept.Body, "tick 1 "), "the text so far is kept: %q", kept.Body)
		assert.Less(t, len(kept.Body), len(ticks(400)), "and it is not the whole reply")
		var status string
		require.NoError(t, s.Pool.QueryRow(t.Context(), `SELECT status FROM messages WHERE id = $1`, reply).Scan(&status))
		assert.Equal(t, "stopped", status)
	})

	t.Run("closing the page mid-reply cancels the agent's run and saves the text so far (SC-2)", func(t *testing.T) {
		leak.Check(t)
		s := stack.Start(t, stack.WithModel(testagent.WithSlow(20*time.Millisecond, 400)))
		c := s.SignedIn()
		conv, reply := c.Start("Keep going #slow")
		stream := c.OpenStream(within(t, 20*time.Second), conv, reply)
		for ev, ok := stream.Next(); ok; ev, ok = stream.Next() {
			if ev.Name == "delta" && replyOf(t, ev).Text != "" {
				break
			}
		}

		stream.Close() // the tab goes away

		require.Eventually(t, func() bool { return s.Model.Probe().Cancelled() == 1 }, 2*time.Second, 10*time.Millisecond,
			"the agent saw its run cancelled")
		var status, content string
		require.Eventually(t, func() bool {
			err := s.Pool.QueryRow(t.Context(), `SELECT status, content FROM messages WHERE id = $1`, reply).Scan(&status, &content)
			return err == nil && status == "stopped"
		}, 2*time.Second, 10*time.Millisecond, "the reply was saved as stopped")
		assert.True(t, strings.HasPrefix(content, "tick 1 "), "with the text so far: %q", content)
		assert.Less(t, len(content), len(ticks(400)))
		assert.Equal(t, content, c.Raw(conv, reply).Body, "and it can be read back")
	})

	t.Run("Regenerate replaces only the latest reply (SC-3)", func(t *testing.T) {
		leak.Check(t)
		s := stack.Start(t)
		c := s.SignedIn()
		conv, first := c.Start("first question")
		c.OpenStream(within(t, 20*time.Second), conv, first).Drain()
		second := c.Send(conv, "second question")
		c.OpenStream(within(t, 20*time.Second), conv, second).Drain()
		before := c.Messages(conv)
		require.Len(t, before, 4)
		require.Equal(t, int64(2), s.Model.Probe().Runs())

		res := c.Regenerate(conv, second)

		require.Equal(t, 200, res.Status, res.Body)
		fresh := stack.MessagesIn(t, res.Body)
		require.Len(t, fresh, 1, "the answer is the new reply")
		c.OpenStream(within(t, 20*time.Second), conv, fresh[0].ID).Drain()
		after := c.Messages(conv)
		require.Len(t, after, 4, "still two turns")
		for i := range 3 {
			assert.Equal(t, before[i].ID, after[i].ID, "message %d is untouched", i)
			assert.Equal(t, before[i].Text, after[i].Text, "message %d is untouched", i)
		}
		assert.Equal(t, "complete", after[3].Status)
		assert.Equal(t, "You said: second question", after[3].Text, "the new reply answers the same question")
		assert.Equal(t, int64(3), s.Model.Probe().Runs(), "the agent was asked once more")

		earlier := c.Regenerate(conv, first)
		assert.Equal(t, 409, earlier.Status, "an earlier reply can't be regenerated")
		assert.Equal(t, int64(3), s.Model.Probe().Runs())
	})

	t.Run("with the agent down the reply says so, and Retry works once it is back (SC-4)", func(t *testing.T) {
		leak.Check(t)
		s := stack.Start(t)
		c := s.SignedIn()
		s.Agent.Down()
		started := time.Now()
		conv, reply := c.Start("Are you there?")

		events := c.OpenStream(within(t, 20*time.Second), conv, reply).Drain()

		assert.Less(t, time.Since(started), 5*time.Second, "the failure shows within 5 s")
		final := lastOf(t, deltas(t, events))
		assert.Equal(t, "failed", final.Status)
		problem := stack.Find(final.Node, stack.TestID("error"))
		require.NotNil(t, problem, "an error bubble")
		assert.Equal(t, domain.FailureUnavailable, strings.TrimSpace(stack.Text(problem)), "in words that give nothing away")
		for _, ev := range events {
			assert.NotContains(t, ev.Data, s.Agent.URL, "the agent's address stays private")
		}
		require.NotNil(t, stack.Find(final.Node, stack.TestID("retry")), "with a Retry button")

		s.Agent.Up()
		res := c.Regenerate(conv, reply)

		require.Equal(t, 200, res.Status, res.Body)
		retried := stack.MessagesIn(t, res.Body)[0]
		again := deltas(t, c.OpenStream(within(t, 20*time.Second), conv, retried.ID).Drain())
		assert.Equal(t, "complete", lastOf(t, again).Status)
		assert.Equal(t, "You said: Are you there?", lastOf(t, again).Text, "the prompt was kept")
		page := c.Messages(conv)
		require.Len(t, page, 2, "one question and one reply")
		assert.Equal(t, "Are you there?", page[0].Text)
	})

	t.Run("a finished reply is replayed without asking the agent again (SC-5)", func(t *testing.T) {
		leak.Check(t)
		s := stack.Start(t)
		c := s.SignedIn()
		conv, reply := c.Start("Say it once")
		first := c.OpenStream(within(t, 20*time.Second), conv, reply).Drain()
		require.Equal(t, int64(1), s.Model.Probe().Runs())

		again := c.OpenStream(within(t, 20*time.Second), conv, reply).Drain()

		require.Len(t, again, 2, "the final reply and done")
		assert.Equal(t, "delta", again[0].Name)
		assert.Equal(t, "done", again[1].Name)
		assert.Equal(t, lastOf(t, deltas(t, first)).Text, replyOf(t, again[0]).Text)
		assert.Equal(t, "complete", replyOf(t, again[0]).Status)
		assert.Equal(t, int64(1), s.Model.Probe().Runs(), "no second run")
		page := c.Get("/c/" + conv)
		assert.NotContains(t, page.Body, "sse-connect", "a finished reply opens no stream when the page is loaded")
	})

	t.Run("the agent's reasoning never reaches the page, and no text is shown twice (SC-7)", func(t *testing.T) {
		leak.Check(t)
		s := stack.Start(t)
		c := s.SignedIn()
		conv, reply := c.Start("What is two plus two? #think")

		events := c.OpenStream(within(t, 20*time.Second), conv, reply).Drain()

		for _, ev := range events {
			assert.NotContains(t, ev.Data, "think about that", "the reasoning is not sent")
		}
		saved := c.Raw(conv, reply)
		assert.Equal(t, "You said: What is two plus two? #think", saved.Body, "exactly what the agent answered, once")
		assert.Equal(t, 1, strings.Count(saved.Body, "You said:"))
		assert.Equal(t, "You said: What is two plus two? #think", lastOf(t, deltas(t, events)).Text)
		assert.NotContains(t, c.Get("/c/"+conv).Body, "think about that")
		assert.Equal(t, int64(1), s.Model.Probe().Runs())
	})

	t.Run("shutting down ends the replies being written and saves their text (SC-19)", func(t *testing.T) {
		leak.Check(t)
		s := stack.Start(t,
			stack.WithModel(testagent.WithSlow(20*time.Millisecond, 500)),
			stack.WithSetting("APP_SHUTDOWN_GRACE", "5s"))
		c := s.SignedIn()
		conv, reply := c.Start("Keep going #slow")
		stream := c.OpenStream(within(t, 30*time.Second), conv, reply)
		for ev, ok := stream.Next(); ok; ev, ok = stream.Next() {
			if ev.Name == "delta" && replyOf(t, ev).Text != "" {
				break
			}
		}

		took := s.Shutdown() // what SIGTERM does

		assert.Less(t, took, 2*time.Second, "well within the 5 s grace")
		rest := stream.Drain()
		require.NotEmpty(t, rest)
		assert.Equal(t, "stopped", lastOf(t, deltas(t, rest)).Status, "the reader is told the reply was stopped")
		assert.Equal(t, "done", rest[len(rest)-1].Name)
		var status, content string
		require.NoError(t, s.Pool.QueryRow(t.Context(), `SELECT status, content FROM messages WHERE id = $1`, reply).Scan(&status, &content))
		assert.Equal(t, "stopped", status)
		assert.True(t, strings.HasPrefix(content, "tick 1 "), "the text so far is saved: %q", content)
		assert.Equal(t, int64(1), s.Model.Probe().Cancelled(), "and the agent's run was cancelled")
	})
}
