package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service/mocks"
)

type chatHarness struct {
	t     *testing.T
	auth  *mocks.Auth
	chat  *mocks.Chat
	convs *mocks.Conversation
	app   *App
	route http.Handler
}

func newChatHarness(t *testing.T) *chatHarness {
	t.Helper()
	return newChatHarnessWithOptions(t, Options{})
}

func newChatHarnessWithOptions(t *testing.T, opts Options) *chatHarness {
	t.Helper()
	h := &chatHarness{t: t, auth: mocks.NewAuth(t), chat: mocks.NewChat(t), convs: mocks.NewConversation(t)}
	h.auth.On("Authenticate", mock.Anything, "tok").Return(alice, nil)
	h.app = testAppWith(t, Services{Auth: h.auth, Chat: h.chat, Conversations: h.convs}, opts)
	h.route = h.app.Routes()
	return h
}

func (h *chatHarness) do(method, path string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	var req *http.Request
	if form != nil {
		req = postForm(path, form, headers)
		req.Method = method
	} else {
		req = httptest.NewRequest(method, path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})
	rec := httptest.NewRecorder()
	h.route.ServeHTTP(rec, req)
	return rec
}

func (h *chatHarness) expectSidebar(convs ...domain.Conversation) {
	h.convs.On("List", mock.Anything, alice.ID).Return(convs, nil)
}

var (
	convID = uuid.NewV7()
	htmx   = map[string]string{"HX-Request": "true"}
)

func conv(id uuid.UUID, title string) domain.Conversation {
	return domain.Conversation{ID: id, UserID: alice.ID, Title: title}
}

func msg(seq int, role domain.Role, status domain.Status, content string) domain.Message {
	return domain.Message{ID: uuid.NewV7(), ConversationID: convID, Seq: seq, Role: role, Status: status, Content: content}
}

