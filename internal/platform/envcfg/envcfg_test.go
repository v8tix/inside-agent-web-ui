package envcfg_test

import (
	"bytes"
	"encoding"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
)

func TestSecret_NeverPrintsItself(t *testing.T) {
	const value = "hunter2-canary"
	s := envcfg.Secret(value)

	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("loaded", "token", s, "group", slog.GroupValue(slog.Any("inner", s)))

	for _, shown := range []string{
		fmt.Sprint(s), fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s), fmt.Sprintf("<%s>", s),
		fmt.Sprintf("%v", struct{ Token envcfg.Secret }{s}), fmt.Sprintf("%+v", &struct{ Token envcfg.Secret }{s}),
		logged.String(),
	} {
		assert.NotContains(t, shown, value)
	}
	assert.Contains(t, logged.String(), "[redacted]")
	assert.Equal(t, value, s.Reveal(), "and the value is still there for whoever needs it")

	// Any letter can follow the percent sign, and fmt hands each one to a value that
	// formats itself. A value that does not gets a complaint for a letter that is not
	// for text, or that fmt does not know, and the complaint quotes the value.
	t.Run("under every verb", func(t *testing.T) {
		held := struct{ Token envcfg.Secret }{s}
		for _, verb := range "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ" {
			if verb == 'p' || verb == 'w' {
				continue // fmt answers these two itself: see below
			}
			for _, flags := range []string{"", "+", "#", "012.3"} {
				format := "%" + flags + string(verb)
				for _, shown := range []string{fmt.Sprintf(format, s), fmt.Sprintf(format, held), fmt.Sprintf(format, []envcfg.Secret{s})} {
					assert.NotContains(t, shown, value, format)
				}
			}
		}
		for format, want := range map[string]string{
			"%s": "[redacted]", "%d": "[redacted]", "%x": "[redacted]", "%y": "[redacted]",
			"%q": `"[redacted]"`, "%#v": `"[redacted]"`, "%T": "envcfg.Secret",
		} {
			assert.Equal(t, want, fmt.Sprintf(format, s), format)
		}
	})

	// %p and %w are for pointers and for errors, and fmt answers them before it asks
	// the value how it prints. Given a Secret it complains, and quotes the value,
	// whatever methods the value has.
	t.Run("but fmt answers the pointer verb and the error verb itself", func(t *testing.T) {
		for _, format := range []string{"%p", "%w"} {
			assert.Contains(t, fmt.Sprintf(format, s), value, format)
		}
	})
}

// A configuration is also dumped as JSON, for a bug report or a debug endpoint,
// and the encoders do not call String: without its own text a Secret is written
// as the string it is.
func TestSecret_NeverWritesItself(t *testing.T) {
	const value = "hunter2-canary"
	type settings struct {
		Token envcfg.Secret `json:"token"`
	}
	in := settings{Token: envcfg.Secret(value)}

	tests := []struct {
		name  string
		write func(any) ([]byte, error)
	}{
		{"encoding/json", json.Marshal},
		{"encoding/json/v2", func(v any) ([]byte, error) { return jsonv2.Marshal(v) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.write(in)

			require.NoError(t, err)
			assert.NotContains(t, string(out), value)
			assert.JSONEq(t, `{"token":"[redacted]"}`, string(out))
		})
	}
	t.Run("as text", func(t *testing.T) {
		marshaler, ok := any(envcfg.Secret(value)).(encoding.TextMarshaler)
		require.True(t, ok, "so every encoder that asks for text gets the placeholder")

		text, err := marshaler.MarshalText()

		require.NoError(t, err)
		assert.Equal(t, "[redacted]", string(text))
	})
}

func TestEnvLookup(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), ".env")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	const file = "# local settings\nFROM_FILE=file\nBOTH=file\nBLANK_IN_ENV=file\n"

	tests := []struct {
		name  string
		path  func(t *testing.T) string
		env   map[string]string
		key   string
		want  string
		found bool
	}{
		{name: "a value in the file", path: func(t *testing.T) string { return write(t, file) }, key: "FROM_FILE", want: "file", found: true},
		{name: "the process wins over the file", path: func(t *testing.T) string { return write(t, file) }, env: map[string]string{"BOTH": "env"}, key: "BOTH", want: "env", found: true},
		{name: "an empty process value counts as unset", path: func(t *testing.T) string { return write(t, file) }, env: map[string]string{"BLANK_IN_ENV": "  "}, key: "BLANK_IN_ENV", want: "file", found: true},
		{name: "a missing file is not an error", path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope.env") }, env: map[string]string{"ONLY_ENV": "env"}, key: "ONLY_ENV", want: "env", found: true},
		{name: "no file named at all", path: func(*testing.T) string { return "" }, env: map[string]string{"ONLY_ENV": "env"}, key: "ONLY_ENV", want: "env", found: true},
		{name: "a key nobody set", path: func(t *testing.T) string { return write(t, file) }, key: "NOWHERE", found: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			lookup, err := envcfg.EnvLookup(tc.path(t))
			require.NoError(t, err)

			got, found := lookup(tc.key)

			assert.Equal(t, tc.found, found)
			assert.Equal(t, tc.want, got)
		})
	}
}

// The file holds secrets. godotenv puts the text it choked on into its error,
// which reaches the terminal and the log files, so none of it may be passed on.
func TestEnvLookup_ASyntaxErrorDoesNotEchoTheFile(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"a name with a dash", "DB_URL=postgres://u:p@h/d\nBAD-KEY=x\nHF_TOKEN=canary-token-1\n"},
		{"an unterminated quote", "HF_TOKEN=\"canary-token-2\nADMIN_PASSWORD=canary-password-3\n"},
		{"a byte order mark", string(rune(0xFEFF)) + "HF_TOKEN=canary-token-4\nADMIN_PASSWORD=canary-password-5\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".env")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))

			_, err := envcfg.EnvLookup(path)

			if err == nil {
				t.Skip("the parser accepts this input; nothing to hide")
			}
			assert.NotContains(t, err.Error(), "canary", "none of the file reaches the message")
			assert.NotContains(t, err.Error(), "BAD-KEY")
			assert.Contains(t, err.Error(), path, "but it says which file")
			assert.Contains(t, err.Error(), "syntax")
		})
	}

	t.Run("a file that cannot be read still says why", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), ".env")
		require.NoError(t, os.Mkdir(path, 0o700)) // a directory where a file should be

		_, err := envcfg.EnvLookup(path)

		require.Error(t, err)
		assert.Contains(t, err.Error(), path)
		assert.NotContains(t, err.Error(), "syntax")
	})
}
