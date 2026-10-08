package domain

import (
	"net/mail"
	"strings"
	"time"
	"uuid"
)

// MinPasswordChars is the shortest password the application accepts when it
// sets one itself (the bootstrap account).
const MinPasswordChars = 12

// User is an account. PasswordHash is an argon2id PHC string.
type User struct {
	ID           uuid.UUID
	Email        string
	PasswordHash string
	CreatedAt    time.Time
}

// Session is a logged-in browser. ID is the SHA-256 hex digest of the cookie
// token, so the database never holds a usable token.
type Session struct {
	ID        string
	UserID    uuid.UUID
	ExpiresAt time.Time
	CreatedAt time.Time
}

// NormalizeEmail trims and lowercases an address and requires a plain
// "user@host" form (no display name).
func NormalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return "", &ValidationError{Code: "email_invalid", Field: "email", Message: "Enter a valid email address."}
	}
	return email, nil
}
