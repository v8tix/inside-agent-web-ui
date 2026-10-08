package conversation_test

import (
	"errors"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/conversation"
	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource/mocks"
)

const listLimit = 25

var (
	userID = uuid.NewV7()
	convID = uuid.NewV7()
)

func newService(t *testing.T) (*conversation.Service, *mocks.ConversationStore, *mocks.MessageStore) {
	t.Helper()
	convs, msgs := mocks.NewConversationStore(t), mocks.NewMessageStore(t)
	return conversation.NewService(conversation.Dependencies{Conversations: convs, Messages: msgs, ListLimit: listLimit}), convs, msgs
}

func TestList(t *testing.T) {
	svc, convs, _ := newService(t)
	want := []domain.Conversation{{ID: convID, Title: "a"}, {ID: uuid.NewV7(), Title: "b"}}
	convs.On("List", mock.Anything, userID, listLimit).Return(want, nil)

	got, err := svc.List(t.Context(), userID)

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestGet(t *testing.T) {
	conv := domain.Conversation{ID: convID, UserID: userID, Title: "chat"}
	msgs := []domain.Message{{Seq: 1, Role: domain.RoleUser}, {Seq: 2, Role: domain.RoleAssistant}}

	t.Run("returns the conversation with its messages", func(t *testing.T) {
		svc, convStore, msgStore := newService(t)
		convStore.On("Get", mock.Anything, userID, convID).Return(conv, nil)
		msgStore.On("List", mock.Anything, userID, convID).Return(msgs, nil)

		got, err := svc.Get(t.Context(), userID, convID)

		require.NoError(t, err)
		assert.Equal(t, conv, got.Conversation)
		assert.Equal(t, msgs, got.Messages)
	})

	t.Run("a missing or foreign conversation never reaches the message store", func(t *testing.T) {
		svc, convStore, _ := newService(t)
		convStore.On("Get", mock.Anything, userID, convID).Return(domain.Conversation{}, domain.ErrNotFound)

		_, err := svc.Get(t.Context(), userID, convID)

		require.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("a failing message query is an error", func(t *testing.T) {
		svc, convStore, msgStore := newService(t)
		convStore.On("Get", mock.Anything, userID, convID).Return(conv, nil)
		msgStore.On("List", mock.Anything, userID, convID).Return(nil, errors.New("db down"))

		_, err := svc.Get(t.Context(), userID, convID)

		require.Error(t, err)
	})
}

func TestFind(t *testing.T) {
	conv := domain.Conversation{ID: convID, UserID: userID, Title: "chat"}

	t.Run("returns the conversation without reading its messages", func(t *testing.T) {
		svc, convStore, _ := newService(t) // the message store has no expectations: any call fails the test
		convStore.On("Get", mock.Anything, userID, convID).Return(conv, nil)

		got, err := svc.Find(t.Context(), userID, convID)

		require.NoError(t, err)
		assert.Equal(t, conv, got)
	})

	t.Run("a missing or foreign conversation is not found", func(t *testing.T) {
		svc, convStore, _ := newService(t)
		convStore.On("Get", mock.Anything, userID, convID).Return(domain.Conversation{}, domain.ErrNotFound)

		_, err := svc.Find(t.Context(), userID, convID)

		require.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func TestMessage(t *testing.T) {
	msgID := uuid.NewV7()
	want := domain.Message{ID: msgID, ConversationID: convID, Role: domain.RoleAssistant, Content: "**done**"}

	t.Run("returns the message", func(t *testing.T) {
		svc, _, msgStore := newService(t)
		msgStore.On("Get", mock.Anything, userID, convID, msgID).Return(want, nil)

		got, err := svc.Message(t.Context(), userID, convID, msgID)

		require.NoError(t, err)
		assert.Equal(t, want, got)
	})

	t.Run("someone else's or a missing message is not found", func(t *testing.T) {
		svc, _, msgStore := newService(t)
		msgStore.On("Get", mock.Anything, userID, convID, msgID).Return(domain.Message{}, domain.ErrNotFound)

		_, err := svc.Message(t.Context(), userID, convID, msgID)

		require.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func TestRename(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantStore string // title sent to the store; empty means the store is not called
		storeErr  error
		wantErr   error
	}{
		{"trims and collapses whitespace", "  My \n chat ", "My chat", nil, nil},
		{"empty is refused before the store", "  ", "", nil, nil},
		{"too long is refused before the store", strings.Repeat("x", domain.MaxTitleChars+1), "", nil, nil},
		{"a foreign conversation", "Mine", "Mine", domain.ErrNotFound, domain.ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, convs, _ := newService(t)
			if tc.wantStore != "" {
				convs.On("Rename", mock.Anything, userID, convID, tc.wantStore).Return(tc.storeErr)
			}

			got, err := svc.Rename(t.Context(), userID, convID, tc.input)

			switch {
			case tc.wantStore == "":
				var ve *domain.ValidationError
				require.ErrorAs(t, err, &ve)
				assert.Equal(t, "title", ve.Field)
			case tc.wantErr != nil:
				require.ErrorIs(t, err, tc.wantErr)
			default:
				require.NoError(t, err)
				assert.Equal(t, tc.wantStore, got)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	for _, storeErr := range []error{nil, domain.ErrNotFound} {
		t.Run(errName(storeErr), func(t *testing.T) {
			svc, convs, _ := newService(t)
			convs.On("Delete", mock.Anything, userID, convID).Return(storeErr)

			err := svc.Delete(t.Context(), userID, convID)

			if storeErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, storeErr)
		})
	}
}

func errName(err error) string {
	if err == nil {
		return "deletes the conversation"
	}
	return "a foreign conversation is not found"
}
