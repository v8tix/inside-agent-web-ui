package web

import (
	"errors"
	"net/http"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

// cannotRegenerate is shown when the reply is not the latest or is still being
// written.
const cannotRegenerate = "That reply can't be regenerated right now."

// regenerateMessage replaces the conversation's last reply with a new one, which
// streams. Retrying a failed reply is the same request. The answer is the new
// reply's wrapper, to take the old one's place.
func (a *App) regenerateMessage(w http.ResponseWriter, r *http.Request) {
	ref, ok := messageRefOf(w, r)
	if !ok {
		return
	}

	reply, err := a.svc.Chat.Regenerate(r.Context(), ref.userID, ref.convID, ref.msgID)
	if errors.Is(err, domain.ErrInvalidState) {
		a.composerProblem(w, r, ref.convID, http.StatusConflict, "", cannotRegenerate)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.answerWithReply(w, r, ref.convID, reply)
}
