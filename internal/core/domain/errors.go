// Package domain holds the entities and rules of the chat application. It has
// no dependencies on HTTP, databases or the agent protocol.
package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors. Adapters map them to responses; use errors.Is to test them.
var (
	// ErrNotFound means the thing does not exist or belongs to someone else.
	// The two are deliberately indistinguishable to callers.
	ErrNotFound = errors.New("not found")
	// ErrInvalidCredentials is the single answer for a failed login.
	ErrInvalidCredentials = errors.New("invalid email or password")
	// ErrAlreadyStreaming means another connection is running this reply.
	ErrAlreadyStreaming = errors.New("reply is already streaming")
	// ErrInvalidState means the operation does not fit the message's status.
	ErrInvalidState = errors.New("operation not allowed in the current state")
	// ErrAgentUnavailable means the agent could not be reached.
	ErrAgentUnavailable = errors.New("agent unavailable")
	// ErrEmailTaken means an account with that email already exists.
	ErrEmailTaken = errors.New("email already registered")
	// ErrBusy means there is more to do than the server can take on at the
	// moment: the request gave up waiting for its turn. Asking again a little
	// later may work.
	ErrBusy = errors.New("too busy")
)

// ValidationError is a problem with user input, phrased for the user. Errors
// with the same Code match each other under errors.Is, so a specific error such
// as TooLong(500) still matches the ErrMessageTooLong sentinel.
type ValidationError struct {
	Code    string
	Field   string
	Message string
}

// Error returns the user-facing message.
func (e *ValidationError) Error() string { return e.Message }

// Is reports whether target is a ValidationError with the same code.
func (e *ValidationError) Is(target error) bool {
	t, ok := target.(*ValidationError)
	return ok && t.Code == e.Code
}

// Validation errors for chat messages.
var (
	ErrEmptyMessage = &ValidationError{
		Code: "message_empty", Field: "content", Message: "Write a message first.",
	}
	ErrMessageTooLong = &ValidationError{
		Code: "message_too_long", Field: "content", Message: "That message is too long.",
	}
	// ErrMessageInvalid is for text that cannot be stored: bytes that are not
	// UTF-8, or a NUL character, which the database refuses.
	ErrMessageInvalid = &ValidationError{
		Code: "message_invalid", Field: "content", Message: "That message has characters that can't be saved.",
	}
)

// TooLong returns the message-too-long error with the limit spelled out.
func TooLong(limit int) *ValidationError {
	return &ValidationError{
		Code:    ErrMessageTooLong.Code,
		Field:   "content",
		Message: fmt.Sprintf("That message is too long. The limit is %d characters.", limit),
	}
}
