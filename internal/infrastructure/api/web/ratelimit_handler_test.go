package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service/mocks"
)

var bob = domain.User{ID: uuid.NewV7(), Email: "bob@example.com"}

// limitedHarness is an app with a tiny bucket that refills far too slowly to
// matter during a test, and two signed-in users.
func limitedHarness(t *testing.T, burst int) *chatHarness {
	t.Helper()
	h := &chatHarness{t: t, auth: mocks.NewAuth(t), chat: mocks.NewChat(t), convs: mocks.NewConversation(t)}
	h.auth.On("Authenticate", mock.Anything, "tok").Return(alice, nil).Maybe()
	h.auth.On("Authenticate", mock.Anything, "tok2").Return(bob, nil).Maybe()
	h.route = testAppWith(t, Services{Auth: h.auth, Chat: h.chat, Conversations: h.convs},
		Options{RateLimitRPS: 0.0001, RateLimitBurst: burst}).Routes()
	return h
}

func (h *chatHarness) as(token, method, path string, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := postForm(path, form, headers)
	req.Method = method
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	h.route.ServeHTTP(rec, req)
	return rec
}

func TestRateLimit_Sends(t *testing.T) {
	turn := service.Turn{
		Conversation: domain.Conversation{ID: convID, UserID: alice.ID, Title: "t"},
		User:         domain.Message{ID: uuid.NewV7(), ConversationID: convID, Role: domain.RoleUser, Status: domain.StatusComplete, Content: "hi"},
		Reply:        domain.Message{ID: uuid.NewV7(), ConversationID: convID, Role: domain.RoleAssistant, Status: domain.StatusPending},
	}
	sendPath := "/c/" + convID.String() + "/messages"

	t.Run("a message over the limit is refused, with the wait and a friendly line, and keeps its text", func(t *testing.T) {
		h := limitedHarness(t, 2)
		h.chat.On("Send", mock.Anything, alice.ID, convID, "hi").Return(turn, nil).Twice()
		for range 2 {
			require.Equal(t, http.StatusOK, h.as("tok", http.MethodPost, sendPath, url.Values{"content": {"hi"}}, htmx).Code)
		}

		rec := h.as("tok", http.MethodPost, sendPath, url.Values{"content": {"hi"}}, htmx)

		assert.Equal(t, http.StatusTooManyRequests, rec.Code)
		assert.NotEmpty(t, rec.Header().Get("Retry-After"))
		assert.Equal(t, "none", rec.Header().Get("HX-Reswap"), "the conversation on screen is left alone")
		line := byTestID(parseHTML(t, rec.Body.String()), "composer-error")
		require.Len(t, line, 1)
		assert.Contains(t, textContent(line[0]), "You're sending messages too quickly. Try again in ")
		h.chat.AssertNumberOfCalls(t, "Send", 2) // the third never reached the use case
	})

	t.Run("a plain form post gets the whole page back with what was typed", func(t *testing.T) {
		h := limitedHarness(t, 1)
		h.chat.On("Send", mock.Anything, alice.ID, convID, "first").Return(turn, nil).Once()
		h.as("tok", http.MethodPost, sendPath, url.Values{"content": {"first"}}, nil)
		h.convs.On("Get", mock.Anything, alice.ID, convID).Return(service.ConversationView{Conversation: conv(convID, "t")}, nil)
		h.convs.On("List", mock.Anything, alice.ID).Return([]domain.Conversation{conv(convID, "t")}, nil)

		rec := h.as("tok", http.MethodPost, sendPath, url.Values{"content": {"second, kept"}}, nil)

		require.Equal(t, http.StatusTooManyRequests, rec.Code)
		root := parseHTML(t, rec.Body.String())
		assert.Equal(t, "second, kept", strings.TrimSpace(textContent(byTestID(root, "composer-input")[0])))
		assert.Contains(t, textContent(byTestID(root, "composer-error")[0]), "too quickly")
		assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	})

	t.Run("a new chat, a message and a regenerate share one bucket", func(t *testing.T) {
		h := limitedHarness(t, 2)
		h.chat.On("StartConversation", mock.Anything, alice.ID, "hi").Return(turn, nil).Once()
		h.chat.On("Regenerate", mock.Anything, alice.ID, convID, streamMsgID).Return(turn.Reply, nil).Once()

		assert.Equal(t, http.StatusSeeOther, h.as("tok", http.MethodPost, "/c", url.Values{"content": {"hi"}}, nil).Code)
		assert.Equal(t, http.StatusOK, h.as("tok", http.MethodPost, regeneratePath(), nil, htmx).Code)

		assert.Equal(t, http.StatusTooManyRequests, h.as("tok", http.MethodPost, sendPath, url.Values{"content": {"hi"}}, htmx).Code)
		assert.Equal(t, http.StatusTooManyRequests, h.as("tok", http.MethodPost, regeneratePath(), nil, htmx).Code)
		h.convs.On("List", mock.Anything, alice.ID).Return(nil, nil).Maybe()
		assert.Equal(t, http.StatusTooManyRequests, h.as("tok", http.MethodPost, "/c", url.Values{"content": {"hi"}}, nil).Code)
	})

	t.Run("each user has their own bucket", func(t *testing.T) {
		h := limitedHarness(t, 1)
		h.chat.On("Send", mock.Anything, alice.ID, convID, "hi").Return(turn, nil).Once()
		h.chat.On("Send", mock.Anything, bob.ID, convID, "hi").Return(turn, nil).Once()

		assert.Equal(t, http.StatusOK, h.as("tok", http.MethodPost, sendPath, url.Values{"content": {"hi"}}, htmx).Code)
		assert.Equal(t, http.StatusTooManyRequests, h.as("tok", http.MethodPost, sendPath, url.Values{"content": {"hi"}}, htmx).Code)
		assert.Equal(t, http.StatusOK, h.as("tok2", http.MethodPost, sendPath, url.Values{"content": {"hi"}}, htmx).Code, "bob is not slowed by alice")
	})

	t.Run("reading, stopping and managing conversations are not limited", func(t *testing.T) {
		h := limitedHarness(t, 1)
		h.chat.On("Send", mock.Anything, alice.ID, convID, "hi").Return(turn, nil).Once()
		h.as("tok", http.MethodPost, sendPath, url.Values{"content": {"hi"}}, htmx) // the bucket is now empty
		require.Equal(t, http.StatusTooManyRequests, h.as("tok", http.MethodPost, sendPath, url.Values{"content": {"hi"}}, htmx).Code)

		h.chat.On("Stop", mock.Anything, alice.ID, convID, streamMsgID).Return(domain.Message{ID: streamMsgID, ConversationID: convID, Role: domain.RoleAssistant, Status: domain.StatusStopped}, nil)
		h.convs.On("Find", mock.Anything, alice.ID, convID).Return(conv(convID, "t"), nil)
		h.convs.On("Rename", mock.Anything, alice.ID, convID, "new").Return("new", nil)
		h.convs.On("Delete", mock.Anything, alice.ID, convID).Return(nil)

		for _, tc := range []struct{ name, method, path string }{
			{"stop", http.MethodPost, stopPath()},
			{"rename form", http.MethodGet, "/c/" + convID.String() + "/rename"},
			{"delete dialog", http.MethodGet, "/c/" + convID.String() + "/delete"},
		} {
			assert.Equal(t, http.StatusOK, h.as("tok", tc.method, tc.path, nil, htmx).Code, tc.name)
		}
		assert.Equal(t, http.StatusOK, h.as("tok", http.MethodPatch, "/c/"+convID.String(), url.Values{"title": {"new"}}, htmx).Code, "rename")
		assert.Equal(t, http.StatusOK, h.as("tok", http.MethodDelete, "/c/"+convID.String(), nil, htmx).Code, "delete")
	})

	t.Run("a rate of zero turns limiting off", func(t *testing.T) {
		h := newChatHarness(t)
		h.chat.On("Send", mock.Anything, alice.ID, convID, "hi").Return(turn, nil)

		for range 50 {
			require.Equal(t, http.StatusOK, h.do(http.MethodPost, sendPath, url.Values{"content": {"hi"}}, htmx).Code)
		}
	})
}

