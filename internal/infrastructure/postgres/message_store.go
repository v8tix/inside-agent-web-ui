package postgres

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource"
)

var _ resource.MessageStore = (*MessageStore)(nil)

// MessageStore is the PostgreSQL implementation of resource.MessageStore.
type MessageStore struct {
	pool *pgxpool.Pool
	sql  messageSQL
}

// NewMessageStore returns a store backed by pool, with its SQL from sql.
func NewMessageStore(pool *pgxpool.Pool, sql Statements) *MessageStore {
	return &MessageStore{pool: pool, sql: sql.message}
}

func scanMessage(row pgx.Row, extra ...any) (domain.Message, error) {
	var m domain.Message
	var meta []byte
	dest := append([]any{&m.ID, &m.ConversationID, &m.Seq, &m.Role, &m.Content, &m.Status, &m.Error, &meta, &m.CreatedAt, &m.UpdatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return domain.Message{}, err
	}
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &m.Meta); err != nil {
			return domain.Message{}, fmt.Errorf("decoding message metadata: %w", err)
		}
	}
	return m, nil
}

// lockConversation takes the conversation's row lock, which serializes
// changes to its messages, and proves the user owns it.
func (s *MessageStore) lockConversation(ctx context.Context, tx pgx.Tx, userID, convID uuid.UUID) error {
	var id uuid.UUID
	err := tx.QueryRow(ctx, s.sql.lockConversation, convID, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	return err
}

type lastMessage struct {
	id     uuid.UUID
	seq    int
	role   domain.Role
	status domain.Status
}

// latestMessage returns the highest-numbered message, or ok=false when the
// conversation has none.
func (s *MessageStore) latestMessage(ctx context.Context, tx pgx.Tx, convID uuid.UUID) (last lastMessage, ok bool, err error) {
	err = tx.QueryRow(ctx, s.sql.latest, convID).Scan(&last.id, &last.seq, &last.role, &last.status)
	if errors.Is(err, pgx.ErrNoRows) {
		return lastMessage{}, false, nil
	}
	return last, err == nil, err
}

func (s *MessageStore) insertMessage(ctx context.Context, tx pgx.Tx, convID uuid.UUID, seq int, role domain.Role, content string, status domain.Status) (domain.Message, error) {
	row := tx.QueryRow(ctx, s.sql.insert, uuid.NewV7(), convID, seq, role, content, status)
	return scanMessage(row)
}

func (s *MessageStore) touchConversation(ctx context.Context, tx pgx.Tx, convID uuid.UUID) error {
	_, err := tx.Exec(ctx, s.sql.touchConversation, convID)
	return err
}

// AppendPair implements resource.MessageStore.
//
// Pattern: Row-Locked Sequence
func (s *MessageStore) AppendPair(ctx context.Context, userID, convID uuid.UUID, content string) (user, reply domain.Message, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockConversation(ctx, tx, userID, convID); err != nil {
			return err
		}
		last, found, err := s.latestMessage(ctx, tx, convID)
		if err != nil {
			return err
		}
		if found && last.role == domain.RoleAssistant && !last.status.Terminal() {
			return fmt.Errorf("the previous reply is still %s: %w", last.status, domain.ErrInvalidState)
		}
		if user, err = s.insertMessage(ctx, tx, convID, last.seq+1, domain.RoleUser, content, domain.StatusComplete); err != nil {
			return err
		}
		if reply, err = s.insertMessage(ctx, tx, convID, last.seq+2, domain.RoleAssistant, "", domain.StatusPending); err != nil {
			return err
		}
		return s.touchConversation(ctx, tx, convID)
	})
	if err != nil {
		return domain.Message{}, domain.Message{}, wrapStore("appending messages", err)
	}
	return user, reply, nil
}

// List implements resource.MessageStore. It asks in two statements: whether the
// user owns the conversation, then for its messages. A conversation that is
// deleted between the two lists as empty, not as missing. Joining the two would
// not give the caller one view of the conversation: the use case reads the
// conversation itself first, in a statement of its own.
func (s *MessageStore) List(ctx context.Context, userID, convID uuid.UUID) ([]domain.Message, error) {
	var owned bool
	if err := s.pool.QueryRow(ctx, s.sql.ownsConversation, convID, userID).Scan(&owned); err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	if !owned {
		return nil, domain.ErrNotFound
	}
	rows, err := s.pool.Query(ctx, s.sql.list, convID)
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	msgs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Message, error) { return scanMessage(row) })
	if err != nil {
		return nil, fmt.Errorf("listing messages: %w", err)
	}
	return msgs, nil
}

