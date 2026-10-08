package domain

import (
	"strings"
	"time"
	"unicode/utf8"
	"uuid"
)

// Title limits, in characters.
const (
	// DefaultTitle is used when a conversation has nothing to name it after.
	DefaultTitle = "New chat"
	// AutoTitleChars is how much of the first message becomes the title.
	AutoTitleChars = 60
	// MaxTitleChars is the longest title a user may set.
	MaxTitleChars = 120
)

// Conversation is a chat thread owned by one user.
type Conversation struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TitleFrom derives a conversation title from its first message: one line, at
// most AutoTitleChars characters, with an ellipsis when it was cut.
func TitleFrom(content string) string {
	line := collapseSpace(content)
	if line == "" {
		return DefaultTitle
	}
	if utf8.RuneCountInString(line) <= AutoTitleChars {
		return line
	}
	cut := []rune(line)[:AutoTitleChars]
	return strings.TrimRight(string(cut), " ") + "…"
}

// NormalizeTitle validates a title a user typed: one line, 1 to MaxTitleChars
// characters.
func NormalizeTitle(raw string) (string, error) {
	title := collapseSpace(raw)
	switch {
	case title == "":
		return "", &ValidationError{Code: "title_empty", Field: "title", Message: "Give the chat a name."}
	case !storable(title):
		return "", &ValidationError{Code: "title_invalid", Field: "title", Message: "That name has characters that can't be saved."}
	case utf8.RuneCountInString(title) > MaxTitleChars:
		return "", &ValidationError{
			Code: "title_too_long", Field: "title",
			Message: "That name is too long. The limit is 120 characters.",
		}
	}
	return title, nil
}

// collapseSpace trims s and folds every run of whitespace, newlines included,
// into a single space.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
