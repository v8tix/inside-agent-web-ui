package chat_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/chat"
	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource/mocks"
)

const maxChars = 10

type harness struct {
	convs *mocks.ConversationStore
	msgs  *mocks.MessageStore
	agent *mocks.AgentGateway
	logs  *bytes.Buffer
	svc   *chat.Service
}

func newHarness(t *testing.T, limit ...int) *harness {
	t.Helper()
	return newHarnessWithReplyLimit(t, 0, limit...)
}

// newHarnessWithReplyLimit is newHarness with a limit on how big a reply may
// grow, in bytes. Zero is the default.
func newHarnessWithReplyLimit(t *testing.T, maxReply int, limit ...int) *harness {
	t.Helper()
	maxLen := maxChars
	if len(limit) > 0 {
		maxLen = limit[0]
	}
	h := &harness{
		convs: mocks.NewConversationStore(t),
		msgs:  mocks.NewMessageStore(t),
		agent: mocks.NewAgentGateway(t),
		logs:  &bytes.Buffer{},
	}
	h.svc = chat.NewService(chat.Dependencies{
		Conversations:   h.convs,
		Messages:        h.msgs,
		Agent:           h.agent,
		Logger:          slog.New(slog.NewTextHandler(h.logs, nil)),
		MaxMessageChars: maxLen,
		MaxReplyBytes:   maxReply,
	})
	return h
}

var (
	userID = uuid.NewV7()
	convID = uuid.NewV7()
)

func conversation(title string) domain.Conversation {
	return domain.Conversation{ID: convID, UserID: userID, Title: title}
}

func pair(content string) (domain.Message, domain.Message) {
	user := domain.Message{ID: uuid.NewV7(), ConversationID: convID, Seq: 1, Role: domain.RoleUser, Content: content, Status: domain.StatusComplete}
	reply := domain.Message{ID: uuid.NewV7(), ConversationID: convID, Seq: 2, Role: domain.RoleAssistant, Status: domain.StatusPending}
	return user, reply
}

func TestSend(t *testing.T) {
	t.Run("sends the trimmed message and returns the turn", func(t *testing.T) {
		h := newHarness(t)
		user, reply := pair("hello")
		h.convs.On("Get", mock.Anything, userID, convID).Return(conversation("chat"), nil)
		h.msgs.On("AppendPair", mock.Anything, userID, convID, "hello").Return(user, reply, nil)

		turn, err := h.svc.Send(t.Context(), userID, convID, "  hello \n")

		require.NoError(t, err)
		assert.Equal(t, conversation("chat"), turn.Conversation)
		assert.Equal(t, user, turn.User)
		assert.Equal(t, reply, turn.Reply)
	})

	tests := []struct {
		name    string
		content string
		wantErr error
	}{
		{"empty", "", domain.ErrEmptyMessage},
		{"whitespace only", " \n\t", domain.ErrEmptyMessage},
		{"one character over the limit", "01234567890", domain.ErrMessageTooLong},
		{"multibyte characters over the limit", strings.Repeat("é", maxChars+1), domain.ErrMessageTooLong},
	}
	for _, tc := range tests {
		t.Run("rejects "+tc.name+" without touching the stores", func(t *testing.T) {
			h := newHarness(t) // mocks fail the test on any unexpected call

			_, err := h.svc.Send(t.Context(), userID, convID, tc.content)

			require.ErrorIs(t, err, tc.wantErr)
		})
	}

	t.Run("accepts exactly the limit, counted in characters", func(t *testing.T) {
		h := newHarness(t)
		content := strings.Repeat("é", maxChars)
		user, reply := pair(content)
		h.convs.On("Get", mock.Anything, userID, convID).Return(conversation("chat"), nil)
		h.msgs.On("AppendPair", mock.Anything, userID, convID, content).Return(user, reply, nil)

		_, err := h.svc.Send(t.Context(), userID, convID, content)

		require.NoError(t, err)
	})

	t.Run("passes store errors through", func(t *testing.T) {
		tests := []struct {
			name      string
			getErr    error
			appendErr error
			want      error
		}{
			{"unknown conversation", domain.ErrNotFound, nil, domain.ErrNotFound},
			{"a turn already in flight", nil, domain.ErrInvalidState, domain.ErrInvalidState},
			{"a database failure", nil, errors.New("connection reset"), nil},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				h := newHarness(t)
				if tc.getErr != nil {
					h.convs.On("Get", mock.Anything, userID, convID).Return(domain.Conversation{}, tc.getErr)
				} else {
					h.convs.On("Get", mock.Anything, userID, convID).Return(conversation("chat"), nil)
					h.msgs.On("AppendPair", mock.Anything, userID, convID, "hi").
						Return(domain.Message{}, domain.Message{}, tc.appendErr)
				}

				_, err := h.svc.Send(t.Context(), userID, convID, "hi")

				require.Error(t, err)
				if tc.want != nil {
					assert.ErrorIs(t, err, tc.want)
				}
			})
		}
	})
}

