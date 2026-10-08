// Package secret holds a string type that shows a
// placeholder to whatever asks it for its text.
package secret

import (
	"fmt"
	"log/slog"
)

const placeholder = "[redacted]"

// Secret hides its value. Call Reveal at the place
// that needs it, and keep it in an exported field.
type Secret string

// Reveal returns the value.
func (s Secret) Reveal() string { return string(s) }

// String is the text of a Secret for package fmt.
func (Secret) String() string { return placeholder }

// GoString is its text for the verbs %#v and %q.
func (Secret) GoString() string { return `"[redacted]"` }

// Format covers every other verb, such as %d. fmt
// answers %p and %w itself: they print the value.
func (s Secret) Format(f fmt.State, verb rune) {
	text := s.String()
	if verb == 'q' || (verb == 'v' && f.Flag('#')) {
		text = s.GoString()
	}
	_, _ = fmt.Fprint(f, text)
}

// LogValue covers package log/slog.
func (Secret) LogValue() slog.Value { return slog.StringValue(placeholder) }

// MarshalText covers encoding/json.
func (Secret) MarshalText() ([]byte, error) { return []byte(placeholder), nil }
