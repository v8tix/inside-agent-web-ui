package testagent_test

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
)

// These tests talk to the real ADK REST server over HTTP. They pin the wire
// behavior the Web UI's agent adapter depends on.

// named decodes the part of a function call or response we care about. The
// explicit tag matters: encoding/json/v2 matches member names case-sensitively.
type named struct {
	Name string `json:"name"`
}

type frame struct {
	Event string
	Data  string
}

type event struct {
	Partial       bool `json:"partial"`
	TurnComplete  bool `json:"turnComplete"`
	UsageMetadata *struct {
		Prompt     int `json:"promptTokenCount"`
		Completion int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	Content *struct {
		Parts []struct {
			Text             string `json:"text"`
			Thought          bool   `json:"thought"`
			FunctionCall     *named `json:"functionCall"`
			FunctionResponse *named `json:"functionResponse"`
		} `json:"parts"`
	} `json:"content"`
}

func (e event) text() string {
	var sb strings.Builder
	if e.Content != nil {
		for _, p := range e.Content.Parts {
			if !p.Thought {
				sb.WriteString(p.Text)
			}
		}
	}
	return sb.String()
}

func startAgent(t *testing.T, opts ...testagent.Option) (*httptest.Server, *testagent.Model) {
	t.Helper()
	m := testagent.NewModel(append([]testagent.Option{testagent.WithWordDelay(0)}, opts...)...)
	h, err := testagent.NewHandler(m)
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, m
}

func post(t *testing.T, ctx context.Context, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func createSession(t *testing.T, base, id string) {
	t.Helper()
	resp := post(t, t.Context(), base+"/api/apps/ui_agent/users/u1/sessions/"+id, "{}")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func runBody(session, text string) string {
	return fmt.Sprintf(`{"appName":"ui_agent","userId":"u1","sessionId":%q,"newMessage":{"role":"user","parts":[{"text":%q}]},"streaming":true}`, session, text)
}

// readFrames parses an SSE body into frames until the stream ends.
func readFrames(t *testing.T, body io.Reader) []frame {
	t.Helper()
	var frames []frame
	var cur frame
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur != (frame{}) {
				frames = append(frames, cur)
			}
			cur = frame{}
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	return frames
}

func events(t *testing.T, frames []frame) []event {
	t.Helper()
	var out []event
	for _, f := range frames {
		require.Empty(t, f.Event, "unexpected named event: %+v", f)
		var e event
		require.NoError(t, json.Unmarshal([]byte(f.Data), &e), f.Data)
		out = append(out, e)
	}
	return out
}

func run(t *testing.T, srv *httptest.Server, session, text string) []frame {
	t.Helper()
	createSession(t, srv.URL, session)
	resp := post(t, t.Context(), srv.URL+"/api/run_sse", runBody(session, text))
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
	return readFrames(t, resp.Body)
}

func TestREST_ListApps(t *testing.T) {
	srv, _ := startAgent(t)

	resp, err := http.Get(srv.URL + "/api/list-apps")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var apps []string
	require.NoError(t, json.UnmarshalRead(resp.Body, &apps))

	assert.Equal(t, []string{"ui_agent"}, apps)
}

func TestREST_StreamingEcho(t *testing.T) {
	srv, _ := startAgent(t)

	evs := events(t, run(t, srv, "echo", "hello brave world"))

	require.GreaterOrEqual(t, len(evs), 4)
	var joined strings.Builder
	for _, e := range evs[:len(evs)-1] {
		assert.True(t, e.Partial)
		assert.Nil(t, e.UsageMetadata, "usage arrives only with the final event")
		joined.WriteString(e.text())
	}
	final := evs[len(evs)-1]
	assert.False(t, final.Partial)
	assert.True(t, final.TurnComplete)
	assert.Equal(t, "You said: hello brave world", final.text())
	assert.Equal(t, final.text(), joined.String(), "partials add up to the final text: a client must not append both")
	require.NotNil(t, final.UsageMetadata)
	assert.Equal(t, 5, final.UsageMetadata.Completion)
}

// The ADK server limits how long one reply may take to stream (its launcher's
// --sse-write-timeout, two minutes unless told otherwise), and ends the stream
// where it is when that time is up: an agent whose answer takes longer reaches
// its client cut off.
func TestREST_ALimitOnTheStreamCutsASlowReplyOff(t *testing.T) {
	m := testagent.NewModel(testagent.WithWordDelay(0), testagent.WithSlow(20*time.Millisecond, 50)) // a second of chunks
	tests := []struct {
		name    string
		limit   []testagent.HandlerOption
		cutOff  bool
		minimum int
	}{
		{name: "with time enough the reply is whole", limit: []testagent.HandlerOption{testagent.WithSSEWriteTimeout(time.Minute)}, minimum: 50},
		{name: "with the default too", cutOff: false, minimum: 50},
		{name: "with less than it takes, the stream ends in the middle", limit: []testagent.HandlerOption{testagent.WithSSEWriteTimeout(150 * time.Millisecond)}, cutOff: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := testagent.NewHandler(m, tc.limit...)
			require.NoError(t, err)
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)

			evs := events(t, run(t, srv, "limit", "#slow"))

			require.NotEmpty(t, evs)
			last := evs[len(evs)-1]
			if tc.cutOff {
				assert.True(t, last.Partial, "the last thing that arrived is a piece, not the whole reply")
				assert.Less(t, len(evs), 50)
				return
			}
			assert.False(t, last.Partial)
			assert.True(t, last.TurnComplete)
			assert.GreaterOrEqual(t, len(evs), tc.minimum)
		})
	}
}

func TestREST_ThinkSendsReasoningFirst(t *testing.T) {
	srv, _ := startAgent(t)

	evs := events(t, run(t, srv, "think", "#think what now"))

	first := evs[0].Content.Parts[0]
	assert.True(t, first.Thought)
	assert.Equal(t, "Let me think about that.", first.Text)
	assert.Equal(t, "You said: #think what now", evs[len(evs)-1].text(), "thought parts are not part of the answer text")
}

func TestREST_ToolCallAndResult(t *testing.T) {
	srv, _ := startAgent(t)

	evs := events(t, run(t, srv, "tool", "#tool find the answer"))

	var sawCall, sawResponse bool
	for _, e := range evs {
		if e.Content == nil {
			continue
		}
		for _, p := range e.Content.Parts {
			sawCall = sawCall || (p.FunctionCall != nil && p.FunctionCall.Name == "lookup")
			sawResponse = sawResponse || (p.FunctionResponse != nil && p.FunctionResponse.Name == "lookup")
		}
	}
	assert.True(t, sawCall, "the tool name is visible in a functionCall part")
	assert.True(t, sawResponse)
	assert.Equal(t, "The lookup returned: forty-two", evs[len(evs)-1].text())
}

func TestREST_ModelErrorIsANamedErrorEvent(t *testing.T) {
	srv, _ := startAgent(t)

	frames := run(t, srv, "err", "please #error now")

	require.Len(t, frames, 1)
	assert.Equal(t, "error", frames[0].Event)
	assert.Contains(t, frames[0].Data, "scripted failure")
}

func TestREST_SessionBehavior(t *testing.T) {
	srv, _ := startAgent(t)
	createSession(t, srv.URL, "exists")

	tests := []struct {
		name       string
		path       string
		body       string
		wantStatus int
		wantInBody string
	}{
		{
			name:       "creating a session twice is a 500, not a 409",
			path:       "/api/apps/ui_agent/users/u1/sessions/exists",
			body:       "{}",
			wantStatus: http.StatusInternalServerError,
			wantInBody: "already exists",
		},
		{
			name:       "running without a session is a 404 that says so",
			path:       "/api/run_sse",
			body:       runBody("missing", "hi"),
			wantStatus: http.StatusNotFound,
			wantInBody: "session not found",
		},
		{
			name:       "unknown request fields are rejected",
			path:       "/api/run_sse",
			body:       `{"appName":"ui_agent","userId":"u1","sessionId":"exists","bogus":1,"newMessage":{"role":"user","parts":[{"text":"hi"}]}}`,
			wantStatus: http.StatusBadRequest,
			wantInBody: "unknown field",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := post(t, t.Context(), srv.URL+tc.path, tc.body)
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)

			assert.Equal(t, tc.wantStatus, resp.StatusCode)
			assert.Contains(t, string(body), tc.wantInBody)
		})
	}
}

func TestREST_ClientDisconnectCancelsTheModel(t *testing.T) {
	srv, model := startAgent(t, testagent.WithSlow(20*time.Millisecond, 500))
	createSession(t, srv.URL, "slow")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	resp := post(t, ctx, srv.URL+"/api/run_sse", runBody("slow", "#slow"))
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	require.True(t, sc.Scan(), "first frame arrives")
	cancel() // the browser went away

	require.Eventually(t, func() bool { return model.Probe().Cancelled() == 1 },
		3*time.Second, 20*time.Millisecond, "the upstream run must stop when the client leaves")
}
