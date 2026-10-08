// Package resource declares the outbound ports: what the core needs from the
// outside world (databases, the agent). Adapters in internal/infrastructure
// implement them.
package resource

import (
	"context"
	"iter"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

// AgentGateway talks to the AI agent.
//
//go:generate mockery --name AgentGateway --filename agent_gateway.go
type AgentGateway interface {
	// Run sends one user turn and streams the reply as events. The run stops,
	// and the upstream request is canceled, when ctx is done or the caller
	// stops iterating. Errors wrap domain.ErrAgentUnavailable when the agent
	// cannot be reached, and are *domain.AgentError when the agent reports one.
	Run(ctx context.Context, req domain.AgentRequest) iter.Seq2[domain.AgentEvent, error]

	// Ping reports whether the agent answers and serves the configured app.
	Ping(ctx context.Context) error
}
