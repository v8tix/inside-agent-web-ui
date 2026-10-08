// Package envcfg reads settings from the process environment and a .env file,
// and holds the type that keeps a secret out of logs and error messages. The Web
// UI and the evaluator both use it, and it knows about neither.
package envcfg

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"github.com/joho/godotenv"
)

// LookupFunc reports the value of a configuration key and whether it is set.
type LookupFunc func(key string) (string, bool)

// Secret is a string that does not print or encode itself: fmt, slog and the
// encoders that ask a value for its text get the placeholder. Call Reveal at
// the point of use. It has to sit in an exported field, since nothing can ask
// a field that is not exported how it prints. An encoder that asks no method
// of the value, as encoding/gob does, writes it.
//
// Pattern: Self-Redacting Secret
type Secret string

// Reveal returns the underlying value.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer.
func (Secret) String() string { return "[redacted]" }

// GoString implements fmt.GoStringer.
func (Secret) GoString() string { return `"[redacted]"` }

// Format implements fmt.Formatter, so that every verb fmt asks a value about
// prints the placeholder. Without it a verb that is not for text, such as %d,
// prints the value. fmt answers %p and %w itself, before it asks: given a Secret
// they print the value. go vet refuses that %w; it does not check the other
// verbs of a type that has a Format method, so a test refuses the %p:
// TestNoProductionStringHoldsThePointerVerb of internal/architecture fails on
// a string of a production file that holds the verb.
func (s Secret) Format(f fmt.State, verb rune) {
	text := s.String()
	if verb == 'q' || (verb == 'v' && f.Flag('#')) {
		text = s.GoString()
	}
	_, _ = io.WriteString(f, text)
}

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// MarshalText implements encoding.TextMarshaler, which encoding/json (either
// version) asks for before it writes a string, so a Secret is not written as one.
func (Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// EnvLookup returns a lookup over the process environment and the .env file at
// path. Process values win over file values, and an empty process value counts
// as unset. A missing file is not an error.
//
// Pattern: One Home for Settings
func EnvLookup(path string) (LookupFunc, error) {
	fileValues := map[string]string{}
	if path != "" {
		values, err := godotenv.Read(path)
		switch {
		case err == nil:
			fileValues = values
		case errors.Is(err, fs.ErrNotExist):
		case isFileError(err):
			return nil, fmt.Errorf("reading %s: %w", path, err)
		default:
			// The parser quotes the text it choked on, which is the rest of a file
			// full of secrets, and this error goes to the terminal and the logs.
			return nil, fmt.Errorf("%s has a syntax error (its text is not shown, because the file holds secrets): "+
				"check for a byte order mark, a name with characters other than letters, digits and _, or an unterminated quote", path)
		}
	}
	return func(key string) (string, bool) {
		if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
			return v, true
		}
		v, ok := fileValues[key]
		return v, ok
	}, nil
}

// isFileError reports whether err is the operating system saying why the file
// could not be opened or read, as opposed to the parser rejecting its content.
func isFileError(err error) bool {
	_, ok := errors.AsType[*fs.PathError](err)
	return ok
}
