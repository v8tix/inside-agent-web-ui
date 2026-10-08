package resource

import (
	"context"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

// ConversationStore persists conversations. Every method that reads or changes a
// conversation is scoped to its owner: another user's conversation is
// indistinguishable from a missing one and yields domain.ErrNotFound. Create
// stores the owner it is given.
//
//go:generate mockery --name ConversationStore --filename conversation_store.go
type ConversationStore interface {
	// Create stores c and returns it with its timestamps filled in.
	Create(ctx context.Context, c domain.Conversation) (domain.Conversation, error)

	// Get returns one of the user's conversations.
	Get(ctx context.Context, userID, id uuid.UUID) (domain.Conversation, error)

	// List returns up to limit of the user's conversations, most recently
	// active first.
	List(ctx context.Context, userID uuid.UUID, limit int) ([]domain.Conversation, error)

	// Rename sets a new title. It does not change the conversation's place in
	// List, which follows message activity.
	Rename(ctx context.Context, userID, id uuid.UUID, title string) error

	// Delete removes a conversation and all its messages.
	Delete(ctx context.Context, userID, id uuid.UUID) error
}
