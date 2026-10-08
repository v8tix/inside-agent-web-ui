package web

import (
	"net/http"
	"strconv"
)

// rawMessage answers with a message exactly as it is stored, as plain text. It
// is what "copy" copies and what the evaluator scores: the Markdown the agent
// wrote, not the page's rendering of it. A reply still being written is not
// final, so it is a conflict.
func (a *App) rawMessage(w http.ResponseWriter, r *http.Request) {
	ref, ok := messageRefOf(w, r)
	if !ok {
		return
	}

	msg, err := a.svc.Conversations.Message(r.Context(), ref.userID, ref.convID, ref.msgID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if !msg.Status.Terminal() {
		http.Error(w, "This reply isn't finished yet.", http.StatusConflict)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(msg.Content)))
	_, _ = w.Write([]byte(msg.Content))
}
