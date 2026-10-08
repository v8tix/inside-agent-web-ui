// Package digestsessions issues a random token and keeps only its digest.
package digestsessions

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"time"
)

// Table stands for the sessions table: the key is the digest of a token.
type Table map[string]time.Time

func digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Issue starts a session that ends at end and returns the token for the cookie.
// The table keeps the digest and drops the token.
func (t Table) Issue(end time.Time) string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	token := base64.RawURLEncoding.EncodeToString(raw)
	t[digest(token)] = end
	return token
}

// Valid reports whether token names a session that has not ended.
func (t Table) Valid(token string, now time.Time) bool {
	end, ok := t[digest(token)]
	return ok && now.Before(end)
}
