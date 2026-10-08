package secret

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const value = "correct-horse-canary"

// settings is what a program holds: a secret in an exported field.
type settings struct {
	APIKey Secret `json:"api_key"`
}

// verb builds a format at run time, so that go vet does not judge the verb.
func verb(letter string) string { return "%" + letter }

func TestNoFormatLogOrEncoderShowsTheValue(t *testing.T) {
	s := Secret(value)
	var text, structured bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("loaded", "key", s, "settings", settings{s})
	slog.New(slog.NewJSONHandler(&structured, nil)).Info("loaded", "key", s, "settings", settings{s})
	encoded, err := json.Marshal(settings{s})
	if err != nil {
		t.Fatal(err)
	}

	shown := map[string]string{
		"%v":           fmt.Sprintf("%v", s),
		"%s":           fmt.Sprintf("%s", s),
		"%#v":          fmt.Sprintf("%#v", s),
		"%+v, struct":  fmt.Sprintf("%+v", settings{s}),
		"%#v, struct":  fmt.Sprintf("%#v", settings{s}),
		"an error":     fmt.Errorf("calling the API with %s", s).Error(),
		"slog text":    text.String(),
		"slog JSON":    structured.String(),
		"json.Marshal": string(encoded),
	}
	// A verb that is not for text is answered by Format. Without that method
	// package fmt complains about the verb, and the complaint quotes the value.
	for _, letter := range []string{"d", "x", "q", "t", "e", "U", "y"} {
		shown["%"+letter] = fmt.Sprintf(verb(letter), s)
		shown["%"+letter+", struct"] = fmt.Sprintf(verb(letter), settings{s})
	}
	for where, got := range shown {
		if strings.Contains(got, value) {
			t.Errorf("%s shows the value: %s", where, got)
		}
	}
	if got := string(encoded); got != `{"api_key":"[redacted]"}` {
		t.Errorf("json.Marshal wrote %s", got)
	}
	for letter, want := range map[string]string{"d": "[redacted]", "q": `"[redacted]"`, "#v": `"[redacted]"`} {
		if got := fmt.Sprintf(verb(letter), s); got != want {
			t.Errorf("%%%s printed %s, want %s", letter, got, want)
		}
	}
}

func TestRevealGivesTheValueToTheCodeThatNeedsIt(t *testing.T) {
	if got := Secret(value).Reveal(); got != value {
		t.Errorf("Reveal() = %q", got)
	}
}

// The first limit of the type in Go: package fmt cannot call the methods of a
// field that is not exported, so it prints the string underneath.
func TestAFieldThatIsNotExportedStillShowsTheValue(t *testing.T) {
	type private struct{ apiKey Secret }

	got := fmt.Sprintf("%v", private{Secret(value)})

	if !strings.Contains(got, value) {
		t.Errorf("fmt now hides the field too, so this limit is gone: %s", got)
	}
}

// The second limit: package fmt answers the pointer verb and the error verb
// itself, before it asks the value how it prints.
func TestThePointerVerbAndTheErrorVerbStillShowTheValue(t *testing.T) {
	for _, letter := range []string{"p", "w"} {
		got := fmt.Sprintf(verb(letter), Secret(value))

		if !strings.Contains(got, value) {
			t.Errorf("%%%s now hides the value, so this limit is gone: %s", letter, got)
		}
	}
}
