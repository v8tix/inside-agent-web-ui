package resource

import (
	"context"
	"time"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

// Claimed is a reply that was just moved to streaming, together with the user
// message it answers.
type Claimed struct {
	Reply  domain.Message
	Prompt string
}

// Outcome is how a reply ended.
type Outcome struct {
	Content string
	Status  domain.Status // must be terminal
	Error   string
	Meta    domain.Meta
}

// MessageStore persists messages. Methods that take a user ID are scoped to
// the owner of the conversation: someone else's conversation or message is
// indistinguishable from a missing one and yields domain.ErrNotFound.
//
//go:generate mockery --name MessageStore --filename message_store.go
type MessageStore interface {
	// AppendPair adds the user's message (complete) and an empty assistant
	// reply (pending) to the conversation in one transaction, with the next two
	// sequence numbers, and bumps the conversation's activity time.
	// It returns domain.ErrInvalidState when the conversation's latest reply
	// is not finished yet: a conversation has one turn in flight at a time.
	AppendPair(ctx context.Context, userID, convID uuid.UUID, content string) (user, reply domain.Message, err error)

	// List returns the conversation's messages in order.
	List(ctx context.Context, userID, convID uuid.UUID) ([]domain.Message, error)

	// Get returns one message of the conversation.
	Get(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error)

	// Claim moves a pending reply to streaming; exactly one concurrent caller
	// wins. The loser gets domain.ErrAlreadyStreaming. A reply in any other
	// state, or a user message, is domain.ErrInvalidState.
	Claim(ctx context.Context, userID, convID, msgID uuid.UUID) (Claimed, error)

	// Finish records how a claimed reply ended. The reply must be streaming
	// and the status terminal, else domain.ErrInvalidState.
	Finish(ctx context.Context, msgID uuid.UUID, outcome Outcome) error

	// ReplaceLastAssistant swaps the reply msgID, which must be the last message
	// of the conversation and finished (complete, stopped or failed), for a new
	// pending one with the same sequence number. Used by regenerate and retry.
	// A message that is not in the conversation is domain.ErrNotFound; one that
	// is there but is not the last finished reply is domain.ErrInvalidState.
	ReplaceLastAssistant(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error)

	// RecoverInterrupted marks replies that have been streaming for longer than
	// olderThan as failed. A crash leaves such replies behind; without this
	// they could never be claimed again. It returns how many it fixed.
	RecoverInterrupted(ctx context.Context, olderThan time.Duration) (int, error)
}
