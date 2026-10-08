package service

import (
	"context"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

// ConversationView is a conversation with its messages in order.
type ConversationView struct {
	Conversation domain.Conversation
	Messages     []domain.Message
}

// Conversation manages a user's conversations. Someone else's conversation is
// indistinguishable from a missing one: domain.ErrNotFound.
//
//go:generate mockery --name Conversation --filename conversation.go
type Conversation interface {
	// List returns the user's conversations, most recently active first.
	List(ctx context.Context, userID uuid.UUID) ([]domain.Conversation, error)

	// Get returns a conversation with its messages.
	Get(ctx context.Context, userID, convID uuid.UUID) (ConversationView, error)

	// Find returns one conversation without its messages.
	Find(ctx context.Context, userID, convID uuid.UUID) (domain.Conversation, error)

	// Message returns one message of a conversation.
	Message(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error)

	// Rename sets a new title and returns the title as stored. Bad input is a
	// *domain.ValidationError.
	Rename(ctx context.Context, userID, convID uuid.UUID, title string) (string, error)

	// Delete removes a conversation and its messages.
	Delete(ctx context.Context, userID, convID uuid.UUID) error
}
