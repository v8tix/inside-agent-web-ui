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
)

// viewing is the header htmx sends: the address of the page the request comes from.
func viewing(id uuid.UUID) map[string]string {
	return map[string]string{"HX-Request": "true", "HX-Current-URL": "http://example.test/c/" + id.String()}
}

func titled(id uuid.UUID, title string) domain.Conversation {
	return domain.Conversation{ID: id, UserID: alice.ID, Title: title}
}

func TestSidebarRow(t *testing.T) {
	other := uuid.NewV7()
	h := newChatHarness(t)
	h.expectSidebar(titled(convID, `Plan "A" <b>now</b>`), titled(other, "Second"))

	rec := h.do(http.MethodGet, "/", nil, nil)

	root := parseHTML(t, rec.Body.String())
	rows := byTestID(root, "conversation")
	require.Len(t, rows, 2)
	renames, deletes := byTestID(root, "rename"), byTestID(root, "delete")
	require.Len(t, renames, 2)
	require.Len(t, deletes, 2)

	t.Run("the row has an id the page and the evaluator can find it by", func(t *testing.T) {
		assert.Equal(t, convID.String(), attrOf(rows[0], "data-id"))
		assert.Equal(t, "conversation-"+convID.String(), attrOf(rows[0], "id"))
	})
	t.Run("rename opens the form in place of the row", func(t *testing.T) {
		assert.Equal(t, "/c/"+convID.String()+"/rename", attrOf(renames[0], "hx-get"))
		assert.Equal(t, "#conversation-"+convID.String(), attrOf(renames[0], "hx-target"))
		assert.Equal(t, "outerHTML", attrOf(renames[0], "hx-swap"))
	})
	t.Run("delete asks first, in a dialog", func(t *testing.T) {
		assert.Equal(t, "/c/"+convID.String()+"/delete", attrOf(deletes[0], "hx-get"))
		assert.Equal(t, "#modal-root", attrOf(deletes[0], "hx-target"))
		assert.Empty(t, byTestID(root, "delete-confirm"), "nothing deletes straight from the sidebar")
	})
	t.Run("the buttons are named after their conversation, and titles stay text", func(t *testing.T) {
		assert.Equal(t, `Rename conversation Plan "A" <b>now</b>`, attrOf(renames[0], "aria-label"))
		assert.Equal(t, `Delete conversation Plan "A" <b>now</b>`, attrOf(deletes[0], "aria-label"))
		assert.Equal(t, "Rename conversation Second", attrOf(renames[1], "aria-label"))
		assert.Empty(t, elementsOf(root, "b"), "a title is never markup")
	})
	t.Run("the page has a place for dialogs", func(t *testing.T) {
		var found bool
		walkHTML(root, func(n *html.Node) {
			if n.Type == html.ElementNode && attrOf(n, "id") == "modal-root" {
				found = true
			}
		})
		assert.True(t, found)
	})
}

