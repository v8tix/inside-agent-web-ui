package testagent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
)

const goodFixture = `{
  "version": 1,
  "replies": {
    "syn-01": {"text": "The answer is 42.\nFINAL ANSWER: 42"},
    "syn-02": {"error": "the agent fell over"},
    "syn-03": {"text": "late reply", "first_delay_ms": 5000, "word_delay_ms": 1000},
    "syn-04": {"text": "after a tool call", "tool": "lookup"},
    "syn-05": {"text": "second time lucky", "fail_first": 2, "error": "agent is down"},
    "syn-06": {"text": "second time lucky too", "fail_first": 1}
  }
}`

func fixtureModel(t *testing.T, opts ...testagent.Option) *testagent.Model {
	t.Helper()
	f, err := testagent.ParseFixture([]byte(goodFixture))
	require.NoError(t, err)
	return testagent.NewModel(append([]testagent.Option{testagent.WithFixture(f), testagent.WithWordDelay(0)}, opts...)...)
}

func finalText(t *testing.T, resps []*model.LLMResponse) string {
	t.Helper()
	require.NotEmpty(t, resps)
	return textOf(resps[len(resps)-1], false)
}

func TestParseFixture(t *testing.T) {
	t.Run("a good fixture", func(t *testing.T) {
		f, err := testagent.ParseFixture([]byte(goodFixture))

		require.NoError(t, err)
		assert.Equal(t, 1, f.Version)
		assert.Len(t, f.Replies, 6)
		assert.Equal(t, "lookup", f.Replies["syn-04"].Tool)
		require.NotNil(t, f.Replies["syn-03"].WordDelayMs)
		assert.Equal(t, 1000, *f.Replies["syn-03"].WordDelayMs)
	})

	bad := []struct {
		name string
		json string
		want string
	}{
		{"not JSON", `{nope`, "reading the fixture"},
		{"another version", `{"version":2,"replies":{"a":{"text":"x"}}}`, "version 2 is not supported"},
		{"no version", `{"replies":{"a":{"text":"x"}}}`, "version 0 is not supported"},
		{"no replies", `{"version":1,"replies":{}}`, "no replies"},
		{"a member that is not known", `{"version":1,"replies":{"a":{"text":"x","wordDelayMs":5}}}`, "reading the fixture"},
		{"a top-level member that is not known", `{"version":1,"replies":{"a":{"text":"x"}},"extra":1}`, "reading the fixture"},
		{"text and error together", `{"version":1,"replies":{"a":{"text":"x","error":"y"}}}`, `"a": give either text or error`},
		{"neither text nor error", `{"version":1,"replies":{"a":{}}}`, `"a": give either text or error`},
		{"a negative delay", `{"version":1,"replies":{"a":{"text":"x","first_delay_ms":-1}}}`, "cannot be negative"},
		{"a negative word delay", `{"version":1,"replies":{"a":{"text":"x","word_delay_ms":-5}}}`, "cannot be negative"},
		{"a negative fail_first", `{"version":1,"replies":{"a":{"text":"x","fail_first":-1}}}`, "cannot be negative"},
		{"a tool the agent does not have", `{"version":1,"replies":{"a":{"text":"x","tool":"search"}}}`, `no tool "search"`},
		{"fail_first without an answer to give later", `{"version":1,"replies":{"a":{"error":"x","fail_first":1}}}`, "needs a text"},
		{"an empty id", `{"version":1,"replies":{"":{"text":"x"}}}`, "must not be empty"},
		{"an id with a space", `{"version":1,"replies":{"a b":{"text":"x"}}}`, "must not be empty or hold whitespace"},
		{"an id inside another", `{"version":1,"replies":{"syn-1":{"text":"x"},"syn-10":{"text":"y"}}}`, `"syn-1" is part of id "syn-10"`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			f, err := testagent.ParseFixture([]byte(tc.json))

			require.Error(t, err)
			assert.Nil(t, f)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestLoadFixture(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	require.NoError(t, os.WriteFile(good, []byte(goodFixture), 0o600))
	broken := filepath.Join(dir, "broken.json")
	require.NoError(t, os.WriteFile(broken, []byte(`{"version":2}`), 0o600))

	f, err := testagent.LoadFixture(good)
	require.NoError(t, err)
	assert.Len(t, f.Replies, 6)

	_, err = testagent.LoadFixture(filepath.Join(dir, "missing.json"))
	require.ErrorContains(t, err, "reading the fixture")

	_, err = testagent.LoadFixture(broken)
	require.ErrorContains(t, err, "broken.json")
	assert.ErrorContains(t, err, "version 2")
}

// A question that names two problems still gets one reply, and the same one every
// time: the longest id wins, and of two ids of one length the one that comes first
// in the question. Each question is asked many times, because a choice left to the
// order of a map comes out right most of the time.
func TestFixture_TwoIdsOfOneLengthAlwaysGiveTheSameReply(t *testing.T) {
	f, err := testagent.ParseFixture([]byte(`{"version":1,"replies":{
		"aaa":{"text":"the reply of aaa"},
		"bbb":{"text":"the reply of bbb"},
		"cccc":{"text":"the reply of cccc"}
	}}`))
	require.NoError(t, err)
	m := testagent.NewModel(testagent.WithFixture(f), testagent.WithWordDelay(0))
	tests := []struct {
		name     string
		question string
		want     string
	}{
		{"the first of the two", "bbb then aaa", "the reply of bbb"},
		{"whichever it is", "aaa then bbb", "the reply of aaa"},
		{"a longer id wins wherever it stands", "aaa then bbb then cccc", "the reply of cccc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for range 200 {
				resps, err := collect(t, m, userRequest(tc.question), false)

				require.NoError(t, err)
				require.Equal(t, tc.want, finalText(t, resps))
			}
		})
	}
}

func TestModel_Fixture(t *testing.T) {
	t.Run("answers the problem named in a prompt wrapped around the question", func(t *testing.T) {
		m := fixtureModel(t)
		prompt := "You are a general AI assistant. Report your thoughts, and finish with FINAL ANSWER: [YOUR FINAL ANSWER].\n\nQuestion: What is 6 times 7? [syn-01]"

		resps, err := collect(t, m, userRequest(prompt), true)

		require.NoError(t, err)
		assert.Equal(t, "The answer is 42.\nFINAL ANSWER: 42", finalText(t, resps))
		assert.Greater(t, len(resps), 3, "streamed in pieces")
		last := resps[len(resps)-1]
		require.NotNil(t, last.UsageMetadata, "with token counts, like a real model")
		assert.Positive(t, last.UsageMetadata.PromptTokenCount)
	})
	t.Run("an agent error", func(t *testing.T) {
		_, err := collect(t, fixtureModel(t), userRequest("Question: anything [syn-02]"), true)

		require.ErrorContains(t, err, "the agent fell over")
	})
	t.Run("the fixture comes before the keyword scripts", func(t *testing.T) {
		resps, err := collect(t, fixtureModel(t), userRequest("#error and #slow but [syn-01]"), true)

		require.NoError(t, err)
		assert.Contains(t, finalText(t, resps), "FINAL ANSWER: 42")
	})
	t.Run("what the fixture does not name falls through to the scripts", func(t *testing.T) {
		m := fixtureModel(t)

		resps, err := collect(t, m, userRequest("#markdown please"), true)
		require.NoError(t, err)
		assert.Contains(t, finalText(t, resps), "# Report")

		resps, err = collect(t, m, userRequest("hello there"), true)
		require.NoError(t, err)
		assert.Equal(t, "You said: hello there", finalText(t, resps))
	})
	t.Run("a pause before the first word and a pace of its own", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			var firstAt time.Duration
			count := 0

			for resp, err := range fixtureModel(t, testagent.WithWordDelay(10*time.Millisecond)).GenerateContent(t.Context(), userRequest("[syn-03]"), true) {
				require.NoError(t, err)
				if resp.Partial {
					if count == 0 {
						firstAt = time.Since(start)
					}
					count++
				}
			}

			assert.Equal(t, 5*time.Second, firstAt, "the first word waits for first_delay_ms")
			assert.Equal(t, 2, count, "late reply: two words")
			assert.Equal(t, 5*time.Second+time.Second, time.Since(start), "and the second follows after word_delay_ms, not the model's 10 ms")
		})
	})
	t.Run("cancelling during the first pause ends the run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			m := fixtureModel(t)
			ctx, cancel := contextWithTimeout(t, time.Second)
			defer cancel()

			var gotErr error
			for _, err := range m.GenerateContent(ctx, userRequest("[syn-03]"), true) {
				gotErr = err
			}

			require.Error(t, gotErr)
			assert.Equal(t, int64(1), m.Probe().Cancelled(), "the agent noticed that the caller went away")
		})
	})
	t.Run("a tool is called first, then the text comes", func(t *testing.T) {
		m := fixtureModel(t)

		first, err := collect(t, m, userRequest("Question: needs a lookup [syn-04]"), true)
		require.NoError(t, err)
		require.Len(t, first, 1)
		call := first[0].Content.Parts[0].FunctionCall
		require.NotNil(t, call)
		assert.Equal(t, "lookup", call.Name)
		assert.Equal(t, "syn-04", call.Args["query"])

		req := userRequest("Question: needs a lookup [syn-04]")
		req.Contents = append(req.Contents,
			&genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: call}}},
			&genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
				Name: "lookup", Response: map[string]any{"result": "forty-two"},
			}}}},
		)
		second, err := collect(t, m, req, true)
		require.NoError(t, err)
		assert.Equal(t, "after a tool call", finalText(t, second), "the fixture's text, not the generic lookup reply")
	})
	t.Run("a run that fails a number of times before it answers", func(t *testing.T) {
		m := fixtureModel(t)
		ask := func(id string) (string, error) {
			resps, err := collect(t, m, userRequest("["+id+"]"), true)
			if err != nil {
				return "", err
			}
			return finalText(t, resps), nil
		}

		_, err := ask("syn-05")
		require.ErrorContains(t, err, "agent is down")
		_, err = ask("syn-05")
		require.ErrorContains(t, err, "agent is down")
		text, err := ask("syn-05")
		require.NoError(t, err)
		assert.Equal(t, "second time lucky", text)
		text, err = ask("syn-05")
		require.NoError(t, err)
		assert.Equal(t, "second time lucky", text, "and it keeps answering")

		_, err = ask("syn-06")
		require.ErrorContains(t, err, "scripted failure", "the default message")
		text, err = ask("syn-06")
		require.NoError(t, err, "the count is kept for each problem")
		assert.Equal(t, "second time lucky too", text)
	})
	t.Run("failures are counted for each question, so a new wording fails afresh", func(t *testing.T) {
		m := fixtureModel(t)
		ask := func(prompt string) error {
			_, err := collect(t, m, userRequest(prompt), true)
			return err
		}

		require.Error(t, ask("Are you back? [syn-05] #a1b2"))
		require.Error(t, ask("Are you back? [syn-05] #a1b2"))
		require.NoError(t, ask("Are you back? [syn-05] #a1b2"), "the third time of asking")
		require.Error(t, ask("Are you back? [syn-05] #c3d4"), "another question, however long the agent has been up")
		require.Error(t, ask("Are you back? [syn-05] #c3d4"))
		require.NoError(t, ask("Are you back? [syn-05] #c3d4"))
		require.NoError(t, ask("Are you back? [syn-05] #a1b2"), "and the first one still answers")
	})
	t.Run("without streaming, the whole reply at once", func(t *testing.T) {
		resps, err := collect(t, fixtureModel(t), userRequest("[syn-01]"), false)

		require.NoError(t, err)
		require.Len(t, resps, 1)
		assert.True(t, strings.HasSuffix(textOf(resps[0], false), "FINAL ANSWER: 42"))
	})
}
