package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
)

func rawPath() string { return "/c/" + convID.String() + "/m/" + streamMsgID.String() + "/raw" }

func stored(role domain.Role, status domain.Status, content string) domain.Message {
	return domain.Message{ID: streamMsgID, ConversationID: convID, Seq: 2, Role: role, Status: status, Content: content}
}

func TestRawMessage_Content(t *testing.T) {
	// Whatever is stored comes out byte for byte: no escaping, no trimming, no
	// line-ending changes, no Markdown processing.
	tests := []struct {
		name    string
		content string
	}{
		{"plain text", "The answer is 42."},
		{"empty", ""},
		{"markdown", "# Title\n\n- one\n- two\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n```go\nfmt.Println(\"<hi>\")\n```\n"},
		{"markup stays text", `<script>alert(1)</script> & <b>bold</b> &amp; "quotes" 'single'`},
		{"unicode", "h\u00e9llo w\u00f6rld \u2014 \u65e5\u672c\u8a9e \U0001F642 \u0627\u0644\u0639\u0631\u0628\u064a\u0629 e\u0301 \u202eRTL\u202c \u01c5"},
		{"line endings are kept as they are", "a\r\nb\rc\nd"},
		{"whitespace at the edges", "\n\n  indented\ttab  \n\n"},
		{"markdown specials", "*not bold* _x_ `code` [link](http://example.test) ![img](x.png) \\ # > ~~strike~~ ***"},
		{"a long reply", strings.Repeat("0123456789 ", 100_000)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatHarness(t)
			h.convs.On("Message", mock.Anything, alice.ID, convID, streamMsgID).
				Return(stored(domain.RoleAssistant, domain.StatusComplete, tc.content), nil)

			rec := h.do(http.MethodGet, rawPath(), nil, nil)

			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, tc.content, rec.Body.String(), "byte for byte")
			assert.Equal(t, strconv.Itoa(len(tc.content)), rec.Header().Get("Content-Length"))
		})
	}
}

func TestRawMessage_Headers(t *testing.T) {
	h := newChatHarness(t)
	h.convs.On("Message", mock.Anything, alice.ID, convID, streamMsgID).
		Return(stored(domain.RoleAssistant, domain.StatusComplete, "<script>alert(1)</script>"), nil)

	rec := h.do(http.MethodGet, rawPath(), nil, nil)

	assert.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"), "never HTML, even if the text looks like it")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), "the browser must not guess it is something else")
}

func TestRawMessage_Status(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		message    domain.Message
		err        error
		callsSvc   bool
		wantStatus int
	}{
		{"a finished reply", rawPath(), stored(domain.RoleAssistant, domain.StatusComplete, "x"), nil, true, http.StatusOK},
		{"a stopped reply keeps its text", rawPath(), stored(domain.RoleAssistant, domain.StatusStopped, "half"), nil, true, http.StatusOK},
		{"a failed reply keeps what arrived", rawPath(), stored(domain.RoleAssistant, domain.StatusFailed, "par"), nil, true, http.StatusOK},
		{"the user's own message", rawPath(), stored(domain.RoleUser, domain.StatusComplete, "my question"), nil, true, http.StatusOK},
		{"a reply that is being written is not final", rawPath(), stored(domain.RoleAssistant, domain.StatusStreaming, "so far"), nil, true, http.StatusConflict},
		{"a reply that has not started", rawPath(), stored(domain.RoleAssistant, domain.StatusPending, ""), nil, true, http.StatusConflict},
		{"not found or someone else's", rawPath(), domain.Message{}, domain.ErrNotFound, true, http.StatusNotFound},
		{"a malformed conversation id", "/c/nope/m/" + streamMsgID.String() + "/raw", domain.Message{}, nil, false, http.StatusNotFound},
		{"a malformed message id", "/c/" + convID.String() + "/m/nope/raw", domain.Message{}, nil, false, http.StatusNotFound},
		{"the use case fails", rawPath(), domain.Message{}, errors.New("db password=hunter2 refused"), true, http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatHarness(t)
			if tc.callsSvc {
				h.convs.On("Message", mock.Anything, alice.ID, convID, streamMsgID).Return(tc.message, tc.err)
			}

			rec := h.do(http.MethodGet, tc.path, nil, nil)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.NotContains(t, rec.Body.String(), "hunter2")
			if tc.wantStatus == http.StatusConflict && tc.message.Content != "" {
				assert.NotContains(t, rec.Body.String(), tc.message.Content, "unfinished text is not handed out")
			}
		})
	}

	t.Run("only GET reads it", func(t *testing.T) {
		h := newChatHarness(t)

		assert.Equal(t, http.StatusMethodNotAllowed, h.do(http.MethodPost, rawPath(), nil, nil).Code)
	})
}

func TestConversationPage_CopyButton(t *testing.T) {
	tests := []struct {
		name     string
		messages []domain.Message
		want     []bool // per message, in order: does it offer Copy?
	}{
		{
			"every finished reply with text, earlier ones too",
			[]domain.Message{
				msg(1, domain.RoleUser, domain.StatusComplete, "q1"), msg(2, domain.RoleAssistant, domain.StatusComplete, "a1"),
				msg(3, domain.RoleUser, domain.StatusComplete, "q2"), msg(4, domain.RoleAssistant, domain.StatusComplete, "a2"),
			},
			[]bool{false, true, false, true},
		},
		{
			"a stopped reply has text to copy",
			[]domain.Message{msg(1, domain.RoleUser, domain.StatusComplete, "q"), msg(2, domain.RoleAssistant, domain.StatusStopped, "half")},
			[]bool{false, true},
		},
		{
			"a failed reply with partial text has something to copy",
			[]domain.Message{msg(1, domain.RoleUser, domain.StatusComplete, "q"), msg(2, domain.RoleAssistant, domain.StatusFailed, "par")},
			[]bool{false, true},
		},
		{
			"nothing to copy when no text arrived",
			[]domain.Message{msg(1, domain.RoleUser, domain.StatusComplete, "q"), msg(2, domain.RoleAssistant, domain.StatusFailed, "")},
			[]bool{false, false},
		},
		{
			"nothing while it is being written",
			[]domain.Message{msg(1, domain.RoleUser, domain.StatusComplete, "q"), msg(2, domain.RoleAssistant, domain.StatusStreaming, "so far")},
			[]bool{false, false},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatHarness(t)
			h.convs.On("Get", mock.Anything, alice.ID, convID).Return(service.ConversationView{
				Conversation: conv(convID, "c"), Messages: tc.messages,
			}, nil)
			h.expectSidebar()

			rec := h.do(http.MethodGet, "/c/"+convID.String(), nil, nil)

			root := parseHTML(t, rec.Body.String())
			var got []bool
			for _, m := range byTestID(root, "message") {
				button := byTestID(m, "copy-message")
				got = append(got, len(button) == 1)
				if len(button) == 1 {
					assert.Equal(t, "/c/"+convID.String()+"/m/"+attrOf(m, "data-id")+"/raw", attrOf(button[0], "data-raw-url"))
					assert.Equal(t, "Copy response", attrOf(button[0], "aria-label"))
					assert.Equal(t, "Copy", textContent(button[0]))
				}
			}
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("earlier replies keep Copy but lose Regenerate", func(t *testing.T) {
		h := newChatHarness(t)
		h.convs.On("Get", mock.Anything, alice.ID, convID).Return(service.ConversationView{
			Conversation: conv(convID, "c"),
			Messages: []domain.Message{
				msg(1, domain.RoleUser, domain.StatusComplete, "q1"), msg(2, domain.RoleAssistant, domain.StatusComplete, "a1"),
				msg(3, domain.RoleUser, domain.StatusComplete, "q2"), msg(4, domain.RoleAssistant, domain.StatusComplete, "a2"),
			},
		}, nil)
		h.expectSidebar()

		rec := h.do(http.MethodGet, "/c/"+convID.String(), nil, nil)

		root := parseHTML(t, rec.Body.String())
		assert.Len(t, byTestID(root, "copy-message"), 2)
		assert.Len(t, byTestID(root, "regenerate"), 1, "only the latest")
	})
}