// Get implements resource.MessageStore.
func (s *MessageStore) Get(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error) {
	m, err := scanMessage(s.pool.QueryRow(ctx, s.sql.get, userID, convID, msgID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Message{}, domain.ErrNotFound
	case err != nil:
		return domain.Message{}, fmt.Errorf("getting message: %w", err)
	}
	return m, nil
}

// Claim implements resource.MessageStore. The UPDATE's WHERE clause is the
// compare-and-set: of several concurrent callers only one still sees "pending".
//
// Pattern: Compare-and-Set Claim
func (s *MessageStore) Claim(ctx context.Context, userID, convID, msgID uuid.UUID) (resource.Claimed, error) {
	var prompt string
	reply, err := scanMessage(s.pool.QueryRow(ctx, s.sql.claim, userID, convID, msgID), &prompt)
	if err == nil {
		return resource.Claimed{Reply: reply, Prompt: prompt}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return resource.Claimed{}, fmt.Errorf("claiming reply: %w", err)
	}

	// Nothing was updated: say why.
	current, getErr := s.Get(ctx, userID, convID, msgID)
	switch {
	case getErr != nil:
		return resource.Claimed{}, getErr
	case current.Role == domain.RoleAssistant && current.Status == domain.StatusStreaming:
		return resource.Claimed{}, domain.ErrAlreadyStreaming
	}
	return resource.Claimed{}, fmt.Errorf("a %s %s cannot be claimed: %w", current.Status, current.Role, domain.ErrInvalidState)
}

// Finish implements resource.MessageStore.
//
// Pattern: Zero-Row Diagnosis
func (s *MessageStore) Finish(ctx context.Context, msgID uuid.UUID, out resource.Outcome) error {
	if !out.Status.Terminal() {
		return fmt.Errorf("a reply cannot finish as %q: %w", out.Status, domain.ErrInvalidState)
	}
	meta, err := json.Marshal(out.Meta)
	if err != nil {
		return fmt.Errorf("encoding message metadata: %w", err)
	}
	tag, err := s.pool.Exec(ctx, s.sql.finish, msgID, out.Content, out.Status, out.Error, string(meta))
	if err != nil {
		return fmt.Errorf("finishing reply: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, s.sql.exists, msgID).Scan(&exists); err != nil {
		return fmt.Errorf("finishing reply: %w", err)
	}
	if !exists {
		return domain.ErrNotFound
	}
	return fmt.Errorf("only a streaming reply can finish: %w", domain.ErrInvalidState)
}

// ReplaceLastAssistant implements resource.MessageStore.
func (s *MessageStore) ReplaceLastAssistant(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error) {
	var fresh domain.Message
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.lockConversation(ctx, tx, userID, convID); err != nil {
			return err
		}
		last, found, err := s.latestMessage(ctx, tx, convID)
		if err != nil {
			return err
		}
		if !found || last.id != msgID {
			var exists bool
			err := tx.QueryRow(ctx, s.sql.existsInConversation, msgID, convID).Scan(&exists)
			if err != nil {
				return err
			}
			if !exists {
				return domain.ErrNotFound
			}
			return fmt.Errorf("only the last reply can be replaced: %w", domain.ErrInvalidState)
		}
		if last.role != domain.RoleAssistant || !last.status.Terminal() {
			return fmt.Errorf("there is no finished reply to replace: %w", domain.ErrInvalidState)
		}
		if _, err := tx.Exec(ctx, s.sql.remove, last.id); err != nil {
			return err
		}
		if fresh, err = s.insertMessage(ctx, tx, convID, last.seq, domain.RoleAssistant, "", domain.StatusPending); err != nil {
			return err
		}
		return s.touchConversation(ctx, tx, convID)
	})
	if err != nil {
		return domain.Message{}, wrapStore("replacing reply", err)
	}
	return fresh, nil
}

// RecoverInterrupted implements resource.MessageStore.
//
// Pattern: Recover at Startup
func (s *MessageStore) RecoverInterrupted(ctx context.Context, olderThan time.Duration) (int, error) {
	tag, err := s.pool.Exec(ctx, s.sql.recoverInterrupted, olderThan.Seconds(), domain.FailureInterrupted)
	if err != nil {
		return 0, fmt.Errorf("recovering interrupted replies: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// wrapStore adds context to unexpected errors and leaves domain errors alone,
// so callers can still match them with errors.Is.
func wrapStore(what string, err error) error {
	switch {
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrInvalidState):
		return err
	}
	return fmt.Errorf("%s: %w", what, err)
}
