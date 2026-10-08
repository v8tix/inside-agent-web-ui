// Package service declares the inbound ports: the use cases the HTTP adapter
// calls. The packages under internal/core implement them.
package service

import (
	"context"
	"iter"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

// Turn is what sending a message produces: the user's message and the empty
// reply that is waiting to be streamed.
type Turn struct {
	Conversation domain.Conversation
	User         domain.Message
	Reply        domain.Message
}

// StreamKind says what a StreamEvent carries.
type StreamKind string

// Stream event kinds.
const (
	// StreamSnapshot carries the whole reply so far.
	StreamSnapshot StreamKind = "snapshot"
	// StreamDone ends the stream and carries the final state.
	StreamDone StreamKind = "done"
)

// StreamEvent is one step of a streamed reply. Snapshots carry the full text so
// far rather than a fragment, so a consumer can drop or coalesce them freely.
//
// Pattern: Whole-Text Snapshots
type StreamEvent struct {
	Kind StreamKind
	Text string
	// The fields below are set on StreamDone only.
	Status domain.Status
	// Error is a message for the user when Status is failed.
	Error string
	Meta  domain.Meta
}

// Chat is the chat use case.
//
//go:generate mockery --name Chat --filename chat.go
type Chat interface {
	// StartConversation creates a conversation named after content and sends
	// content as its first message.
	StartConversation(ctx context.Context, userID uuid.UUID, content string) (Turn, error)

	// Send adds a message to a conversation. Bad input is a
	// *domain.ValidationError; a turn already in flight is
	// domain.ErrInvalidState.
	Send(ctx context.Context, userID, convID uuid.UUID, content string) (Turn, error)

	// Regenerate replaces the reply msgID with a new pending one. The reply must
	// be the conversation's last and finished: another is domain.ErrInvalidState,
	// and one that is not in the conversation is domain.ErrNotFound. Retrying a
	// failed reply is the same call.
	Regenerate(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error)

	// Stream produces a pending reply by running the agent, and yields the
	// text as it grows, then a StreamDone. A reply that already finished is
	// replayed from storage without calling the agent. A reply another
	// connection is producing is domain.ErrAlreadyStreaming.
	//
	// When ctx ends or the caller stops iterating, the agent run is canceled
	// and the text so far is saved as a stopped reply.
	Stream(ctx context.Context, userID, convID, msgID uuid.UUID) iter.Seq2[StreamEvent, error]

	// Stop ends a reply that is being written, or that was never started, and
	// returns it as saved: stopped, with the text so far. A reply that already
	// finished is returned as it is.
	Stop(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error)
}
