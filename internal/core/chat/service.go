// Package chat implements the chat use case: sending messages, regenerating
// replies and streaming the agent's answer.
package chat

import (
	"context"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
)

// cleanupTimeout bounds work done after a request has gone away: saving a
// partial reply, removing a half-created conversation.
const cleanupTimeout = 5 * time.Second

// DefaultMaxReplyBytes is how big a reply may grow before it is cut off. It is
// the size beyond which the page stops interpreting Markdown, and a reply that
// large is not an answer a person will read.
const DefaultMaxReplyBytes = 1 << 20

// Dependencies are what the chat service needs.
type Dependencies struct {
	Conversations   resource.ConversationStore
	Messages        resource.MessageStore
	Agent           resource.AgentGateway
	Logger          *slog.Logger
	MaxMessageChars int
	// MaxReplyBytes is how big a reply may grow; zero means DefaultMaxReplyBytes.
	MaxReplyBytes int
}

// Service implements service.Chat.
type Service struct {
	deps Dependencies
	runs runs
}

var _ service.Chat = (*Service)(nil)

// NewService returns a chat service.
func NewService(d Dependencies) *Service {
	if d.MaxReplyBytes <= 0 {
		d.MaxReplyBytes = DefaultMaxReplyBytes
	}
	return &Service{deps: d}
}

// StartConversation implements service.Chat.
func (s *Service) StartConversation(ctx context.Context, userID uuid.UUID, content string) (service.Turn, error) {
	content, err := domain.NormalizeContent(content, s.deps.MaxMessageChars)
	if err != nil {
		return service.Turn{}, err
	}
	conv, err := s.deps.Conversations.Create(ctx, domain.Conversation{
		ID:     uuid.NewV7(),
		UserID: userID,
		Title:  domain.TitleFrom(content),
	})
	if err != nil {
		return service.Turn{}, fmt.Errorf("starting conversation: %w", err)
	}

	user, reply, err := s.deps.Messages.AppendPair(ctx, userID, conv.ID, content)
	if err != nil {
		// Do not leave an empty conversation behind, even if the request is gone.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if delErr := s.deps.Conversations.Delete(cleanupCtx, userID, conv.ID); delErr != nil {
			s.deps.Logger.ErrorContext(ctx, "removing a conversation whose first message failed",
				"conversation_id", conv.ID, "error", delErr)
		}
		return service.Turn{}, fmt.Errorf("sending first message: %w", err)
	}
	return service.Turn{Conversation: conv, User: user, Reply: reply}, nil
}

// Send implements service.Chat.
func (s *Service) Send(ctx context.Context, userID, convID uuid.UUID, content string) (service.Turn, error) {
	content, err := domain.NormalizeContent(content, s.deps.MaxMessageChars)
	if err != nil {
		return service.Turn{}, err
	}
	conv, err := s.deps.Conversations.Get(ctx, userID, convID)
	if err != nil {
		return service.Turn{}, err
	}
	user, reply, err := s.deps.Messages.AppendPair(ctx, userID, convID, content)
	if err != nil {
		return service.Turn{}, fmt.Errorf("sending message: %w", err)
	}
	return service.Turn{Conversation: conv, User: user, Reply: reply}, nil
}

// Regenerate implements service.Chat.
func (s *Service) Regenerate(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error) {
	reply, err := s.deps.Messages.ReplaceLastAssistant(ctx, userID, convID, msgID)
	if err != nil {
		return domain.Message{}, fmt.Errorf("regenerating reply: %w", err)
	}
	return reply, nil
}
