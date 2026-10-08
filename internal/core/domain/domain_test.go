package domain_test

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

func TestStatus_Terminal(t *testing.T) {
	tests := []struct {
		status domain.Status
		want   bool
	}{
		{domain.StatusPending, false},
		{domain.StatusStreaming, false},
		{domain.StatusComplete, true},
		{domain.StatusStopped, true},
		{domain.StatusFailed, true},
		{"", false},
		{"thinking", false},
	}
	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			assert.Equal(t, tc.want, tc.status.Terminal())
		})
	}
}

func TestTitleFrom(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"short message is used as is", "What is a goroutine?", "What is a goroutine?"},
		{"surrounding space is trimmed", "  hello  ", "hello"},
		{"newlines and runs of space collapse", "line one\n\n  line   two\tend", "line one line two end"},
		{"exactly 60 characters is kept whole", strings.Repeat("a", 60), strings.Repeat("a", 60)},
		{"61 characters is cut with an ellipsis", strings.Repeat("a", 61), strings.Repeat("a", 60) + "…"},
		{"cuts on characters, not bytes", strings.Repeat("ñ", 70), strings.Repeat("ñ", 60) + "…"},
		{"trailing space before the cut is dropped", strings.Repeat("a", 59) + " bbbbbb", strings.Repeat("a", 59) + "…"},
		{"empty content gets the default", "", "New chat"},
		{"whitespace only gets the default", " \n\t ", "New chat"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, domain.TitleFrom(tc.content))
		})
	}
}

func TestNormalizeTitle(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"trimmed", "  My chat  ", "My chat", false},
		{"inner newlines collapse", "My\nchat", "My chat", false},
		{"exactly 120 characters", strings.Repeat("x", 120), strings.Repeat("x", 120), false},
		{"121 characters is too long", strings.Repeat("x", 121), "", true},
		{"120 multibyte characters fit", strings.Repeat("é", 120), strings.Repeat("é", 120), false},
		{"empty", "", "", true},
		{"whitespace only", "   ", "", true},
		{"a NUL byte, which the database refuses", "My\x00chat", "", true},
		{"bytes that are not UTF-8", "My \xff\xfe chat", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.NormalizeTitle(tc.raw)
			if tc.wantErr {
				var ve *domain.ValidationError
				require.ErrorAs(t, err, &ve)
				assert.Equal(t, "title", ve.Field)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNormalizeContent(t *testing.T) {
	const limit = 10
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr error
	}{
		{"trimmed", "  hi there \n", "hi there", nil},
		{"inner newlines and markdown are preserved", "# T\n\n- a", "# T\n\n- a", nil},
		{"exactly at the limit", "0123456789", "0123456789", nil},
		{"one over the limit", "0123456789a", "", domain.ErrMessageTooLong},
		{"the limit counts characters, not bytes", strings.Repeat("é", limit), strings.Repeat("é", limit), nil},
		{"one multibyte character over", strings.Repeat("é", limit+1), "", domain.ErrMessageTooLong},
		{"empty", "", "", domain.ErrEmptyMessage},
		{"whitespace only", " \n\t ", "", domain.ErrEmptyMessage},
		{"a NUL byte, which the database refuses", "hi\x00there", "", domain.ErrMessageInvalid},
		{"only a NUL byte", "\x00", "", domain.ErrMessageInvalid},
		{"bytes that are not UTF-8", "hi \xff\xfe", "", domain.ErrMessageInvalid},
		{"text from other scripts and emoji is fine", "héllo 你好 🙂", "héllo 你好 🙂", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.NormalizeContent(tc.raw, limit)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("the too-long message names the limit", func(t *testing.T) {
		_, err := domain.NormalizeContent("0123456789a", limit)
		ve, ok := errors.AsType[*domain.ValidationError](err)
		require.True(t, ok)
		assert.Contains(t, ve.Message, "10")
		assert.Equal(t, "content", ve.Field)
	})
}

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"lowercased and trimmed", "  Alice@Example.COM ", "alice@example.com", false},
		{"plus addressing is kept", "a+b@example.com", "a+b@example.com", false},
		{"empty", "", "", true},
		{"no at sign", "alice.example.com", "", true},
		{"two at signs", "a@b@example.com", "", true},
		{"display name form is refused", "Alice <alice@example.com>", "", true},
		{"missing domain", "alice@", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.NormalizeEmail(tc.raw)
			if tc.wantErr {
				var ve *domain.ValidationError
				require.ErrorAs(t, err, &ve)
				assert.Equal(t, "email", ve.Field)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestValidationError(t *testing.T) {
	t.Run("matches its sentinel by code even when the message differs", func(t *testing.T) {
		err := domain.TooLong(500)
		assert.ErrorIs(t, err, domain.ErrMessageTooLong)
		assert.NotErrorIs(t, err, domain.ErrEmptyMessage)
	})
	t.Run("survives wrapping", func(t *testing.T) {
		wrapped := errors.Join(errors.New("context"), domain.ErrEmptyMessage)
		assert.ErrorIs(t, wrapped, domain.ErrEmptyMessage)
		_, ok := errors.AsType[*domain.ValidationError](wrapped)
		assert.True(t, ok)
	})
	t.Run("the error text is the user-facing message", func(t *testing.T) {
		assert.Equal(t, domain.ErrEmptyMessage.Message, domain.ErrEmptyMessage.Error())
	})
}

func TestSentinelErrorsAreDistinct(t *testing.T) {
	all := []error{
		domain.ErrNotFound, domain.ErrInvalidCredentials, domain.ErrAlreadyStreaming,
		domain.ErrInvalidState, domain.ErrAgentUnavailable, domain.ErrEmailTaken, domain.ErrBusy,
		domain.ErrEmptyMessage, domain.ErrMessageTooLong,
	}
	for i, a := range all {
		for j, b := range all {
			assert.Equal(t, i == j, errors.Is(a, b), "%v vs %v", a, b)
		}
	}
}

func TestMetaJSON(t *testing.T) {
	tests := []struct {
		name string
		meta domain.Meta
		want string
	}{
		{"unknown values are omitted", domain.Meta{}, `{}`},
		{
			"known values are kept",
			domain.Meta{TTFTMs: 120, DurationMs: 3400, Usage: domain.Usage{Prompt: 10, Completion: 25}, ToolCalls: []string{"search", "calc"}},
			`{"ttft_ms":120,"duration_ms":3400,"usage":{"prompt":10,"completion":25},"tool_calls":["search","calc"]}`,
		},
		{"partial usage", domain.Meta{Usage: domain.Usage{Completion: 7}}, `{"usage":{"completion":7}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.meta)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(got))

			var back domain.Meta
			require.NoError(t, json.Unmarshal(got, &back))
			assert.Equal(t, tc.meta, back)
		})
	}

	t.Run("unknown members in stored JSON are ignored", func(t *testing.T) {
		var m domain.Meta
		require.NoError(t, json.Unmarshal([]byte(`{"ttft_ms":5,"future_field":true}`), &m))
		assert.Equal(t, int64(5), m.TTFTMs)
	})
}
