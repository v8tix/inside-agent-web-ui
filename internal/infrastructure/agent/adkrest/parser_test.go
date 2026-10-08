package adkrest

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return string(data)
}

func parse(t *testing.T, stream string) ([]domain.AgentEvent, error) {
	t.Helper()
	var out []domain.AgentEvent
	for ev, err := range parseStream(strings.NewReader(stream)) {
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
	return out, nil
}

func kinds(evs []domain.AgentEvent) []domain.AgentEventKind {
	out := make([]domain.AgentEventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func texts(evs []domain.AgentEvent, kind domain.AgentEventKind) []string {
	var out []string
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e.Text)
		}
	}
	return out
}

// frameFor builds a JSON event frame the way ADK writes them.
func frameFor(json string) string { return "data: " + json + "\n\n" }

const finalHello = `{"turnComplete":true,"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8},"content":{"role":"model","parts":[{"text":"hello"}]}}`

func TestParseStream_RecordedFixtures(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		wantDeltas []string
		wantFinal  string
		wantUsage  *domain.Usage
		wantTools  []string
		wantErr    string
		prefixOnly []domain.AgentEventKind
	}{
		{
			name:       "echo streams deltas then one final",
			file:       "echo.sse",
			wantDeltas: []string{"You ", "said: ", "hello ", "brave ", "world"},
			wantFinal:  "You said: hello brave world",
			wantUsage:  &domain.Usage{Prompt: 3, Completion: 5},
		},
		{
			name:       "reasoning parts are skipped",
			file:       "think.sse",
			wantDeltas: []string{"You ", "said: ", "#think ", "what ", "now"},
			wantFinal:  "You said: #think what now",
			wantUsage:  &domain.Usage{Prompt: 3, Completion: 5},
		},
		{
			name:       "a tool call is reported by name and the answer follows",
			file:       "tool.sse",
			wantDeltas: []string{"The ", "lookup ", "returned: ", "forty-two"},
			wantFinal:  "The lookup returned: forty-two",
			wantTools:  []string{"lookup"},
			wantUsage:  &domain.Usage{Prompt: 4, Completion: 4},
		},
		{
			name:    "a named error event becomes an agent error",
			file:    "error.sse",
			wantErr: "scripted failure",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evs, err := parse(t, readFixture(t, tc.file))

			if tc.wantErr != "" {
				var ae *domain.AgentError
				require.ErrorAs(t, err, &ae)
				assert.Contains(t, ae.Detail, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantDeltas, texts(evs, domain.AgentDelta))
			assert.Equal(t, []string{tc.wantFinal}, texts(evs, domain.AgentFinal))

			var tools []string
			var usage *domain.Usage
			for _, e := range evs {
				switch e.Kind {
				case domain.AgentToolCall:
					tools = append(tools, e.Tool)
				case domain.AgentUsage:
					usage = &e.Usage
				}
			}
			assert.Equal(t, tc.wantTools, tools)
			assert.Equal(t, tc.wantUsage, usage)
		})
	}
}

func TestParseStream_LargeAndSpecialContent(t *testing.T) {
	t.Run("markdown keeps its newlines, backticks and quotes", func(t *testing.T) {
		evs, err := parse(t, readFixture(t, "markdown.sse"))

		require.NoError(t, err)
		final := texts(evs, domain.AgentFinal)
		require.Len(t, final, 1)
		assert.Contains(t, final[0], "```go\nfmt.Println(\"hello\")\n```")
		assert.Contains(t, final[0], "| Name | Value |")
		assert.Equal(t, final[0], strings.Join(texts(evs, domain.AgentDelta), ""), "deltas add up to the final text")
	})

	t.Run("a long run of deltas parses completely", func(t *testing.T) {
		evs, err := parse(t, readFixture(t, "slow.sse"))

		require.NoError(t, err)
		assert.Len(t, texts(evs, domain.AgentDelta), 50)
		assert.Len(t, texts(evs, domain.AgentFinal), 1)
	})

	t.Run("a frame larger than the default scanner buffer parses", func(t *testing.T) {
		big := strings.Repeat("x", 300_000)
		stream := frameFor(`{"content":{"role":"model","parts":[{"text":"` + big + `"}]}}`)

		evs, err := parse(t, stream)

		require.NoError(t, err)
		require.Len(t, evs, 1)
		assert.Equal(t, domain.AgentFinal, evs[0].Kind)
		assert.Len(t, evs[0].Text, 300_000)
	})

	t.Run("a frame over the size cap is an error, not unbounded memory", func(t *testing.T) {
		stream := "data: " + strings.Repeat("x", maxFrameBytes+1) + "\n\n"

		_, err := parse(t, stream)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "too large")
	})

	// The cap is on the frame, whatever its lines are.
	for _, tc := range []struct{ name, prefix string }{
		{"many small data lines", "data: "},
		{"many comment lines", ": "},
	} {
		t.Run(tc.name+" add up to the size cap", func(t *testing.T) {
			line := tc.prefix + strings.Repeat("x", 100_000) + "\n"
			stream := strings.Repeat(line, maxFrameBytes/100_000+10) + "\n"

			_, err := parse(t, stream)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "too large")
		})
	}
}

