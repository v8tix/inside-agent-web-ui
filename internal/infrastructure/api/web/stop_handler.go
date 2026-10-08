package web

import (
	"errors"
	"net/http"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

// stopMessage ends a reply that is being written and answers with the reply as
// saved, to replace the one on screen. Replacing the streaming wrapper is what
// closes the browser's event stream.
func (a *App) stopMessage(w http.ResponseWriter, r *http.Request) {
	ref, ok := messageRefOf(w, r)
	if !ok {
		return
	}

	msg, err := a.svc.Chat.Stop(r.Context(), ref.userID, ref.convID, ref.msgID)
	if errors.Is(err, domain.ErrInvalidState) { // not a reply that can be stopped: as good as missing
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.answerWithReply(w, r, ref.convID, msg)
}
