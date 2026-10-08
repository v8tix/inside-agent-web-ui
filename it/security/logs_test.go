package security_test

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
	"github.com/v8tix/inside-agent-web-ui/it/internal/stack"
)

// What people write, what the agent answers, and the passwords they type are not
// the server's to keep in its log, however much goes wrong: the log is read by
// whoever runs the server, and is kept by whatever collects it.
func TestLogs_NeverHoldWhatPeopleWrite(t *testing.T) {
	const (
		said     = "zebra-quartz-7731"
		titled   = "renamed-to-giraffe-4410"
		wrongPwd = "not-the-password-9082"
	)
	var logs bytes.Buffer
	// A #slow reply takes 300 ms and the agent's server lets a reply stream for
	// 150, so that one of them can be cut off with some of its text written.
	s := stack.Start(t, stack.WithLogs(&logs), stack.WithModel(testagent.WithSlow(10*time.Millisecond, 30)),
		stack.WithAgentServer(testagent.WithSSEWriteTimeout(150*time.Millisecond)))
	c := s.SignedIn()
	ctx := t.Context()

	// A conversation with everything done to it that writes something down.
	conv, reply := c.Start("tell me about " + said)
	c.OpenStream(ctx, conv, reply).Drain()
	require.Equal(t, 200, c.Raw(conv, reply).Status)
	regenerated := c.Regenerate(conv, reply)
	require.Equal(t, 200, regenerated.Status, regenerated.Body)
	c.OpenStream(ctx, conv, stack.MessagesIn(t, regenerated.Body)[0].ID).Drain()
	second := c.Send(conv, "and "+said+" again")
	c.OpenStream(ctx, conv, second).Drain()
	third := c.Send(conv, "once more "+said+" #slow")
	require.Equal(t, 200, c.Stop(conv, third).Status)
	require.Equal(t, 200, c.Do(http.MethodPatch, "/c/"+conv, url.Values{"title": {titled}}, "HX-Request", "true").Status)

	// And what goes wrong: an answer cut off with some of it written, the agent
	// gone, a password that is wrong, a message that is too long.
	cut := c.Send(conv, "a long one about "+said+" #slow")
	cutOff := stack.Named(c.OpenStream(ctx, conv, cut).Drain(), "delta")
	require.NotEmpty(t, cutOff)
	require.Contains(t, cutOff[len(cutOff)-1].Data, `data-status="failed"`)
	require.Contains(t, cutOff[len(cutOff)-1].Data, "tick 1", "some of the answer was written before it was cut off")
	s.Agent.Down()
	failed := c.Send(conv, "are you there, "+said+"?")
	c.OpenStream(ctx, conv, failed).Drain()
	s.Agent.Up()
	refused := s.Anonymous().Do(http.MethodPost, "/login", url.Values{"email": {stack.Email}, "password": {wrongPwd}})
	assert.NotEqual(t, http.StatusSeeOther, refused.Status)
	tooLong := c.Post("/c/"+conv+"/messages", url.Values{"content": {said + strings.Repeat(" x", 20_000)}}, "HX-Request", "true")
	assert.GreaterOrEqual(t, tooLong.Status, 400)

	s.Shutdown()
	log := logs.String()

	require.Contains(t, log, `"request_id"`, "the log was captured")
	require.Contains(t, log, `"msg":"reply failed"`, "and holds the failures, in words of the server's own")
	for _, secret := range []string{said, titled, wrongPwd, stack.Password, "You said", "tick 1"} {
		assert.NotContains(t, log, secret)
	}
}