// The cap is on one frame: a long stream of frames, each well inside it, is fine.
func TestReadFrames_TheCapStartsAgainWithEachFrame(t *testing.T) {
	frames := maxFrameBytes/100_000 + 10
	stream := strings.Repeat("data: "+strings.Repeat("x", 100_000)+"\n\n", frames)

	count := 0
	for _, err := range readFrames(strings.NewReader(stream)) {
		require.NoError(t, err)
		count++
	}

	assert.Equal(t, frames, count, "the frames add up to more than the cap, and every one arrives")
}

func TestReadFrames(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   []frame
	}{
		{"two frames", "data: a\n\ndata: b\n\n", []frame{{data: "a"}, {data: "b"}}},
		{"an empty stream", "", nil},
		{"comments alone make no frame", ": a\n\n: b\n\n", nil},
		{"blank lines and comments between frames make none", "\n\n: hi\n\ndata: a\n\n\n", []frame{{data: "a"}}},
		{"an event with no data is a frame", "event: error\n\n", []frame{{event: "error"}}},
		{"the event and the data of one frame", "event: error\ndata: boom\n\n", []frame{{event: "error", data: "boom"}}},
		{"data lines are joined with a newline", "data: a\ndata: b\n\n", []frame{{data: "a\nb"}}},
		{"an empty data line is data", "data:\n\n", []frame{{}}},
		{"a field with no colon is that field with an empty value", "data\n\n", []frame{{}}},
		{"one space after the colon is dropped, any other kept", "data:  two spaces  \n\n", []frame{{data: " two spaces  "}}},
		{"other fields are ignored", "id: 7\nretry: 100\nwhat: ever\ndata: a\n\n", []frame{{data: "a"}}},
		{"the next frame starts clean", "event: error\ndata: x\n\ndata: y\n\n", []frame{{event: "error", data: "x"}, {data: "y"}}},
		{"a frame that has no blank line after it is dropped", "data: a\n\ndata: b\n", []frame{{data: "a"}}},
		{"and so is one that stops in the middle of a line", "data: a\n\ndata: b", []frame{{data: "a"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []frame
			for fr, err := range readFrames(strings.NewReader(tc.stream)) {
				require.NoError(t, err)
				got = append(got, fr)
			}

			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseStream_Truncation(t *testing.T) {
	echo := readFixture(t, "echo.sse")
	frames := strings.Split(strings.TrimSpace(echo), "\n\n")
	require.Len(t, frames, 6)

	tests := []struct {
		name       string
		stream     string
		wantDeltas int
	}{
		{"final frame never arrives", strings.Join(frames[:5], "\n\n") + "\n\n", 5},
		{"stream ends mid-frame", strings.Join(frames[:3], "\n\n") + "\n\ndata: {\"partial\":tr", 3},
		{"stream ends before the blank line that completes a frame", strings.Join(frames[:5], "\n\n") + "\n\n" + frames[5], 5},
		{"empty stream", "", 0},
		{"only comments", ": keep-alive\n\n: keep-alive\n\n", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evs, err := parse(t, tc.stream)

			require.ErrorIs(t, err, domain.ErrIncompleteStream)
			assert.Len(t, texts(evs, domain.AgentDelta), tc.wantDeltas, "text received before the cut is still delivered")
		})
	}
}

func TestParseStream_SSEFraming(t *testing.T) {
	tests := []struct {
		name      string
		stream    string
		wantFinal string
	}{
		{"CRLF line endings", strings.ReplaceAll(frameFor(finalHello), "\n", "\r\n"), "hello"},
		{"comments and ids are ignored", ": hi\n\nid: 7\nretry: 100\ndata: " + finalHello + "\n\n", "hello"},
		{"no space after the colon", "data:" + finalHello + "\n\n", "hello"},
		{"a keep-alive between frames", ": ping\n\n" + frameFor(finalHello), "hello"},
		{
			"data split over several lines is joined with a newline",
			"data: {\"content\":{\"role\":\"model\",\n" +
				"data: \"parts\":[{\"text\":\"hello\"}]}}\n\n",
			"hello",
		},
		{"unknown JSON members are ignored", frameFor(`{"brandNew":{"x":1},"content":{"role":"model","parts":[{"text":"hello","extra":true}]}}`), "hello"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evs, err := parse(t, tc.stream)

			require.NoError(t, err)
			assert.Equal(t, []string{tc.wantFinal}, texts(evs, domain.AgentFinal))
		})
	}
}

func TestParseStream_EventRules(t *testing.T) {
	tests := []struct {
		name      string
		stream    string
		wantKinds []domain.AgentEventKind
		wantText  string
	}{
		{
			name:      "a partial event is a delta",
			stream:    frameFor(`{"partial":true,"content":{"role":"model","parts":[{"text":"Hi "}]}}`) + frameFor(finalHello),
			wantKinds: []domain.AgentEventKind{domain.AgentDelta, domain.AgentFinal, domain.AgentUsage},
			wantText:  "Hi ",
		},
		{
			name:      "a thought in a final event is dropped but the answer stays",
			stream:    frameFor(`{"content":{"role":"model","parts":[{"text":"musing","thought":true},{"text":"answer"}]}}`),
			wantKinds: []domain.AgentEventKind{domain.AgentFinal},
			wantText:  "answer",
		},
		{
			name:      "an event with only reasoning yields nothing, then the stream is incomplete",
			stream:    frameFor(`{"partial":true,"content":{"role":"model","parts":[{"text":"musing","thought":true}]}}`),
			wantKinds: nil,
		},
		{
			name:      "usage on a partial event is not counted",
			stream:    frameFor(`{"partial":true,"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":9},"content":{"role":"model","parts":[{"text":"x"}]}}`) + frameFor(finalHello),
			wantKinds: []domain.AgentEventKind{domain.AgentDelta, domain.AgentFinal, domain.AgentUsage},
			wantText:  "x",
		},
		{
			name:      "a partial function call is not reported twice",
			stream:    frameFor(`{"partial":true,"content":{"role":"model","parts":[{"functionCall":{"name":"lookup"}}]}}`) + frameFor(`{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup"}}]}}`) + frameFor(finalHello),
			wantKinds: []domain.AgentEventKind{domain.AgentToolCall, domain.AgentFinal, domain.AgentUsage},
		},
		{
			name:      "an event without content is skipped",
			stream:    frameFor(`{"partial":false,"actions":{"stateDelta":{}}}`) + frameFor(finalHello),
			wantKinds: []domain.AgentEventKind{domain.AgentFinal, domain.AgentUsage},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evs, err := parse(t, tc.stream)

			if tc.wantKinds == nil {
				require.ErrorIs(t, err, domain.ErrIncompleteStream)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantKinds, kinds(evs))
			if tc.wantText != "" {
				assert.Equal(t, tc.wantText, evs[0].Text)
			}
		})
	}
}

func TestParseStream_Errors(t *testing.T) {
	tests := []struct {
		name         string
		stream       string
		wantAgentErr string
		wantInErr    string
	}{
		{
			name:         "errorCode in an ordinary event",
			stream:       frameFor(`{"errorCode":"RESOURCE_EXHAUSTED","errorMessage":"quota exceeded"}`),
			wantAgentErr: "RESOURCE_EXHAUSTED: quota exceeded",
		},
		{
			name:         "a named error event with a message",
			stream:       "event: error\ndata: {\"error\":\"boom\"}\n\n",
			wantAgentErr: "boom",
		},
		{
			name:         "a named error event without JSON still reports its text",
			stream:       "event: error\ndata: plain text failure\n\n",
			wantAgentErr: "plain text failure",
		},
		{
			name:      "invalid JSON in a data frame",
			stream:    "data: {not json}\n\n",
			wantInErr: "decoding agent event",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.stream)

			require.Error(t, err)
			if tc.wantAgentErr != "" {
				var ae *domain.AgentError
				require.True(t, errors.As(err, &ae), "got %v", err)
				assert.Contains(t, ae.Detail, tc.wantAgentErr)
				return
			}
			assert.Contains(t, err.Error(), tc.wantInErr)
			assert.NotErrorIs(t, err, domain.ErrIncompleteStream)
		})
	}

	t.Run("events before an error are still delivered", func(t *testing.T) {
		stream := frameFor(`{"partial":true,"content":{"role":"model","parts":[{"text":"half "}]}}`) +
			"event: error\ndata: {\"error\":\"boom\"}\n\n"

		evs, err := parse(t, stream)

		require.Error(t, err)
		assert.Equal(t, []string{"half "}, texts(evs, domain.AgentDelta))
	})
}