func TestNewChatPage(t *testing.T) {
	h := newChatHarness(t)
	h.expectSidebar(conv(convID, "Earlier chat"), conv(uuid.NewV7(), "<b>Another</b>"))

	rec := h.do(http.MethodGet, "/", nil, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	root := parseHTML(t, rec.Body.String())

	t.Run("offers the composer and a way to start over", func(t *testing.T) {
		assert.Len(t, byTestID(root, "new-chat"), 1)
		input := byTestID(root, "composer-input")
		require.Len(t, input, 1)
		assert.Equal(t, "content", attrOf(input[0], "name"))
		assert.False(t, hasAttr(input[0], "maxlength"),
			"a long paste is not cut off without a word: the server says what the limit is, and the text stays")
		assert.Len(t, byTestID(root, "send"), 1)
		assert.Equal(t, "Send message", attrOf(byTestID(root, "send")[0], "aria-label"))
		assert.Equal(t, "Stop generating", attrOf(byTestID(root, "stop")[0], "aria-label"))
		assert.True(t, hasAttr(byTestID(root, "stop")[0], "hidden"), "stop is shown only while streaming")
		form := elementsOf(root, "form")
		var composer *struct{ action string }
		for _, f := range form {
			if attrOf(f, "id") == "composer" {
				composer = &struct{ action string }{attrOf(f, "action")}
			}
		}
		require.NotNil(t, composer)
		assert.Equal(t, "/c", composer.action, "the first message creates the conversation")
	})
	t.Run("the input has a label", func(t *testing.T) {
		assert.Contains(t, rec.Body.String(), `<label for="composer-input" class="visually-hidden">Message</label>`)
	})
	t.Run("shows the empty state and no messages", func(t *testing.T) {
		assert.Empty(t, byTestID(root, "message"))
		assert.Contains(t, rec.Body.String(), "How can I help?")
	})
	t.Run("lists conversations with titles escaped, none active", func(t *testing.T) {
		items := byTestID(root, "conversation")
		require.Len(t, items, 2)
		links := byTestID(root, "conversation-link")
		require.Len(t, links, 2)
		assert.Equal(t, "/c/"+convID.String(), attrOf(links[0], "href"))
		assert.Equal(t, convID.String(), attrOf(items[0], "data-id"), "the row carries the id")
		assert.Equal(t, "<b>Another</b>", textContent(links[1]), "shown as text, not parsed as HTML")
		assert.Empty(t, elementsOf(root, "b"))
		for _, link := range links {
			assert.False(t, hasAttr(link, "aria-current"))
		}
	})
}

func TestNewChatPage_EmptySidebar(t *testing.T) {
	h := newChatHarness(t)
	h.expectSidebar()

	rec := h.do(http.MethodGet, "/", nil, nil)

	assert.Contains(t, rec.Body.String(), "No conversations yet.")
}

func TestConversationPage(t *testing.T) {
	user := msg(1, domain.RoleUser, domain.StatusComplete, "What is **2+2**?\n<script>alert(1)</script>")
	done := msg(2, domain.RoleAssistant, domain.StatusComplete, "It is **4**.")
	done.Meta = domain.Meta{TTFTMs: 120, DurationMs: 3400, Usage: domain.Usage{Prompt: 10, Completion: 25}, ToolCalls: []string{"search", "calc"}}
	user2 := msg(3, domain.RoleUser, domain.StatusComplete, "again")
	pending := msg(4, domain.RoleAssistant, domain.StatusPending, "")

	h := newChatHarness(t)
	h.convs.On("Get", mock.Anything, alice.ID, convID).Return(service.ConversationView{
		Conversation: conv(convID, "Math"),
		Messages:     []domain.Message{user, done, user2, pending},
	}, nil)
	h.expectSidebar(conv(uuid.NewV7(), "Other"), conv(convID, "Math"))

	rec := h.do(http.MethodGet, "/c/"+convID.String(), nil, nil)

	require.Equal(t, http.StatusOK, rec.Code)
	root := parseHTML(t, rec.Body.String())
	messages := byTestID(root, "message")
	require.Len(t, messages, 4)

	t.Run("the page is titled after the conversation", func(t *testing.T) {
		assert.Contains(t, rec.Body.String(), "<title>Math · Agent Web UI</title>")
	})

	t.Run("the composer posts to this conversation, for htmx and for plain forms", func(t *testing.T) {
		var composer = func() (action, hxPost string) {
			for _, f := range elementsOf(root, "form") {
				if attrOf(f, "id") == "composer" {
					return attrOf(f, "action"), attrOf(f, "hx-post")
				}
			}
			return "", ""
		}
		action, hxPost := composer()
		assert.Equal(t, "/c/"+convID.String()+"/messages", action)
		assert.Equal(t, action, hxPost)
	})

	t.Run("user messages are plain text, never markup", func(t *testing.T) {
		m := messages[0]
		assert.Equal(t, "user", attrOf(m, "data-role"))
		assert.Equal(t, user.ID.String(), attrOf(m, "data-id"))
		assert.Equal(t, "complete", attrOf(m, "data-status"))
		body := byTestID(m, "message-body")
		require.Len(t, body, 1)
		assert.Equal(t, "What is **2+2**?\n<script>alert(1)</script>", textContent(body[0]))
		assert.Empty(t, elementsOf(m, "script"))
		assert.Empty(t, elementsOf(m, "strong"), "asterisks in a user's message stay asterisks")
	})

	t.Run("a finished reply is rendered Markdown and carries its metadata", func(t *testing.T) {
		m := messages[1]
		assert.Equal(t, "assistant", attrOf(m, "data-role"))
		assert.Equal(t, "complete", attrOf(m, "data-status"))
		assert.Len(t, elementsOf(m, "strong"), 1)
		assert.Equal(t, "120", attrOf(m, "data-ttft-ms"))
		assert.Equal(t, "3400", attrOf(m, "data-duration-ms"))
		assert.Equal(t, "10", attrOf(m, "data-prompt-tokens"))
		assert.Equal(t, "25", attrOf(m, "data-completion-tokens"))
		assert.Equal(t, "search,calc", attrOf(m, "data-tool-calls"))
		assert.False(t, strings.Contains(textContent(m.Parent), "sse-connect"))
		assert.False(t, hasAttr(m.Parent, "sse-connect"), "a finished reply opens no stream")
	})

	t.Run("an unfinished reply opens its stream from a stable wrapper", func(t *testing.T) {
		m := messages[3]
		assert.Equal(t, "pending", attrOf(m, "data-status"))
		wrapper := m.Parent
		assert.Equal(t, "reply", attrOf(wrapper, "class"))
		assert.Equal(t, "/c/"+convID.String()+"/m/"+pending.ID.String()+"/stream", attrOf(wrapper, "sse-connect"))
		assert.Equal(t, "delta", attrOf(wrapper, "sse-swap"))
		assert.Equal(t, "done", attrOf(wrapper, "sse-close"))
		assert.False(t, hasAttr(m, "data-ttft-ms"), "unknown values are absent, not zero")
		assert.False(t, hasAttr(m, "data-tool-calls"))
	})

	t.Run("the sidebar marks the open conversation", func(t *testing.T) {
		items := byTestID(root, "conversation")
		links := byTestID(root, "conversation-link")
		require.Len(t, items, 2)
		require.Len(t, links, 2)
		assert.False(t, hasAttr(links[0], "aria-current"))
		assert.Equal(t, "page", attrOf(links[1], "aria-current"))
		assert.Contains(t, attrOf(items[1], "class"), "active")
		assert.NotContains(t, attrOf(items[0], "class"), "active")
	})
}

// Showing a conversation renders the Markdown of every reply in it. Replies that
// are finished do not change, so their HTML is kept; a reply still being written
// is different on every view and is not.
func TestConversationPage_KeepsTheHTMLOfFinishedRepliesOnly(t *testing.T) {
	h := newChatHarness(t)
	h.convs.On("Get", mock.Anything, alice.ID, convID).Return(service.ConversationView{
		Conversation: conv(convID, "c"),
		Messages: []domain.Message{
			msg(1, domain.RoleUser, domain.StatusComplete, "q"),
			msg(2, domain.RoleAssistant, domain.StatusComplete, "It is **4**."),
			msg(3, domain.RoleUser, domain.StatusComplete, "and?"),
			msg(4, domain.RoleAssistant, domain.StatusStreaming, "still comi"),
		},
	}, nil)
	h.expectSidebar()

	first := h.do(http.MethodGet, "/c/"+convID.String(), nil, nil)
	assert.Equal(t, 1, h.app.md.Cached(), "the finished reply is kept, the one being written is not")
	second := h.do(http.MethodGet, "/c/"+convID.String(), nil, nil)

	assert.Equal(t, 1, h.app.md.Cached())
	assert.Equal(t, first.Body.String(), second.Body.String(), "and the page is the same from the kept HTML")
	assert.Contains(t, second.Body.String(), "<strong>4</strong>")
}

func TestConversationPage_ReplyStates(t *testing.T) {
	tests := []struct {
		name       string
		status     domain.Status
		content    string
		errText    string
		wantStream bool
		wantNote   bool
		wantError  string
		wantBody   string
	}{
		{"pending", domain.StatusPending, "", "", true, false, "", ""},
		{"streaming", domain.StatusStreaming, "partial text", "", true, false, "", "partial text"},
		{"stopped keeps its text and says so", domain.StatusStopped, "half an answer", "", false, true, "", "half an answer"},
		{"failed shows why, with any partial text", domain.StatusFailed, "par", "The agent took too long to answer.", false, false, "The agent took too long to answer.", "par"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reply := msg(2, domain.RoleAssistant, tc.status, tc.content)
			reply.Error = tc.errText
			h := newChatHarness(t)
			h.convs.On("Get", mock.Anything, alice.ID, convID).Return(service.ConversationView{
				Conversation: conv(convID, "c"), Messages: []domain.Message{msg(1, domain.RoleUser, domain.StatusComplete, "q"), reply},
			}, nil)
			h.expectSidebar()

			rec := h.do(http.MethodGet, "/c/"+convID.String(), nil, nil)

			root := parseHTML(t, rec.Body.String())
			m := byTestID(root, "message")[1]
			assert.Equal(t, string(tc.status), attrOf(m, "data-status"))
			assert.Equal(t, tc.wantStream, hasAttr(m.Parent, "sse-connect"))
			assert.Equal(t, tc.wantNote, len(byTestID(m, "stopped-note")) == 1)
			assert.Contains(t, textContent(byTestID(m, "message-body")[0]), tc.wantBody)
			errs := byTestID(m, "error")
			if tc.wantError == "" {
				assert.Empty(t, errs)
			} else {
				require.Len(t, errs, 1)
				assert.Equal(t, tc.wantError, textContent(errs[0]))
				assert.Equal(t, "alert", attrOf(errs[0], "role"))
			}
		})
	}
}

