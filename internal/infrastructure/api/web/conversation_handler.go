package web

import (
	"errors"
	"html"
	"net/http"
	"net/url"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

// renameView is what the inline rename form draws.
type renameView struct {
	ID       string
	Title    string // what the field holds
	Error    string
	MaxChars int
}

func newRenameView(id uuid.UUID, title, problem string) renameView {
	return renameView{ID: id.String(), Title: title, Error: problem, MaxChars: domain.MaxTitleChars}
}

// showsConversation reports whether the page that sent an htmx request is the
// one for this conversation. htmx sends the page's address in HX-Current-URL.
func showsConversation(r *http.Request, id uuid.UUID) bool {
	page, err := url.Parse(r.Header.Get("HX-Current-URL"))
	return err == nil && page.Path == "/c/"+id.String()
}

func (a *App) conversationRow(r *http.Request, c domain.Conversation) conversationItem {
	return conversationItem{ID: c.ID.String(), Title: c.Title, Active: showsConversation(r, c.ID)}
}

// findConversation reads the {id} conversation of the signed-in user, answering
// 404 itself when it is malformed, missing or someone else's.
func (a *App) findConversation(w http.ResponseWriter, r *http.Request) (domain.Conversation, bool) {
	id, ok := parseID(r)
	if !ok {
		http.NotFound(w, r)
		return domain.Conversation{}, false
	}
	user, _ := UserFrom(r.Context())
	c, err := a.svc.Conversations.Find(r.Context(), user.ID, id)
	if err != nil {
		a.fail(w, r, err)
		return domain.Conversation{}, false
	}
	return c, true
}

// block renders a template block as the response.
func (a *App) block(w http.ResponseWriter, r *http.Request, status int, name string, data any, extra ...string) {
	out, err := a.renderer.Block(name, data)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	for _, more := range extra {
		out += more
	}
	writeHTML(w, r, status, out)
}

// conversationItem answers with the sidebar row. Cancelling a rename asks for it.
func (a *App) conversationItem(w http.ResponseWriter, r *http.Request) {
	c, ok := a.findConversation(w, r)
	if !ok {
		return
	}
	a.block(w, r, http.StatusOK, "partial:conversation_item", a.conversationRow(r, c))
}

// renameForm answers with the sidebar row turned into a title field.
func (a *App) renameForm(w http.ResponseWriter, r *http.Request) {
	c, ok := a.findConversation(w, r)
	if !ok {
		return
	}
	a.block(w, r, http.StatusOK, "partial:rename_form", newRenameView(c.ID, c.Title, ""))
}

// renameConversation sets a new title. It answers with the renamed row, or with
// the form again and the problem when the title is not acceptable.
func (a *App) renameConversation(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !a.parseForm(w, r) {
		return
	}
	user, _ := UserFrom(r.Context())
	typed := r.PostFormValue("title")

	title, err := a.svc.Conversations.Rename(r.Context(), user.ID, id, typed)
	if ve, ok := errors.AsType[*domain.ValidationError](err); ok {
		a.block(w, r, http.StatusUnprocessableEntity, "partial:rename_form", newRenameView(id, typed, ve.Message))
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}

	row := conversationItem{ID: id.String(), Title: title, Active: showsConversation(r, id)}
	var extra []string
	if row.Active {
		// htmx updates the tab title from a <title> in the response.
		extra = append(extra, "<title>"+html.EscapeString(title)+" · Agent Web UI</title>")
	}
	a.block(w, r, http.StatusOK, "partial:conversation_item", row, extra...)
}

// deleteDialog answers with the confirmation for deleting a conversation.
func (a *App) deleteDialog(w http.ResponseWriter, r *http.Request) {
	c, ok := a.findConversation(w, r)
	if !ok {
		return
	}
	a.block(w, r, http.StatusOK, "partial:delete_modal", conversationItem{ID: c.ID.String(), Title: c.Title})
}

// deleteConversation removes a conversation. When the page is showing it, the
// browser goes to the start page; otherwise its sidebar row just disappears
// (the answer is empty, which replaces the row with nothing).
func (a *App) deleteConversation(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	user, _ := UserFrom(r.Context())
	if err := a.svc.Conversations.Delete(r.Context(), user.ID, id); err != nil {
		a.fail(w, r, err)
		return
	}
	if showsConversation(r, id) {
		w.Header().Set("HX-Redirect", "/")
	}
	writeHTML(w, r, http.StatusOK, "")
}