// brokenAfter delivers its text and then fails with err, like a connection that
// breaks in the middle of a reply.
type brokenAfter struct {
	text string
	err  error
	sent bool
}

func (b *brokenAfter) Read(p []byte) (int, error) {
	if b.sent {
		return 0, b.err
	}
	b.sent = true
	return copy(p, b.text), nil
}

// A connection that breaks mid-reply is a cut-off answer, whatever the transport
// calls it, and the text that arrived before the cut stays delivered.
func TestParseStream_ABrokenConnectionIsACutOffAnswer(t *testing.T) {
	partial := frameFor(`{"partial":true,"content":{"role":"model","parts":[{"text":"half "}]}}`)
	tests := []struct {
		name    string
		err     error
		wantCut bool
	}{
		{"a chunked body that ends early", io.ErrUnexpectedEOF, true},
		{"a connection reset by the peer", &net.OpError{Op: "read", Net: "tcp", Err: fmt.Errorf("read: %w", syscall.ECONNRESET)}, true},
		{"anything else is still a plain failure", errors.New("disk on fire"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []domain.AgentEvent
			var err error
			for ev, e := range parseStream(&brokenAfter{text: partial, err: tc.err}) {
				if e != nil {
					err = e
					break
				}
				got = append(got, ev)
			}

			require.Error(t, err)
			assert.Equal(t, []string{"half "}, texts(got, domain.AgentDelta), "what arrived before the break is kept")
			require.ErrorIs(t, err, tc.err, "the cause is still there for the logs")
			if tc.wantCut {
				assert.ErrorIs(t, err, domain.ErrIncompleteStream)
			} else {
				assert.NotErrorIs(t, err, domain.ErrIncompleteStream)
			}
		})
	}
}

// The database refuses a NUL character, and a reply that cannot be saved is a
// reply lost. The agent's text is untrusted, so the NUL is replaced before it
// goes any further, in deltas and in final text alike.
func TestParseStream_ANulInTheAgentsTextIsReplaced(t *testing.T) {
	stream := frameFor(`{"partial":true,"content":{"role":"model","parts":[{"text":"a\u0000b"}]}}`) +
		frameFor(`{"turnComplete":true,"content":{"role":"model","parts":[{"text":"c\u0000d"}]}}`)

	evs, err := parse(t, stream)

	require.NoError(t, err)
	assert.Equal(t, []string{"a\uFFFDb"}, texts(evs, domain.AgentDelta))
	assert.Equal(t, []string{"c\uFFFDd"}, texts(evs, domain.AgentFinal))
}
