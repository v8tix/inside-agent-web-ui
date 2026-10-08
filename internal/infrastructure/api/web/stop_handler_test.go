package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

func stopPath() string { return "/c/" + convID.String() + "/m/" + streamMsgID.String() + "/stop" }

func reply(status domain.Status, content string) domain.Message {
	return domain.Message{
		ID: streamMsgID, ConversationID: convID, Seq: 2, Role: domain.RoleAssistant,
		Status: status, Content: content,
	}
}

func TestStopMessage(t *testing.T) {
	t.Run("answers with the saved reply, which no longer streams", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Stop", mock.Anything, alice.ID, convID, streamMsgID).Return(reply(domain.StatusStopped, "Half an ans"), nil)

		rec := h.do(http.MethodPost, stopPath(), nil, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")
		root := parseHTML(t, rec.Body.String())
		wrapper := elementsOf(root, "div")
		require.NotEmpty(t, wrapper)
		assert.Equal(t, "reply", attrOf(wrapper[0], "class"))
		assert.False(t, hasAttr(wrapper[0], "sse-connect"), "replacing the wrapper is what closes the stream; the new one must not open another")
		assert.False(t, hasAttr(wrapper[0], "data-stop-url"))
		article := byTestID(root, "message")
		require.Len(t, article, 1)
		assert.Equal(t, "stopped", attrOf(article[0], "data-status"))
		assert.Equal(t, "assistant", attrOf(article[0], "data-role"))
		assert.Equal(t, "Half an ans", strings.TrimSpace(textContent(byTestID(root, "message-body")[0])))
		assert.Len(t, byTestID(root, "stopped-note"), 1)
	})

	t.Run("a reply that is still running elsewhere keeps its stream, which settles it", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Stop", mock.Anything, alice.ID, convID, streamMsgID).Return(reply(domain.StatusStreaming, "so far"), nil)

		rec := h.do(http.MethodPost, stopPath(), nil, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		wrapper := elementsOf(parseHTML(t, rec.Body.String()), "div")[0]
		assert.Equal(t, "/c/"+convID.String()+"/m/"+streamMsgID.String()+"/stream", attrOf(wrapper, "sse-connect"))
		assert.Equal(t, stopPath(), attrOf(wrapper, "data-stop-url"))
	})

	t.Run("without htmx it goes back to the conversation", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Stop", mock.Anything, alice.ID, convID, streamMsgID).Return(reply(domain.StatusStopped, ""), nil)

		rec := h.do(http.MethodPost, stopPath(), nil, nil)

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, "/c/"+convID.String(), rec.Header().Get("Location"))
	})

	failures := []struct {
		name       string
		path       string
		err        error
		callsStop  bool
		wantStatus int
		wantBody   string
	}{
		{"not found or someone else's", stopPath(), domain.ErrNotFound, true, http.StatusNotFound, ""},
		{"not a reply", stopPath(), domain.ErrInvalidState, true, http.StatusNotFound, ""},
		{"a malformed conversation id", "/c/nope/m/" + streamMsgID.String() + "/stop", nil, false, http.StatusNotFound, ""},
		{"a malformed message id", "/c/" + convID.String() + "/m/nope/stop", nil, false, http.StatusNotFound, ""},
		{"the use case fails", stopPath(), errors.New("db password=hunter2 refused"), true, http.StatusInternalServerError, "hunter2"},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatHarness(t)
			if tc.callsStop {
				h.chat.On("Stop", mock.Anything, alice.ID, convID, streamMsgID).Return(domain.Message{}, tc.err)
			}

			rec := h.do(http.MethodPost, tc.path, nil, htmx)

			assert.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantBody != "" {
				assert.NotContains(t, rec.Body.String(), tc.wantBody, "errors never show internals")
			}
		})
	}

	t.Run("only POST stops a reply", func(t *testing.T) {
		h := newChatHarness(t)

		rec := h.do(http.MethodGet, stopPath(), nil, nil)

		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, "a link or an image tag must not stop anything")
	})
}