func TestRateLimit_Login(t *testing.T) {
	newApp := func(t *testing.T, burst int) (http.Handler, *mocks.Auth) {
		t.Helper()
		authMock := mocks.NewAuth(t)
		app := testAppWith(t, Services{Auth: authMock}, Options{RateLimitRPS: 0.0001, RateLimitBurst: burst})
		return app.Routes(), authMock
	}
	attempt := func(route http.Handler, from, email string) *httptest.ResponseRecorder {
		req := postForm("/login", url.Values{"email": {email}, "password": {"hunter2"}, "next": {"/c/1"}}, nil)
		req.RemoteAddr = from
		rec := httptest.NewRecorder()
		route.ServeHTTP(rec, req)
		return rec
	}

	t.Run("too many attempts from one address are refused, and the password is never echoed", func(t *testing.T) {
		route, authMock := newApp(t, 2)
		authMock.On("Login", mock.Anything, "a@example.com", "hunter2").Return(service.AuthSession{}, domain.ErrInvalidCredentials).Twice()
		for range 2 {
			require.Equal(t, http.StatusUnprocessableEntity, attempt(route, "192.0.2.1:5000", "a@example.com").Code)
		}

		rec := attempt(route, "192.0.2.1:5001", "a@example.com") // another port, same client

		assert.Equal(t, http.StatusTooManyRequests, rec.Code)
		assert.NotEmpty(t, rec.Header().Get("Retry-After"))
		root := parseHTML(t, rec.Body.String())
		assert.Contains(t, textContent(byTestID(root, "error")[0]), "Too many sign-in attempts. Try again in ")
		assert.Equal(t, "a@example.com", attrOf(byTestID(root, "login-email")[0], "value"), "the email is kept")
		assert.NotContains(t, rec.Body.String(), "hunter2")
		assert.Equal(t, "/c/1", attrOf(elementsOf(root, "input")[0], "value"), "and so is where the visitor was headed")
	})

	t.Run("another address is not affected", func(t *testing.T) {
		route, authMock := newApp(t, 1)
		authMock.On("Login", mock.Anything, "a@example.com", "hunter2").Return(service.AuthSession{}, domain.ErrInvalidCredentials).Twice()

		assert.Equal(t, http.StatusUnprocessableEntity, attempt(route, "192.0.2.1:5000", "a@example.com").Code)
		assert.Equal(t, http.StatusTooManyRequests, attempt(route, "192.0.2.1:5000", "a@example.com").Code)
		assert.Equal(t, http.StatusUnprocessableEntity, attempt(route, "192.0.2.2:5000", "a@example.com").Code)
	})

	t.Run("an address without a port is still one client", func(t *testing.T) {
		route, authMock := newApp(t, 1)
		authMock.On("Login", mock.Anything, "a@example.com", "hunter2").Return(service.AuthSession{}, domain.ErrInvalidCredentials).Once()

		attempt(route, "pipe", "a@example.com")

		assert.Equal(t, http.StatusTooManyRequests, attempt(route, "pipe", "a@example.com").Code)
	})

	t.Run("showing the form is not limited", func(t *testing.T) {
		route, _ := newApp(t, 1)
		for range 5 {
			rec := httptest.NewRecorder()
			route.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
			require.Equal(t, http.StatusOK, rec.Code)
		}
	})
}
