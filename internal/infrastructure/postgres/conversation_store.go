package postgres

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource"
)

var _ resource.ConversationStore = (*ConversationStore)(nil)

// ConversationStore is the PostgreSQL implementation of resource.ConversationStore.
type ConversationStore struct {
	pool *pgxpool.Pool
	sql  conversationSQL
}

// NewConversationStore returns a store backed by pool, with its SQL from sql.
func NewConversationStore(pool *pgxpool.Pool, sql Statements) *ConversationStore {
	return &ConversationStore{pool: pool, sql: sql.conversation}
}

func scanConversation(row pgx.Row) (domain.Conversation, error) {
	var c domain.Conversation
	err := row.Scan(&c.ID, &c.UserID, &c.Title, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// Create implements resource.ConversationStore.
func (s *ConversationStore) Create(ctx context.Context, c domain.Conversation) (domain.Conversation, error) {
	row := s.pool.QueryRow(ctx, s.sql.create, c.ID, c.UserID, c.Title)
	created, err := scanConversation(row)
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("creating conversation: %w", err)
	}
	return created, nil
}

// Get implements resource.ConversationStore.
//
// Pattern: Ownership in the Query
func (s *ConversationStore) Get(ctx context.Context, userID, id uuid.UUID) (domain.Conversation, error) {
	c, err := scanConversation(s.pool.QueryRow(ctx, s.sql.get, id, userID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Conversation{}, domain.ErrNotFound
	case err != nil:
		return domain.Conversation{}, fmt.Errorf("getting conversation: %w", err)
	}
	return c, nil
}

// List implements resource.ConversationStore.
func (s *ConversationStore) List(ctx context.Context, userID uuid.UUID, limit int) ([]domain.Conversation, error) {
	rows, err := s.pool.Query(ctx, s.sql.list, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing conversations: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Conversation, error) {
		return scanConversation(row)
	})
	if err != nil {
		return nil, fmt.Errorf("listing conversations: %w", err)
	}
	return list, nil
}

// Rename implements resource.ConversationStore.
func (s *ConversationStore) Rename(ctx context.Context, userID, id uuid.UUID, title string) error {
	tag, err := s.pool.Exec(ctx, s.sql.rename, id, userID, title)
	if err != nil {
		return fmt.Errorf("renaming conversation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Delete implements resource.ConversationStore.
func (s *ConversationStore) Delete(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, s.sql.remove, id, userID)
	if err != nil {
		return fmt.Errorf("deleting conversation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
