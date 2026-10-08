// Package adkrest is the driven adapter that talks to an agent through the ADK
// REST API: it creates sessions, runs a turn with server-sent events, and turns
// the stream into domain.AgentEvent values.
package adkrest

import (
	"bufio"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
	"syscall"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
)

// maxFrameBytes caps one SSE frame so a misbehaving agent cannot make us buffer
// without bound.
const maxFrameBytes = 8 << 20

var errFrameTooLarge = errors.New("agent stream frame too large")

// The wire types spell out every JSON name: encoding/json/v2 matches names
// case-sensitively and ADK uses camelCase.

type wireEvent struct {
	Partial      bool         `json:"partial"`
	ErrorCode    string       `json:"errorCode"`
	ErrorMessage string       `json:"errorMessage"`
	Usage        *wireUsage   `json:"usageMetadata"`
	Content      *wireContent `json:"content"`
}

type wireUsage struct {
	Prompt     int `json:"promptTokenCount"`
	Completion int `json:"candidatesTokenCount"`
}

type wireContent struct {
	Parts []wirePart `json:"parts"`
}

type wirePart struct {
	Text         string        `json:"text"`
	Thought      bool          `json:"thought"`
	FunctionCall *wireFunction `json:"functionCall"`
}

type wireFunction struct {
	Name string `json:"name"`
}

// frame is one server-sent event.
type frame struct {
	event string
	data  string
}

// parseStream reads an ADK /run_sse body and yields the agent's events.
//
// Rules:
//   - Reasoning ("thought") parts are dropped.
//   - Text in a partial event is a delta. Text in a non-partial event is the
//     final text of its segment, never to be added to the deltas.
//   - A tool call is reported once, from its non-partial event.
//   - Token usage is read from non-partial events only: partial events can
//     repeat cumulative counts.
//   - An error event, or an event carrying errorCode/errorMessage, ends the
//     stream with a *domain.AgentError.
//   - A stream that ends without any final text is domain.ErrIncompleteStream.
//     Text delivered before the cut stays delivered.
//
// Pattern: Final or Incomplete
func parseStream(r io.Reader) iter.Seq2[domain.AgentEvent, error] {
	return func(yield func(domain.AgentEvent, error) bool) {
		sawFinal := false
		for fr, err := range readFrames(r) {
			if err != nil {
				yield(domain.AgentEvent{}, err)
				return
			}
			events, err := decodeFrame(fr)
			for _, ev := range events {
				if ev.Kind == domain.AgentFinal {
					sawFinal = true
				}
				if !yield(ev, nil) {
					return
				}
			}
			if err != nil {
				yield(domain.AgentEvent{}, err)
				return
			}
		}
		if !sawFinal {
			yield(domain.AgentEvent{}, domain.ErrIncompleteStream)
		}
	}
}

func decodeFrame(f frame) ([]domain.AgentEvent, error) {
	if f.event == "error" {
		return nil, &domain.AgentError{Detail: errorDetail(f.data)}
	}
	if f.data == "" {
		return nil, nil
	}
	var we wireEvent
	if err := json.Unmarshal([]byte(f.data), &we); err != nil {
		return nil, fmt.Errorf("decoding agent event: %w", err)
	}
	if we.ErrorCode != "" || we.ErrorMessage != "" {
		return nil, &domain.AgentError{Detail: strings.Trim(we.ErrorCode+": "+we.ErrorMessage, ": ")}
	}

	var events []domain.AgentEvent
	if we.Content != nil {
		for _, part := range we.Content.Parts {
			switch {
			case part.Thought:
			case part.Text != "":
				kind := domain.AgentFinal
				if we.Partial {
					kind = domain.AgentDelta
				}
				// The database refuses NUL, and a reply that cannot be saved is lost.
				text := strings.ReplaceAll(part.Text, "\x00", "\uFFFD")
				events = append(events, domain.AgentEvent{Kind: kind, Text: text})
			case part.FunctionCall != nil && !we.Partial:
				events = append(events, domain.AgentEvent{Kind: domain.AgentToolCall, Tool: part.FunctionCall.Name})
			}
		}
	}
	if !we.Partial && we.Usage != nil && (we.Usage.Prompt > 0 || we.Usage.Completion > 0) {
		events = append(events, domain.AgentEvent{
			Kind:  domain.AgentUsage,
			Usage: domain.Usage{Prompt: we.Usage.Prompt, Completion: we.Usage.Completion},
		})
	}
	return events, nil
}

// errorDetail extracts {"error": "..."} from an error event, or falls back to
// the raw data.
func errorDetail(data string) string {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &body); err == nil && body.Error != "" {
		return body.Error
	}
	return data
}

// readFrames splits an SSE body into frames. A frame is complete only at its
// terminating blank line, as the SSE specification requires: whatever is left
// when the stream ends is discarded.
func readFrames(r io.Reader) iter.Seq2[frame, error] {
	return func(yield func(frame, error) bool) {
		br := bufio.NewReaderSize(r, 64<<10)
		var pending frameBuilder
		for {
			line, err := readLine(br, pending.room())
			if err != nil {
				if !errors.Is(err, io.EOF) {
					yield(frame{}, readFailure(err))
				}
				return
			}
			if line != "" {
				pending.add(line)
				continue
			}
			if pending.started() && !yield(pending.frame, nil) {
				return
			}
			pending = frameBuilder{}
		}
	}
}

// frameBuilder puts a frame together from its lines. Comment lines and unknown
// fields are ignored.
type frameBuilder struct {
	frame
	hasData bool // a data line was seen, even an empty one
	size    int  // bytes of this frame read so far, comments included, line endings not
}

func (b *frameBuilder) add(line string) {
	b.size += len(line)
	// A comment. Read as a field it would have an empty name and match nothing below;
	// the check is here because the SSE rules say "comment".
	if strings.HasPrefix(line, ":") {
		return
	}
	field, value, _ := strings.Cut(line, ":")
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		b.event = value
	case "data":
		if b.hasData {
			b.data += "\n"
		}
		b.data += value
		b.hasData = true
	}
}

// started says the frame has something in it: a blank line completes it.
func (b *frameBuilder) started() bool { return b.hasData || b.event != "" }

// room is how many more bytes the frame may take.
func (b *frameBuilder) room() int { return maxFrameBytes - b.size }

// readFailure explains why the agent's stream could not be read. A connection
// that breaks in the middle of the reply (the agent died, a proxy cut it) is a
// cut-off answer, the same as a stream that ends early; anything else is not.
func readFailure(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) {
		return fmt.Errorf("reading agent stream: %w: %w", domain.ErrIncompleteStream, err)
	}
	return fmt.Errorf("reading agent stream: %w", err)
}

// readLine returns the next line without its line ending. It fails once the
// line grows beyond limit bytes.
func readLine(br *bufio.Reader, limit int) (string, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > limit {
			return "", errFrameTooLarge
		}
		switch {
		case err == nil:
			return strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r"), nil
		case errors.Is(err, bufio.ErrBufferFull):
		default:
			return "", err
		}
	}
}
