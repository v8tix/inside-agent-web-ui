// Package conversation implements listing, reading, renaming and deleting
// conversations.
package conversation

import (
	"context"
	"fmt"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
)

// Dependencies are what the conversation service needs.
type Dependencies struct {
	Conversations resource.ConversationStore
	Messages      resource.MessageStore
	ListLimit     int
}

// Service implements service.Conversation.
type Service struct {
	deps Dependencies
}

var _ service.Conversation = (*Service)(nil)

// NewService returns a conversation service.
func NewService(d Dependencies) *Service { return &Service{deps: d} }

// List implements service.Conversation.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]domain.Conversation, error) {
	return s.deps.Conversations.List(ctx, userID, s.deps.ListLimit)
}

// Get implements service.Conversation. The conversation is read first, so a
// missing or foreign one stops there.
func (s *Service) Get(ctx context.Context, userID, convID uuid.UUID) (service.ConversationView, error) {
	conv, err := s.deps.Conversations.Get(ctx, userID, convID)
	if err != nil {
		return service.ConversationView{}, err
	}
	msgs, err := s.deps.Messages.List(ctx, userID, convID)
	if err != nil {
		return service.ConversationView{}, fmt.Errorf("reading messages: %w", err)
	}
	return service.ConversationView{Conversation: conv, Messages: msgs}, nil
}

// Find implements service.Conversation.
func (s *Service) Find(ctx context.Context, userID, convID uuid.UUID) (domain.Conversation, error) {
	return s.deps.Conversations.Get(ctx, userID, convID)
}

// Message implements service.Conversation.
func (s *Service) Message(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error) {
	return s.deps.Messages.Get(ctx, userID, convID, msgID)
}

// Rename implements service.Conversation.
func (s *Service) Rename(ctx context.Context, userID, convID uuid.UUID, title string) (string, error) {
	title, err := domain.NormalizeTitle(title)
	if err != nil {
		return "", err
	}
	if err := s.deps.Conversations.Rename(ctx, userID, convID, title); err != nil {
		return "", err
	}
	return title, nil
}

// Delete implements service.Conversation.
func (s *Service) Delete(ctx context.Context, userID, convID uuid.UUID) error {
	return s.deps.Conversations.Delete(ctx, userID, convID)
}
