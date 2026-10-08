package web

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
)

func regeneratePath() string {
	return "/c/" + convID.String() + "/m/" + streamMsgID.String() + "/regenerate"
}

func TestRegenerateMessage(t *testing.T) {
	newReply := domain.Message{
		ID: uuid.NewV7(), ConversationID: convID, Seq: 2, Role: domain.RoleAssistant, Status: domain.StatusPending,
	}

	t.Run("answers with a new reply that streams, to replace the old one", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Regenerate", mock.Anything, alice.ID, convID, streamMsgID).Return(newReply, nil)

		rec := h.do(http.MethodPost, regeneratePath(), nil, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		root := parseHTML(t, rec.Body.String())
		wrapper := elementsOf(root, "div")[0]
		assert.Equal(t, "reply", attrOf(wrapper, "class"))
		assert.Equal(t, "/c/"+convID.String()+"/m/"+newReply.ID.String()+"/stream", attrOf(wrapper, "sse-connect"),
			"the new reply streams, not the old one")
		article := byTestID(root, "message")
		require.Len(t, article, 1)
		assert.Equal(t, newReply.ID.String(), attrOf(article[0], "data-id"))
		assert.Equal(t, "pending", attrOf(article[0], "data-status"))
		assert.Empty(t, byTestID(root, "regenerate"), "nothing to regenerate while it is being written")
	})

	t.Run("without htmx it goes back to the conversation, where the new reply streams", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Regenerate", mock.Anything, alice.ID, convID, streamMsgID).Return(newReply, nil)

		rec := h.do(http.MethodPost, regeneratePath(), nil, nil)

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, "/c/"+convID.String(), rec.Header().Get("Location"))
	})

	t.Run("a reply that is not the latest, or not finished, is a conflict shown by the composer", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Regenerate", mock.Anything, alice.ID, convID, streamMsgID).Return(domain.Message{}, domain.ErrInvalidState)

		rec := h.do(http.MethodPost, regeneratePath(), nil, htmx)

		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Equal(t, "none", rec.Header().Get("HX-Reswap"), "the conversation on screen is left alone")
		line := byTestID(parseHTML(t, rec.Body.String()), "composer-error")
		require.Len(t, line, 1)
		assert.Equal(t, cannotRegenerate, textContent(line[0]))
		assert.Equal(t, "true", attrOf(line[0], "hx-swap-oob"), "it lands in the composer's message line")
	})

	failures := []struct {
		name       string
		path       string
		err        error
		callsChat  bool
		wantStatus int
	}{
		{"not found or someone else's", regeneratePath(), domain.ErrNotFound, true, http.StatusNotFound},
		{"a malformed conversation id", "/c/nope/m/" + streamMsgID.String() + "/regenerate", nil, false, http.StatusNotFound},
		{"a malformed message id", "/c/" + convID.String() + "/m/nope/regenerate", nil, false, http.StatusNotFound},
		{"the use case fails", regeneratePath(), errors.New("db password=hunter2 refused"), true, http.StatusInternalServerError},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatHarness(t)
			if tc.callsChat {
				h.chat.On("Regenerate", mock.Anything, alice.ID, convID, streamMsgID).Return(domain.Message{}, tc.err)
			}

			rec := h.do(http.MethodPost, tc.path, nil, htmx)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.NotContains(t, rec.Body.String(), "hunter2", "errors never show internals")
		})
	}

	t.Run("only POST regenerates", func(t *testing.T) {
		h := newChatHarness(t)

		rec := h.do(http.MethodGet, regeneratePath(), url.Values(nil), nil)

		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})
}

// actionsOn returns the testids of the buttons a rendered conversation offers
// on each message, in order.
func actionsOn(t *testing.T, root *html.Node) [][]string {
	t.Helper()
	var out [][]string
	for _, m := range byTestID(root, "message") {
		var ids []string
		for _, id := range []string{"regenerate", "retry"} {
			for range byTestID(m, id) {
				ids = append(ids, id)
			}
		}
		out = append(out, ids)
	}
	return out
}

func TestConversationPage_MessageActions(t *testing.T) {
	failed := msg(4, domain.RoleAssistant, domain.StatusFailed, "")
	failed.Error = "The agent took too long to answer."
	tests := []struct {
		name string
		msgs []domain.Message
		want [][]string
	}{
		{
			"only the latest finished reply can be regenerated",
			[]domain.Message{
				msg(1, domain.RoleUser, domain.StatusComplete, "q1"), msg(2, domain.RoleAssistant, domain.StatusComplete, "a1"),
				msg(3, domain.RoleUser, domain.StatusComplete, "q2"), msg(4, domain.RoleAssistant, domain.StatusComplete, "a2"),
			},
			[][]string{nil, nil, nil, {"regenerate"}},
		},
		{
			"a stopped reply can be regenerated",
			[]domain.Message{msg(1, domain.RoleUser, domain.StatusComplete, "q"), msg(2, domain.RoleAssistant, domain.StatusStopped, "half")},
			[][]string{nil, {"regenerate"}},
		},
		{
			"a failed reply offers retry, not regenerate",
			[]domain.Message{msg(1, domain.RoleUser, domain.StatusComplete, "q"), msg(2, domain.RoleAssistant, domain.StatusComplete, "a"), msg(3, domain.RoleUser, domain.StatusComplete, "q2"), failed},
			[][]string{nil, nil, nil, {"retry"}},
		},
		{
			"nothing while the reply is being written",
			[]domain.Message{msg(1, domain.RoleUser, domain.StatusComplete, "q"), msg(2, domain.RoleAssistant, domain.StatusStreaming, "so far")},
			[][]string{nil, nil},
		},
		{
			"nothing while it waits to start",
			[]domain.Message{msg(1, domain.RoleUser, domain.StatusComplete, "q"), msg(2, domain.RoleAssistant, domain.StatusPending, "")},
			[][]string{nil, nil},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatHarness(t)
			h.convs.On("Get", mock.Anything, alice.ID, convID).Return(service.ConversationView{
				Conversation: conv(convID, "c"), Messages: tc.msgs,
			}, nil)
			h.expectSidebar()

			rec := h.do(http.MethodGet, "/c/"+convID.String(), nil, nil)

			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tc.want, actionsOn(t, parseHTML(t, rec.Body.String())))
		})
	}

	t.Run("the buttons ask for a new reply in the old one's place", func(t *testing.T) {
		h := newChatHarness(t)
		reply := msg(2, domain.RoleAssistant, domain.StatusComplete, "a")
		h.convs.On("Get", mock.Anything, alice.ID, convID).Return(service.ConversationView{
			Conversation: conv(convID, "c"), Messages: []domain.Message{msg(1, domain.RoleUser, domain.StatusComplete, "q"), reply},
		}, nil)
		h.expectSidebar()

		rec := h.do(http.MethodGet, "/c/"+convID.String(), nil, nil)

		button := byTestID(parseHTML(t, rec.Body.String()), "regenerate")
		require.Len(t, button, 1)
		assert.Equal(t, "/c/"+convID.String()+"/m/"+reply.ID.String()+"/regenerate", attrOf(button[0], "hx-post"))
		assert.Equal(t, "closest .reply", attrOf(button[0], "hx-target"))
		assert.Equal(t, "outerHTML", attrOf(button[0], "hx-swap"))
		assert.Equal(t, "Regenerate response", attrOf(button[0], "aria-label"), "an accessible name that contains the visible text")
		assert.Equal(t, "Regenerate", textContent(button[0]))
	})
}