func TestConversationPage_HostileReplyIsRenderedSafely(t *testing.T) {
	reply := msg(2, domain.RoleAssistant, domain.StatusComplete,
		"<script>alert(1)</script>\n\n[x](javascript:alert(1))\n\n![t](https://evil.example/p.png)\n\n<img src=x onerror=alert(1)>")
	h := newChatHarness(t)
	h.convs.On("Get", mock.Anything, alice.ID, convID).Return(service.ConversationView{
		Conversation: conv(convID, "c"), Messages: []domain.Message{msg(1, domain.RoleUser, domain.StatusComplete, "q"), reply},
	}, nil)
	h.expectSidebar()

	rec := h.do(http.MethodGet, "/c/"+convID.String(), nil, nil)

	root := parseHTML(t, rec.Body.String())
	body := byTestID(byTestID(root, "message")[1], "message-body")[0]
	for _, tag := range []string{"script", "img", "iframe", "style"} {
		assert.Empty(t, elementsOf(body, tag), tag)
	}
	for _, a := range elementsOf(body, "a") {
		assert.NotContains(t, strings.ToLower(attrOf(a, "href")), "javascript:")
	}
}

func TestConversationPage_Refusals(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		getErr     error
		wantStatus int
	}{
		{"someone else's conversation looks like a missing one", "/c/" + convID.String(), domain.ErrNotFound, http.StatusNotFound},
		{"not a UUID", "/c/not-a-uuid", nil, http.StatusNotFound},
		{"a service failure", "/c/" + convID.String(), errors.New("db down"), http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatHarness(t)
			if tc.getErr != nil {
				h.convs.On("Get", mock.Anything, alice.ID, convID).Return(service.ConversationView{}, tc.getErr)
			}

			rec := h.do(http.MethodGet, tc.path, nil, nil)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.NotContains(t, rec.Body.String(), "db down")
		})
	}
}