func TestStartConversation(t *testing.T) {
	t.Run("names the conversation after the first message", func(t *testing.T) {
		tests := []struct {
			name      string
			content   string
			wantTitle string
		}{
			{"short", "hello", "hello"},
			{"newlines collapse", "a\nb", "a b"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				h := newHarness(t)
				user, reply := pair(tc.content)
				h.convs.On("Create", mock.Anything, mock.MatchedBy(func(c domain.Conversation) bool {
					return c.UserID == userID && c.Title == tc.wantTitle && c.ID != uuid.Nil()
				})).Return(conversation(tc.wantTitle), nil)
				h.msgs.On("AppendPair", mock.Anything, userID, convID, tc.content).Return(user, reply, nil)

				turn, err := h.svc.StartConversation(t.Context(), userID, tc.content)

				require.NoError(t, err)
				assert.Equal(t, tc.wantTitle, turn.Conversation.Title)
				assert.Equal(t, reply.ID, turn.Reply.ID)
			})
		}
	})

	t.Run("a long first message makes a 60-character title with an ellipsis", func(t *testing.T) {
		h := newHarness(t, 500)
		long := strings.Repeat("a", 100)
		wantTitle := strings.Repeat("a", 60) + "…"
		user, reply := pair(long)
		h.convs.On("Create", mock.Anything, mock.MatchedBy(func(c domain.Conversation) bool { return c.Title == wantTitle })).
			Return(conversation(wantTitle), nil)
		h.msgs.On("AppendPair", mock.Anything, userID, convID, long).Return(user, reply, nil)

		_, err := h.svc.StartConversation(t.Context(), userID, long)

		require.NoError(t, err)
	})

	t.Run("validates before creating anything", func(t *testing.T) {
		h := newHarness(t)

		_, err := h.svc.StartConversation(t.Context(), userID, "   ")

		require.ErrorIs(t, err, domain.ErrEmptyMessage)
	})

	t.Run("a failed create stops there", func(t *testing.T) {
		h := newHarness(t)
		h.convs.On("Create", mock.Anything, mock.Anything).Return(domain.Conversation{}, errors.New("disk full"))

		_, err := h.svc.StartConversation(t.Context(), userID, "hi")

		require.Error(t, err)
	})

	t.Run("removes the empty conversation when the first message cannot be saved", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		h.convs.On("Create", mock.Anything, mock.Anything).Return(conversation("hi"), nil)
		h.msgs.On("AppendPair", mock.Anything, userID, convID, "hi").
			Run(func(mock.Arguments) { cancel() }). // the request goes away mid-way
			Return(domain.Message{}, domain.Message{}, context.Canceled)
		h.convs.On("Delete", mock.MatchedBy(func(c context.Context) bool { return c.Err() == nil }), userID, convID).Return(nil)

		_, err := h.svc.StartConversation(ctx, userID, "hi")

		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("a failed cleanup is logged and the original error wins", func(t *testing.T) {
		h := newHarness(t)
		appendErr := errors.New("append failed")
		h.convs.On("Create", mock.Anything, mock.Anything).Return(conversation("hi"), nil)
		h.msgs.On("AppendPair", mock.Anything, userID, convID, "hi").Return(domain.Message{}, domain.Message{}, appendErr)
		h.convs.On("Delete", mock.Anything, userID, convID).Return(errors.New("delete failed"))

		_, err := h.svc.StartConversation(t.Context(), userID, "hi")

		require.ErrorIs(t, err, appendErr)
		assert.Contains(t, h.logs.String(), "delete failed")
	})
}

func TestRegenerate(t *testing.T) {
	tests := []struct {
		name    string
		storeEr error
	}{
		{"replaces a finished reply", nil},
		{"a reply still in flight, or not the last one", domain.ErrInvalidState},
		{"someone else's conversation, or a message that is not there", domain.ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			_, reply := pair("q")
			h.msgs.On("ReplaceLastAssistant", mock.Anything, userID, convID, reply.ID).Return(reply, tc.storeEr)

			got, err := h.svc.Regenerate(t.Context(), userID, convID, reply.ID)

			if tc.storeEr != nil {
				require.ErrorIs(t, err, tc.storeEr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, reply, got)
		})
	}
}