func TestRenameForm(t *testing.T) {
	t.Run("turns the row into a title field holding the current title", func(t *testing.T) {
		h := newChatHarness(t)
		h.convs.On("Find", mock.Anything, alice.ID, convID).Return(titled(convID, `A "quoted" <i>title</i>`), nil)

		rec := h.do(http.MethodGet, "/c/"+convID.String()+"/rename", nil, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		root := parseHTML(t, rec.Body.String())
		input := byTestID(root, "rename-input")
		require.Len(t, input, 1)
		assert.Equal(t, `A "quoted" <i>title</i>`, attrOf(input[0], "value"))
		assert.Equal(t, "title", attrOf(input[0], "name"))
		assert.Equal(t, "120", attrOf(input[0], "maxlength"))
		assert.True(t, hasAttr(input[0], "required"))
		assert.Empty(t, elementsOf(root, "i"), "the title is a value, not markup")
		form := byTestID(root, "rename-form")
		require.Len(t, form, 1)
		assert.Equal(t, "/c/"+convID.String(), attrOf(form[0], "hx-patch"))
		assert.Equal(t, "#conversation-"+convID.String(), attrOf(form[0], "hx-target"))
		cancel := byTestID(root, "rename-cancel")
		require.Len(t, cancel, 1)
		assert.Equal(t, "/c/"+convID.String()+"/item", attrOf(cancel[0], "hx-get"))
		assert.Empty(t, textContent(byTestID(root, "rename-error")[0]), "no complaint yet")
		label := elementsOf(root, "label")
		require.Len(t, label, 1)
		assert.Equal(t, attrOf(input[0], "id"), attrOf(label[0], "for"), "the field has a label")
	})

	t.Run("someone else's or a missing conversation is not found", func(t *testing.T) {
		h := newChatHarness(t)
		h.convs.On("Find", mock.Anything, alice.ID, convID).Return(domain.Conversation{}, domain.ErrNotFound)

		rec := h.do(http.MethodGet, "/c/"+convID.String()+"/rename", nil, htmx)

		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

func TestConversationItem(t *testing.T) {
	tests := []struct {
		name       string
		headers    map[string]string
		wantActive bool
	}{
		{"the row of the conversation on screen is active", viewing(convID), true},
		{"another page: not active", viewing(uuid.NewV7()), false},
		{"no page address: not active", htmx, false},
		{"a garbled address: not active", map[string]string{"HX-Request": "true", "HX-Current-URL": "::not a url"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatHarness(t)
			h.convs.On("Find", mock.Anything, alice.ID, convID).Return(titled(convID, "Cancelled edit"), nil)

			rec := h.do(http.MethodGet, "/c/"+convID.String()+"/item", nil, tc.headers)

			require.Equal(t, http.StatusOK, rec.Code)
			root := parseHTML(t, rec.Body.String())
			row := byTestID(root, "conversation")
			require.Len(t, row, 1)
			assert.Equal(t, tc.wantActive, hasAttr(byTestID(root, "conversation-link")[0], "aria-current"))
			assert.Equal(t, "Cancelled edit", textContent(byTestID(root, "conversation-link")[0]))
		})
	}
}

func TestRenameConversation(t *testing.T) {
	path := "/c/" + convID.String()

	t.Run("answers with the renamed row; the title is what was stored, and text", func(t *testing.T) {
		h := newChatHarness(t)
		h.convs.On("Rename", mock.Anything, alice.ID, convID, "  <b>New</b> name ").Return("<b>New</b> name", nil)

		rec := h.do(http.MethodPatch, path, url.Values{"title": {"  <b>New</b> name "}}, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		root := parseHTML(t, rec.Body.String())
		link := byTestID(root, "conversation-link")
		require.Len(t, link, 1)
		assert.Equal(t, "<b>New</b> name", textContent(link[0]))
		assert.Empty(t, elementsOf(root, "b"), "escaped, not parsed")
		assert.False(t, hasAttr(link[0], "aria-current"), "this page is not the conversation")
		assert.Empty(t, elementsOf(root, "title"), "the tab title only changes for the open conversation")
	})

	t.Run("renaming the open conversation also updates the tab title", func(t *testing.T) {
		h := newChatHarness(t)
		h.convs.On("Rename", mock.Anything, alice.ID, convID, "A & B").Return("A & B", nil)

		rec := h.do(http.MethodPatch, path, url.Values{"title": {"A & B"}}, viewing(convID))

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), "<title>A &amp; B · Agent Web UI</title>")
		root := parseHTML(t, rec.Body.String())
		assert.Equal(t, "page", attrOf(byTestID(root, "conversation-link")[0], "aria-current"))
		assert.Contains(t, attrOf(byTestID(root, "conversation")[0], "class"), "active")
	})

	t.Run("a title that is not acceptable brings the form back with what was typed and why", func(t *testing.T) {
		h := newChatHarness(t)
		problem := &domain.ValidationError{Code: "title_empty", Field: "title", Message: "Give the chat a name."}
		h.convs.On("Rename", mock.Anything, alice.ID, convID, "   ").Return("", problem)

		rec := h.do(http.MethodPatch, path, url.Values{"title": {"   "}}, htmx)

		require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		root := parseHTML(t, rec.Body.String())
		assert.Equal(t, "Give the chat a name.", textContent(byTestID(root, "rename-error")[0]))
		input := byTestID(root, "rename-input")[0]
		assert.Equal(t, "   ", attrOf(input, "value"))
		assert.Equal(t, "true", attrOf(input, "aria-invalid"))
		assert.Equal(t, attrOf(byTestID(root, "rename-error")[0], "id"), attrOf(input, "aria-describedby"))
	})

	failures := []struct {
		name       string
		path       string
		err        error
		callsRen   bool
		wantStatus int
	}{
		{"someone else's conversation", path, domain.ErrNotFound, true, http.StatusNotFound},
		{"a malformed id", "/c/nope", nil, false, http.StatusNotFound},
		{"the use case fails", path, errors.New("db password=hunter2 refused"), true, http.StatusInternalServerError},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatHarness(t)
			if tc.callsRen {
				h.convs.On("Rename", mock.Anything, alice.ID, convID, "x").Return("", tc.err)
			}

			rec := h.do(http.MethodPatch, tc.path, url.Values{"title": {"x"}}, htmx)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.NotContains(t, rec.Body.String(), "hunter2")
		})
	}
}

func TestDeleteDialog(t *testing.T) {
	t.Run("asks for confirmation, naming the conversation as text", func(t *testing.T) {
		h := newChatHarness(t)
		h.convs.On("Find", mock.Anything, alice.ID, convID).Return(titled(convID, "<script>alert(1)</script> plans"), nil)

		rec := h.do(http.MethodGet, "/c/"+convID.String()+"/delete", nil, htmx)

		require.Equal(t, http.StatusOK, rec.Code)
		root := parseHTML(t, rec.Body.String())
		assert.Empty(t, elementsOf(root, "script"))
		assert.Equal(t, "<script>alert(1)</script> plans", textContent(byTestID(root, "delete-title")[0]))
		dialog := byTestID(root, "delete-dialog")
		require.Len(t, dialog, 1)
		assert.Equal(t, "delete-modal-title", attrOf(dialog[0], "aria-labelledby"), "the dialog is named")
		assert.Contains(t, attrOf(dialog[0], "class"), "modal")
		confirm := byTestID(root, "delete-confirm")
		require.Len(t, confirm, 1)
		assert.Equal(t, "/c/"+convID.String(), attrOf(confirm[0], "hx-delete"))
		assert.Equal(t, "#conversation-"+convID.String(), attrOf(confirm[0], "hx-target"))
		assert.Equal(t, "Delete", textContent(confirm[0]))
		assert.Len(t, byTestID(root, "delete-cancel"), 1)
	})

	t.Run("someone else's conversation is not found", func(t *testing.T) {
		h := newChatHarness(t)
		h.convs.On("Find", mock.Anything, alice.ID, convID).Return(domain.Conversation{}, domain.ErrNotFound)

		rec := h.do(http.MethodGet, "/c/"+convID.String()+"/delete", nil, htmx)

		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

func TestDeleteConversation(t *testing.T) {
	path := "/c/" + convID.String()

	t.Run("deleting the open conversation sends the browser to the start page", func(t *testing.T) {
		h := newChatHarness(t)
		h.convs.On("Delete", mock.Anything, alice.ID, convID).Return(nil)

		rec := h.do(http.MethodDelete, path, nil, viewing(convID))

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "/", rec.Header().Get("HX-Redirect"))
		assert.Empty(t, rec.Body.String())
	})

	t.Run("deleting another one only removes its row, and leaves the open page alone", func(t *testing.T) {
		h := newChatHarness(t)
		h.convs.On("Delete", mock.Anything, alice.ID, convID).Return(nil)

		rec := h.do(http.MethodDelete, path, nil, viewing(uuid.NewV7()))

		assert.Equal(t, http.StatusOK, rec.Code, "200, because 204 would leave the row in place")
		assert.Empty(t, rec.Header().Get("HX-Redirect"))
		assert.Empty(t, rec.Body.String(), "an empty answer replaces the row with nothing")
	})

	failures := []struct {
		name       string
		path       string
		err        error
		calls      bool
		wantStatus int
	}{
		{"someone else's conversation", path, domain.ErrNotFound, true, http.StatusNotFound},
		{"a malformed id", "/c/nope", nil, false, http.StatusNotFound},
		{"the use case fails", path, errors.New("db password=hunter2 refused"), true, http.StatusInternalServerError},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatHarness(t)
			if tc.calls {
				h.convs.On("Delete", mock.Anything, alice.ID, convID).Return(tc.err)
			}

			rec := h.do(http.MethodDelete, tc.path, nil, htmx)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.NotContains(t, rec.Body.String(), "hunter2")
			assert.Empty(t, rec.Header().Get("HX-Redirect"))
		})
	}
}