func TestStartConversation(t *testing.T) {
	turn := service.Turn{Conversation: conv(convID, "Hello")}

	t.Run("goes to the new conversation, where the reply starts streaming", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("StartConversation", mock.Anything, alice.ID, "hello").Return(turn, nil)

		rec := h.do(http.MethodPost, "/c", url.Values{"content": {"hello"}}, nil)

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, "/c/"+convID.String(), rec.Header().Get("Location"))
	})

	t.Run("htmx is redirected with HX-Redirect", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("StartConversation", mock.Anything, alice.ID, "hello").Return(turn, nil)

		rec := h.do(http.MethodPost, "/c", url.Values{"content": {"hello"}}, htmx)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "/c/"+convID.String(), rec.Header().Get("HX-Redirect"))
	})

	t.Run("a boosted form post is redirected in place, so the document is not reloaded", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("StartConversation", mock.Anything, alice.ID, "hello").Return(turn, nil)

		rec := h.do(http.MethodPost, "/c", url.Values{"content": {"hello"}}, map[string]string{"HX-Request": "true", "HX-Boosted": "true"})

		assert.Equal(t, http.StatusSeeOther, rec.Code, "htmx follows it inside the XHR and swaps the page in")
		assert.Equal(t, "/c/"+convID.String(), rec.Header().Get("Location"))
		assert.Empty(t, rec.Header().Get("HX-Redirect"), "HX-Redirect would reload the document")
	})

	t.Run("a problem found while htmx sends the form only updates the composer's line", func(t *testing.T) {
		for name, headers := range map[string]map[string]string{
			"htmx":    htmx,
			"boosted": {"HX-Request": "true", "HX-Boosted": "true"},
		} {
			t.Run(name, func(t *testing.T) {
				h := newChatHarness(t)
				h.chat.On("StartConversation", mock.Anything, alice.ID, "  ").Return(service.Turn{}, domain.ErrEmptyMessage)

				rec := h.do(http.MethodPost, "/c", url.Values{"content": {"  "}}, headers)

				assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
				assert.Equal(t, "none", rec.Header().Get("HX-Reswap"), "the page on screen, and the text in the box, stay")
				assert.Equal(t, "false", rec.Header().Get("HX-Push-Url"), "and the address stays too: /c only accepts POST")
				line := byTestID(parseHTML(t, rec.Body.String()), "composer-error")
				require.Len(t, line, 1)
				assert.Equal(t, domain.ErrEmptyMessage.Message, textContent(line[0]))
			})
		}
	})

	t.Run("a problem with the text shows the page again with the text kept", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("StartConversation", mock.Anything, alice.ID, "  ").Return(service.Turn{}, domain.ErrEmptyMessage)
		h.expectSidebar()

		rec := h.do(http.MethodPost, "/c", url.Values{"content": {"  "}}, nil)

		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		root := parseHTML(t, rec.Body.String())
		assert.Equal(t, domain.ErrEmptyMessage.Message, textContent(byTestID(root, "composer-error")[0]))
		assert.Equal(t, "  ", textContent(byTestID(root, "composer-input")[0]))
	})

	t.Run("a hostile draft is escaped when shown again", func(t *testing.T) {
		h := newChatHarness(t)
		draft := "</textarea><script>alert(1)</script>"
		h.chat.On("StartConversation", mock.Anything, alice.ID, draft).Return(service.Turn{}, domain.TooLong(10))
		h.expectSidebar()

		rec := h.do(http.MethodPost, "/c", url.Values{"content": {draft}}, nil)

		root := parseHTML(t, rec.Body.String())
		for _, script := range elementsOf(root, "script") {
			assert.True(t, hasAttr(script, "src"), "only the page's own external scripts, never an inline one")
		}
		assert.Equal(t, draft, textContent(byTestID(root, "composer-input")[0]), "kept as text inside the textarea")
	})

	t.Run("a service failure is a 500 without detail", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("StartConversation", mock.Anything, alice.ID, "hi").Return(service.Turn{}, errors.New("secret detail"))

		rec := h.do(http.MethodPost, "/c", url.Values{"content": {"hi"}}, nil)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), "secret detail")
	})
}

