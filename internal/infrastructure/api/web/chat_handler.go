package web

import (
	"errors"
	"html/template"
	"net/http"
	"strings"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

type conversationItem struct {
	ID     string
	Title  string
	Active bool
}

// messageView is what the message templates draw.
type messageView struct {
	ID             string
	ConversationID string
	Role           string
	Status         string
	Text           string        // a user's message: plain text
	Body           template.HTML // a reply: rendered, sanitized Markdown
	Error          string
	// Streaming is true while a reply is unfinished: its wrapper opens the
	// event stream, which fills it in.
	Streaming bool
	// Actions is true when the reply offers Regenerate (or Retry): it is
	// finished and the latest. loadChat clears it on earlier replies.
	Actions bool
	// Copy is true when there is a finished reply text to copy.
	Copy             bool
	TTFTMs           int64
	DurationMs       int64
	PromptTokens     int
	CompletionTokens int
	ToolCalls        string
}

func (a *App) messageView(m domain.Message) messageView {
	v := messageView{
		ID:             m.ID.String(),
		ConversationID: m.ConversationID.String(),
		Role:           string(m.Role),
		Status:         string(m.Status),
		Error:          m.Error,
	}
	if m.Role == domain.RoleUser {
		v.Text = m.Content
		return v
	}
	if m.Status.Terminal() {
		v.Body = a.md.RenderFinal(m.Content)
	} else {
		v.Body = a.md.Render(m.Content)
	}
	v.Streaming = !m.Status.Terminal()
	v.Actions = m.Status.Terminal()
	v.Copy = m.Status.Terminal() && m.Content != ""
	if m.Status.Terminal() {
		v.TTFTMs, v.DurationMs = m.Meta.TTFTMs, m.Meta.DurationMs
		v.PromptTokens, v.CompletionTokens = m.Meta.Usage.Prompt, m.Meta.Usage.Completion
		v.ToolCalls = strings.Join(m.Meta.ToolCalls, ",")
	}
	return v
}

type chatPage struct {
	basePage
	Conversations []conversationItem
	Active        *conversationItem
	Messages      []messageView
	Draft         string // what the visitor typed, when the page is shown again after a problem
	Error         string
}

// loadChat builds the page: the sidebar, and the active conversation with its
// messages when active is set. A missing or foreign conversation is
// domain.ErrNotFound.
func (a *App) loadChat(r *http.Request, active *uuid.UUID) (chatPage, error) {
	user, _ := UserFrom(r.Context())
	page := chatPage{basePage: a.base(r)}

	var msgs []domain.Message
	if active != nil {
		view, err := a.svc.Conversations.Get(r.Context(), user.ID, *active)
		if err != nil {
			return chatPage{}, err
		}
		msgs = view.Messages
		page.Active = &conversationItem{ID: view.Conversation.ID.String(), Title: view.Conversation.Title, Active: true}
	}
	list, err := a.svc.Conversations.List(r.Context(), user.ID)
	if err != nil {
		return chatPage{}, err
	}
	for _, c := range list {
		page.Conversations = append(page.Conversations, conversationItem{
			ID: c.ID.String(), Title: c.Title, Active: active != nil && c.ID == *active,
		})
	}
	for _, m := range msgs {
		page.Messages = append(page.Messages, a.messageView(m))
	}
	// Only the latest reply can be regenerated.
	for i := range len(page.Messages) - 1 {
		page.Messages[i].Actions = false
	}
	return page, nil
}

func (a *App) renderChat(w http.ResponseWriter, r *http.Request, status int, page chatPage) {
	if err := a.renderer.Page(w, r, status, "chat", page); err != nil {
		a.serverError(w, r, err)
	}
}

// newChatPage is the empty state: the sidebar and a composer that starts a
// conversation.
func (a *App) newChatPage(w http.ResponseWriter, r *http.Request) {
	page, err := a.loadChat(r, nil)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.renderChat(w, r, http.StatusOK, page)
}

// startConversation creates a conversation from the first message and goes to
// it. The conversation page opens the reply's stream as it loads, so there is
// a single path to a streaming reply. The empty-state form is boosted: htmx
// follows the redirect and swaps the page in without reloading the document.
func (a *App) startConversation(w http.ResponseWriter, r *http.Request) {
	if !a.parseForm(w, r) {
		return
	}
	user, _ := UserFrom(r.Context())
	content := r.PostFormValue("content")

	turn, err := a.svc.Chat.StartConversation(r.Context(), user.ID, content)
	if ve, ok := errors.AsType[*domain.ValidationError](err); ok {
		a.composerProblem(w, r, uuid.Nil(), http.StatusUnprocessableEntity, content, ve.Message)
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	redirect(w, r, "/c/"+turn.Conversation.ID.String())
}

func (a *App) conversationPage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	page, err := a.loadChat(r, &id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.renderChat(w, r, http.StatusOK, page)
}

// sendMessage adds a message to a conversation. htmx gets the two new bubbles
// to append (the reply's wrapper opens its stream); a plain form post goes back
// to the conversation page.
func (a *App) sendMessage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !a.parseForm(w, r) {
		return
	}
	user, _ := UserFrom(r.Context())
	content := r.PostFormValue("content")

	turn, err := a.svc.Chat.Send(r.Context(), user.ID, id, content)
	if errors.Is(err, domain.ErrInvalidState) {
		a.composerProblem(w, r, id, http.StatusConflict, content, "Wait for the current reply to finish.")
		return
	}
	if ve, ok := errors.AsType[*domain.ValidationError](err); ok {
		a.composerProblem(w, r, id, http.StatusUnprocessableEntity, content, ve.Message)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}

	if !fromHTMX(r) {
		redirect(w, r, "/c/"+id.String())
		return
	}
	var out strings.Builder
	for _, m := range []domain.Message{turn.User, turn.Reply} {
		html, err := a.renderer.Block("partial:message", a.messageView(m))
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		out.WriteString(html)
	}
	clear, err := a.renderer.Block("partial:composer_error", "")
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	out.WriteString(clear)
	writeHTML(w, r, http.StatusOK, out.String())
}

// composerProblem reports a problem with what was typed. htmx updates only the
// composer's message line and leaves the conversation alone; a plain form post
// shows the whole page again with the text kept. uuid.Nil() as convID means the
// request is not about a conversation yet (a new chat).
func (a *App) composerProblem(w http.ResponseWriter, r *http.Request, convID uuid.UUID, status int, draft, message string) {
	if fromHTMX(r) {
		html, err := a.renderer.Block("partial:composer_error", message)
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		w.Header().Set("HX-Reswap", "none")
		// A boosted form would otherwise leave its action (/c) in the address bar.
		w.Header().Set("HX-Push-Url", "false")
		writeHTML(w, r, status, html)
		return
	}
	var active *uuid.UUID
	if convID != uuid.Nil() {
		active = &convID
	}
	page, err := a.loadChat(r, active)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	page.Draft, page.Error = draft, message
	a.renderChat(w, r, status, page)
}

// parseForm reads the form, answering 413 or 400 itself when it cannot.
func (a *App) parseForm(w http.ResponseWriter, r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		if !writeBodyError(w, err) {
			http.Error(w, "bad form", http.StatusBadRequest)
		}
		return false
	}
	return true
}

func writeHTML(w http.ResponseWriter, r *http.Request, status int, html string) {
	sendHTML(w, r, status, []byte(html))
}
