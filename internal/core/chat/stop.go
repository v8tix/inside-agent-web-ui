package chat

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/resource"
)

// stopWait is how long Stop waits for a canceled run to save its reply. A
// browser that stopped reading can hold a run up for a moment; Stop does not
// wait on it forever.
const stopWait = 3 * time.Second

// stopAttempts bounds the look-again loop in Stop. A second pass is needed only
// when a stream starts at the very moment of the stop.
const stopAttempts = 3

// Stop implements service.Chat.
//
// Pattern: Cancel, Wait, Read Back
func (s *Service) Stop(ctx context.Context, userID, convID, msgID uuid.UUID) (domain.Message, error) {
	for range stopAttempts {
		msg, err := s.deps.Messages.Get(ctx, userID, convID, msgID)
		if err != nil {
			return domain.Message{}, err
		}
		if msg.Role != domain.RoleAssistant {
			return domain.Message{}, fmt.Errorf("a %s message cannot be stopped: %w", msg.Role, domain.ErrInvalidState)
		}
		if msg.Status.Terminal() {
			return msg, nil
		}

		if active, ok := s.runs.get(msgID); ok {
			active.cancel()
			select {
			case <-active.done:
			case <-time.After(stopWait):
				s.deps.Logger.WarnContext(ctx, "a stopped reply was slow to save", "message_id", msgID)
			case <-ctx.Done():
				return domain.Message{}, ctx.Err()
			}
			continue // read the saved state
		}

		if msg.Status != domain.StatusPending {
			// Streaming, yet nothing here is writing it.
			return s.endLost(ctx, userID, msg)
		}
		// Nobody has started this reply. Mark it stopped so it never runs.
		err = s.stopUnstarted(ctx, userID, convID, msgID)
		if err != nil && !errors.Is(err, domain.ErrAlreadyStreaming) && !errors.Is(err, domain.ErrInvalidState) {
			return domain.Message{}, err
		}
		// Either way, read the result: a stream that won the race is a run to stop,
		// and a reply that ended in the meantime is returned as it is stored.
	}
	return s.deps.Messages.Get(ctx, userID, convID, msgID)
}

// stopUnstarted moves a reply that no one has started to stopped. Between its
// claim and its save the reply is streaming, so it registers as the reply's writer
// first, as Stream does: a stream that arrives in between is told that the reply
// is being written, and another Stop waits for the save. It returns
// domain.ErrAlreadyStreaming when somebody else was writing the reply first, and
// domain.ErrInvalidState when the reply ended before it could be claimed.
func (s *Service) stopUnstarted(ctx context.Context, userID, convID, msgID uuid.UUID) error {
	// There is no run to cancel: the claim and the save finish on their own.
	active, ok := s.runs.begin(msgID, func() {})
	if !ok {
		return domain.ErrAlreadyStreaming
	}
	defer s.runs.end(msgID, active)

	claimed, err := s.claim(ctx, userID, convID, msgID)
	if err != nil {
		return err
	}
	// The reply is claimed now, so finish it even if the request goes away.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := s.deps.Messages.Finish(saveCtx, claimed.Reply.ID, resource.Outcome{Status: domain.StatusStopped}); err != nil {
		return fmt.Errorf("stopping reply: %w", err)
	}
	return nil
}

// claim moves the reply to streaming on a context the request cannot cancel. The
// database may commit a claim just as the caller gives up on it (a Stop, a closed
// tab); a claim nobody heard about would leave the reply streaming with nothing
// to write it. Whoever claims always goes on to finish, and the run's own context
// still ends the work that follows.
//
// Pattern: Detached Save
func (s *Service) claim(ctx context.Context, userID, convID, msgID uuid.UUID) (resource.Claimed, error) {
	claimCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	return s.deps.Messages.Claim(claimCtx, userID, convID, msgID)
}

// endLost ends a reply that is streaming in the database while nothing here is
// writing it: the run was lost (a failed save, a claim whose answer never came
// back) and nothing will ever finish it. The page would wait for it for good and
// Retry is refused until it ends. This process is the only one that writes
// replies, as at startup, so no one else can be on it. The reply is failed, with
// the text it has. If it finished by itself in the meantime, that is what comes
// back: a finished reply is never overwritten.
//
// Pattern: Lost-Run Repair
func (s *Service) endLost(ctx context.Context, userID uuid.UUID, msg domain.Message) (domain.Message, error) {
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	err := s.deps.Messages.Finish(saveCtx, msg.ID, resource.Outcome{
		Content: msg.Content, Status: domain.StatusFailed, Error: domain.FailureInterrupted,
	})
	switch {
	case err == nil:
		s.deps.Logger.WarnContext(ctx, "ended a reply that nothing was writing", "message_id", msg.ID)
		msg.Status, msg.Error = domain.StatusFailed, domain.FailureInterrupted
		return msg, nil
	case errors.Is(err, domain.ErrInvalidState):
		return s.deps.Messages.Get(ctx, userID, msg.ConversationID, msg.ID)
	}
	return domain.Message{}, fmt.Errorf("ending a lost reply: %w", err)
}