func TestSendMessage(t *testing.T) {
	path := "/c/" + convID.String() + "/messages"
	user := msg(3, domain.RoleUser, domain.StatusComplete, "and now?")
	reply := msg(4, domain.RoleAssistant, domain.StatusPending, "")
	turn := service.Turn{Conversation: conv(convID, "c"), User: user, Reply: reply}

	t.Run("htmx gets the two new bubbles, ready to stream, and a cleared message line", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Send", mock.Anything, alice.ID, convID, "and now?").Return(turn, nil)

		rec := h.do(http.MethodPost, path, url.Values{"content": {"and now?"}}, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
		root := parseHTML(t, rec.Body.String())
		messages := byTestID(root, "message")
		require.Len(t, messages, 2)
		assert.Equal(t, "user", attrOf(messages[0], "data-role"))
		assert.Equal(t, "and now?", textContent(byTestID(messages[0], "message-body")[0]))
		assert.Equal(t, "assistant", attrOf(messages[1], "data-role"))
		assert.Equal(t, "/c/"+convID.String()+"/m/"+reply.ID.String()+"/stream", attrOf(messages[1].Parent, "sse-connect"))
		clear := byTestID(root, "composer-error")
		require.Len(t, clear, 1)
		assert.Equal(t, "true", attrOf(clear[0], "hx-swap-oob"))
		assert.Empty(t, strings.TrimSpace(textContent(clear[0])))
	})

	t.Run("a plain form post goes back to the conversation", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Send", mock.Anything, alice.ID, convID, "and now?").Return(turn, nil)

		rec := h.do(http.MethodPost, path, url.Values{"content": {"and now?"}}, nil)

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, "/c/"+convID.String(), rec.Header().Get("Location"))
	})

	problems := []struct {
		name       string
		err        error
		wantStatus int
		wantText   string
	}{
		{"empty message", domain.ErrEmptyMessage, http.StatusUnprocessableEntity, domain.ErrEmptyMessage.Message},
		{"too long", domain.TooLong(8000), http.StatusUnprocessableEntity, "8000"},
		{"a reply is still being written", domain.ErrInvalidState, http.StatusConflict, "Wait for the current reply to finish."},
	}
	for _, tc := range problems {
		t.Run("htmx: "+tc.name+" updates only the message line", func(t *testing.T) {
			h := newChatHarness(t)
			h.chat.On("Send", mock.Anything, alice.ID, convID, "x").Return(service.Turn{}, tc.err)

			rec := h.do(http.MethodPost, path, url.Values{"content": {"x"}}, htmx)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, "none", rec.Header().Get("HX-Reswap"), "nothing is appended to the conversation")
			root := parseHTML(t, rec.Body.String())
			line := byTestID(root, "composer-error")
			require.Len(t, line, 1)
			assert.Contains(t, textContent(line[0]), tc.wantText)
			assert.Empty(t, byTestID(root, "message"))
		})

		t.Run("plain form: "+tc.name+" shows the page again with the text kept", func(t *testing.T) {
			h := newChatHarness(t)
			h.chat.On("Send", mock.Anything, alice.ID, convID, "my draft").Return(service.Turn{}, tc.err)
			h.convs.On("Get", mock.Anything, alice.ID, convID).Return(service.ConversationView{Conversation: conv(convID, "c")}, nil)
			h.expectSidebar()

			rec := h.do(http.MethodPost, path, url.Values{"content": {"my draft"}}, nil)

			assert.Equal(t, tc.wantStatus, rec.Code)
			root := parseHTML(t, rec.Body.String())
			assert.Contains(t, textContent(byTestID(root, "composer-error")[0]), tc.wantText)
			assert.Equal(t, "my draft", textContent(byTestID(root, "composer-input")[0]))
		})
	}

	t.Run("someone else's conversation is not found", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Send", mock.Anything, alice.ID, convID, "x").Return(service.Turn{}, domain.ErrNotFound)

		rec := h.do(http.MethodPost, path, url.Values{"content": {"x"}}, htmx)

		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("a bad conversation id is not found", func(t *testing.T) {
		rec := newChatHarness(t).do(http.MethodPost, "/c/nope/messages", url.Values{"content": {"x"}}, htmx)

		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("a service failure is a 500 without detail", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Send", mock.Anything, alice.ID, convID, "x").Return(service.Turn{}, errors.New("secret detail"))

		rec := h.do(http.MethodPost, path, url.Values{"content": {"x"}}, htmx)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), "secret detail")
	})
}

