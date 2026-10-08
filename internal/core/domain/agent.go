package domain

import (
	"errors"
	"uuid"
)

// AgentRequest is one user turn sent to the agent. The agent keeps its own
// session per conversation, keyed by the conversation ID.
type AgentRequest struct {
	UserID         uuid.UUID
	ConversationID uuid.UUID
	Text           string
}

// AgentEventKind says what an AgentEvent carries.
type AgentEventKind string

// Agent event kinds.
const (
	// AgentDelta is a piece of the reply, to append to the segment in progress.
	AgentDelta AgentEventKind = "delta"
	// AgentFinal is the complete text of one segment of the reply. It replaces
	// the deltas of that segment; clients must never append both.
	AgentFinal AgentEventKind = "final"
	// AgentUsage reports token counts for one model call.
	AgentUsage AgentEventKind = "usage"
	// AgentToolCall says the agent called a tool.
	AgentToolCall AgentEventKind = "tool_call"
)

// AgentEvent is one thing the agent did while producing a reply. Reasoning
// ("thought") parts never become events.
type AgentEvent struct {
	Kind  AgentEventKind
	Text  string // AgentDelta, AgentFinal
	Usage Usage  // AgentUsage
	Tool  string // AgentToolCall
}

// ErrIncompleteStream means the agent's stream ended without a complete reply.
var ErrIncompleteStream = errors.New("agent stream ended before the reply was complete")

// AgentError is an error the agent itself reported. Detail is for logs; it can
// contain internals and must not be shown to users.
type AgentError struct {
	Detail string
}

// Error implements error.
func (e *AgentError) Error() string { return "agent reported an error: " + e.Detail }
