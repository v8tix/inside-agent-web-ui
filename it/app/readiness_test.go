package app_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/it/internal/stack"
)

// /readyz is what a load balancer or a container runtime asks before sending
// people here, so it has to follow the agent: ready while the agent answers,
// unavailable while it does not, and ready again when it is back. The answer is
// kept for a second, so the changes are waited for.
func TestReadiness_FollowsTheAgent(t *testing.T) {
	s := stack.Start(t)
	c := s.Anonymous()
	status := func() int { return c.Get("/readyz").Status }

	first := c.Get("/readyz")
	require.Equal(t, 200, first.Status, first.Body)
	assert.JSONEq(t, `{"status":"ok","checks":[{"name":"database","status":"ok"},{"name":"agent","status":"ok"}]}`, first.Body)

	s.Agent.Down()
	require.Eventually(t, func() bool { return status() == 503 }, 5*time.Second, 100*time.Millisecond, "unavailable while the agent does not answer")
	down := c.Get("/readyz")
	assert.JSONEq(t, `{"status":"unavailable","checks":[{"name":"database","status":"ok"},{"name":"agent","status":"failed"}]}`, down.Body,
		"it names the check that failed, and nothing of why")
	assert.NotContains(t, down.Body, s.Agent.URL, "the agent's address stays private")
	assert.Equal(t, 200, c.Get("/healthz").Status, "while the process itself is alive")

	s.Agent.Up()
	require.Eventually(t, func() bool { return status() == 200 }, 5*time.Second, 100*time.Millisecond, "ready again when the agent is back")
}
