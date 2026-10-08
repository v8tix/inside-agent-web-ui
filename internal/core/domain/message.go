package domain

import (
	"strings"
	"time"
	"unicode/utf8"
	"uuid"
)

// Role says who wrote a message.
type Role string

// Message roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Status is where a message is in its life.
//
//	pending   -> streaming -> complete | stopped | failed
//
// Terminal messages never change again. Regenerating or retrying replaces the
// message with a new pending one.
//
// Pattern: Store-Enforced State Machine
type Status string

// Message statuses.
const (
	StatusPending   Status = "pending"
	StatusStreaming Status = "streaming"
	StatusComplete  Status = "complete"
	StatusStopped   Status = "stopped"
	StatusFailed    Status = "failed"
)

// Terminal reports whether the message has reached its final state.
func (s Status) Terminal() bool {
	return s == StatusComplete || s == StatusStopped || s == StatusFailed
}

// What the user is told when a reply fails. These are stored with the reply as
// its Error and shown in the chat as they are. Technical detail goes to the
// logs, never here.
//
// Pattern: Closed Failure Vocabulary
const (
	FailureUnavailable = "The agent can't be reached right now. Try again in a moment."
	FailureTimeout     = "The agent took too long to answer."
	FailureAgentError  = "The agent ran into a problem and couldn't finish."
	FailureIncomplete  = "The agent's answer was cut off."
	FailureTooLong     = "The agent's answer grew too long and was stopped."
	FailureUnknown     = "Something went wrong while writing the reply."
	// FailureInterrupted is for a reply that was cut off with nothing left to
	// finish it: a crash or restart recovered at startup, or a run that was lost
	// on the way.
	FailureInterrupted = "This reply was interrupted before it finished."
)

// Usage is the token count the agent reported. Zero means unknown.
type Usage struct {
	Prompt     int `json:"prompt,omitzero"`
	Completion int `json:"completion,omitzero"`
}

// Meta is what we learned while producing a reply. Zero values mean unknown
// and are left out of the stored JSON and of the page.
type Meta struct {
	// TTFTMs is the time from sending the upstream request to the first
	// non-empty piece of text, measured on the server.
	TTFTMs     int64    `json:"ttft_ms,omitzero"`
	DurationMs int64    `json:"duration_ms,omitzero"`
	Usage      Usage    `json:"usage,omitzero"`
	ToolCalls  []string `json:"tool_calls,omitempty"`
}

// Message is one turn of a conversation. Content is Markdown exactly as typed
// or as the agent produced it.
type Message struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	Seq            int
	Role           Role
	Content        string
	Status         Status
	Error          string
	Meta           Meta
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NormalizeContent trims raw and checks it against the limit, counted in
// characters. Inner whitespace and Markdown are left alone.
func NormalizeContent(raw string, maxChars int) (string, error) {
	content := strings.TrimSpace(raw)
	switch {
	case content == "":
		return "", ErrEmptyMessage
	case !storable(content):
		return "", ErrMessageInvalid
	case utf8.RuneCountInString(content) > maxChars:
		return "", TooLong(maxChars)
	}
	return content, nil
}

// storable reports whether s can be saved as text: valid UTF-8, and no NUL
// character, which the database refuses.
func storable(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }
