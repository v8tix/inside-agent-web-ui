package testagent_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
)

func userRequest(text string) *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText(text, genai.RoleUser)}}
}

func collect(t *testing.T, m *testagent.Model, req *model.LLMRequest, stream bool) ([]*model.LLMResponse, error) {
	t.Helper()
	var out []*model.LLMResponse
	for resp, err := range m.GenerateContent(t.Context(), req, stream) {
		if err != nil {
			return out, err
		}
		out = append(out, resp)
	}
	return out, nil
}

func textOf(resp *model.LLMResponse, thought bool) string {
	var sb strings.Builder
	if resp.Content == nil {
		return ""
	}
	for _, p := range resp.Content.Parts {
		if p.Thought == thought {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

func TestModel_StreamingShape(t *testing.T) {
	tests := []struct {
		name         string
		prompt       string
		wantText     string
		wantThought  string
		wantInText   []string
		minPartials  int
		wantUsageSet bool
	}{
		{
			name:         "echo streams word by word",
			prompt:       "hello brave new world",
			wantText:     "You said: hello brave new world",
			minPartials:  4,
			wantUsageSet: true,
		},
		{
			name:        "think puts reasoning before the answer",
			prompt:      "#think what now",
			wantText:    "You said: #think what now",
			wantThought: "Let me think about that.",
			minPartials: 3,
		},
		{
			name:       "markdown reply has headings, lists, a table and a code block",
			prompt:     "#markdown",
			wantInText: []string{"# ", "- first", "| Name |", "```go", "[link](https://example.com)"},
		},
		{
			name:       "html reply carries hostile payloads for sanitizer tests",
			prompt:     "#html",
			wantInText: []string{"<script>alert(1)</script>", "onerror=", "javascript:alert(1)", "<iframe", "![tracker](https://evil.example"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := testagent.NewModel(testagent.WithWordDelay(0))

			resps, err := collect(t, m, userRequest(tc.prompt), true)

			require.NoError(t, err)
			require.GreaterOrEqual(t, len(resps), 2, "at least one partial and a final")
			final := resps[len(resps)-1]
			assert.False(t, final.Partial, "the last response is the aggregated, non-partial one")
			assert.True(t, final.TurnComplete)

			var partialText, partialThought strings.Builder
			partials := 0
			for _, r := range resps[:len(resps)-1] {
				assert.True(t, r.Partial)
				partials++
				partialText.WriteString(textOf(r, false))
				partialThought.WriteString(textOf(r, true))
			}
			assert.GreaterOrEqual(t, partials, tc.minPartials)
			assert.Equal(t, textOf(final, false), partialText.String(), "partials add up to the final text")
			assert.Equal(t, tc.wantThought, textOf(final, true))
			assert.Equal(t, tc.wantThought, partialThought.String())
			if tc.wantText != "" {
				assert.Equal(t, tc.wantText, textOf(final, false))
			}
			for _, want := range tc.wantInText {
				assert.Contains(t, textOf(final, false), want)
			}
			if tc.wantUsageSet {
				require.NotNil(t, final.UsageMetadata)
				assert.Positive(t, final.UsageMetadata.CandidatesTokenCount)
				assert.Positive(t, final.UsageMetadata.PromptTokenCount)
			}
		})
	}
}

// scriptOf names the keyword whose script gave an answer, or says that the message
// was only said back.
func scriptOf(resps []*model.LLMResponse, err error) string {
	switch {
	case err != nil && strings.Contains(err.Error(), "#error"):
		return "#error"
	case err != nil:
		return "another error: " + err.Error()
	case len(resps) == 0 || resps[len(resps)-1].Content == nil || len(resps[len(resps)-1].Content.Parts) == 0:
		return "no answer"
	}
	last := resps[len(resps)-1]
	text := textOf(last, false)
	switch {
	case last.Content.Parts[0].FunctionCall != nil:
		return "#tool"
	case textOf(last, true) != "":
		return "#think"
	case strings.HasPrefix(text, "tick 1 "):
		return "#slow"
	case strings.HasPrefix(text, "# Report"):
		return "#markdown"
	case strings.Contains(text, "<script>"):
		return "#html"
	}
	return "said back"
}

// A message that holds two keywords gets the script of the one that comes first
// in the model's list, wherever the two stand in the message.
func TestModel_TheFirstKeywordOfTheListWins(t *testing.T) {
	keywords := []string{"#error", "#slow", "#tool", "#think", "#markdown", "#html"} // in the order the model tries them
	m := testagent.NewModel(testagent.WithWordDelay(0), testagent.WithSlow(0, 3))
	ask := func(t *testing.T, prompt string) string {
		t.Helper()
		return scriptOf(collect(t, m, userRequest(prompt), false))
	}

	for i, first := range keywords {
		t.Run(first+" alone", func(t *testing.T) {
			assert.Equal(t, first, ask(t, "please "+first+" now"))
		})
		for _, later := range keywords[i+1:] {
			t.Run(first+" before "+later, func(t *testing.T) {
				assert.Equal(t, first, ask(t, first+" and "+later))
				assert.Equal(t, first, ask(t, later+" and "+first), "the list decides, not the place in the message")
			})
		}
	}
	t.Run("no keyword at all", func(t *testing.T) {
		assert.Equal(t, "said back", ask(t, "nothing special"))
	})
}

func TestModel_Name(t *testing.T) {
	assert.Equal(t, "scripted", testagent.NewModel().Name(), "the name ADK is given for the model")
}

// The probe counts a run that stopped early because its caller went away: once,
// however the caller left. A call without streaming gives its one answer whole, so
// there is nothing to leave.
func TestModel_CountsARunTheCallerLeft(t *testing.T) {
	ended := func(t *testing.T) context.Context {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return ctx
	}
	// lastError reads a stream to its end and returns the error it ended with.
	lastError := func(ctx context.Context, m *testagent.Model, prompt string, afterEach func()) error {
		var last error
		for _, err := range m.GenerateContent(ctx, userRequest(prompt), true) {
			last = err
			afterEach()
		}
		return last
	}
	tests := []struct {
		name  string
		leave func(t *testing.T, m *testagent.Model)
		want  int64
	}{
		{"a stream read to its end", func(t *testing.T, m *testagent.Model) {
			require.NoError(t, lastError(t.Context(), m, "one two three", func() {}))
		}, 0},
		{"the caller stops reading a stream", func(t *testing.T, m *testagent.Model) {
			for _, err := range m.GenerateContent(t.Context(), userRequest("one two three"), true) {
				require.NoError(t, err)
				break
			}
		}, 1},
		{"the context had ended before the first piece", func(t *testing.T, m *testagent.Model) {
			require.ErrorIs(t, lastError(ended(t), m, "one two three", func() {}), context.Canceled)
		}, 1},
		{"the context ends between two pieces", func(t *testing.T, m *testagent.Model) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			require.ErrorIs(t, lastError(ctx, m, "one two three", cancel), context.Canceled)
		}, 1},
		{"the context had ended before the pause that comes first", func(t *testing.T, m *testagent.Model) {
			require.ErrorIs(t, lastError(ended(t), m, "[syn-03]", func() {}), context.Canceled) // five seconds before its first word
		}, 1},
		{"a call without streaming, left at its one answer", func(t *testing.T, m *testagent.Model) {
			for _, err := range m.GenerateContent(t.Context(), userRequest("one two three"), false) {
				require.NoError(t, err)
				break
			}
		}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := fixtureModel(t)

			tc.leave(t, m)

			assert.Equal(t, tc.want, m.Probe().Cancelled())
			assert.Equal(t, int64(1), m.Probe().Runs(), "and it is one run either way")
		})
	}
}

func TestModel_NonStreamingIsOneResponse(t *testing.T) {
	m := testagent.NewModel()

	resps, err := collect(t, m, userRequest("hello there"), false)

	require.NoError(t, err)
	require.Len(t, resps, 1)
	assert.False(t, resps[0].Partial)
	assert.Equal(t, "You said: hello there", textOf(resps[0], false))
}

func TestModel_Error(t *testing.T) {
	m := testagent.NewModel()

	_, err := collect(t, m, userRequest("please #error now"), true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "scripted failure")
}

func TestModel_ToolRoundTrip(t *testing.T) {
	m := testagent.NewModel(testagent.WithWordDelay(0))

	first, err := collect(t, m, userRequest("#tool find the answer"), true)
	require.NoError(t, err)
	require.Len(t, first, 1, "a tool call is a single, non-partial response")
	call := first[0].Content.Parts[0].FunctionCall
	require.NotNil(t, call)
	assert.Equal(t, "lookup", call.Name)
	assert.Equal(t, "#tool find the answer", call.Args["query"])

	req := userRequest("#tool find the answer")
	req.Contents = append(req.Contents,
		&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: call}}},
		&genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
			Name: "lookup", Response: map[string]any{"result": "forty-two"},
		}}}},
	)
	second, err := collect(t, m, req, true)
	require.NoError(t, err)
	assert.Contains(t, textOf(second[len(second)-1], false), "forty-two")
}

func TestModel_SlowStopsWhenCanceled(t *testing.T) {
	t.Run("runs to the end when left alone", func(t *testing.T) {
		m := testagent.NewModel(testagent.WithSlow(time.Millisecond, 5))

		resps, err := collect(t, m, userRequest("#slow"), true)

		require.NoError(t, err)
		assert.Len(t, resps, 6, "5 chunks and the aggregated final")
		assert.Equal(t, int64(1), m.Probe().Runs())
		assert.Zero(t, m.Probe().Cancelled())
	})

	t.Run("stops promptly and records the cancellation", func(t *testing.T) {
		m := testagent.NewModel(testagent.WithSlow(50*time.Millisecond, 1000))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		seen := 0

		for _, err := range m.GenerateContent(ctx, userRequest("#slow"), true) {
			if err != nil {
				assert.True(t, errors.Is(err, context.Canceled), "got %v", err)
				break
			}
			seen++
			if seen == 2 {
				cancel()
			}
		}

		assert.Less(t, seen, 10)
		assert.Equal(t, int64(1), m.Probe().Cancelled())
	})
}

func TestModel_UsesTheLastUserText(t *testing.T) {
	m := testagent.NewModel(testagent.WithWordDelay(0))
	req := &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("first question", genai.RoleUser),
		genai.NewContentFromText("an answer", genai.RoleModel),
		genai.NewContentFromText("second question", genai.RoleUser),
	}}

	resps, err := collect(t, m, req, false)

	require.NoError(t, err)
	assert.Equal(t, "You said: second question", textOf(resps[0], false))
}

func contextWithTimeout(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(t.Context(), d)
}