func TestComposerForm(t *testing.T) {
	formOf := func(t *testing.T, root *html.Node) *html.Node {
		t.Helper()
		for _, f := range elementsOf(root, "form") {
			if attrOf(f, "id") == "composer" {
				return f
			}
		}
		t.Fatal("no composer form")
		return nil
	}

	t.Run("on the start page it is boosted, so the first message keeps the document", func(t *testing.T) {
		h := newChatHarness(t)
		h.expectSidebar()

		rec := h.do(http.MethodGet, "/", nil, nil)

		form := formOf(t, parseHTML(t, rec.Body.String()))
		assert.Equal(t, "/c", attrOf(form, "action"))
		assert.Equal(t, "true", attrOf(form, "hx-boost"))
		assert.False(t, hasAttr(form, "hx-post"))
	})

	t.Run("in a conversation it posts to the conversation and is not boosted", func(t *testing.T) {
		h := newChatHarness(t)
		h.convs.On("Get", mock.Anything, alice.ID, convID).Return(service.ConversationView{Conversation: conv(convID, "c")}, nil)
		h.expectSidebar()

		rec := h.do(http.MethodGet, "/c/"+convID.String(), nil, nil)

		form := formOf(t, parseHTML(t, rec.Body.String()))
		assert.Equal(t, "/c/"+convID.String()+"/messages", attrOf(form, "hx-post"))
		assert.False(t, hasAttr(form, "hx-boost"))
	})
}
