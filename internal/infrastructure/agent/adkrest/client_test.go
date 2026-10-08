package adkrest

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
)

// counting wraps a handler and counts session creations and runs.
type counting struct {
	http.Handler
	creates atomic.Int32
	runs    atomic.Int32
}

func (c *counting) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/sessions/"):
		c.creates.Add(1)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/run_sse"):
		c.runs.Add(1)
	}
	c.Handler.ServeHTTP(w, r)
}

func newClient(t *testing.T, srv *httptest.Server, mutate ...func(*config.Agent)) *Client {
	t.Helper()
	cfg := config.Agent{BaseURL: srv.URL + "/api", AppName: testagent.AppName, RequestTimeout: 10 * time.Second}
	for _, m := range mutate {
		m(&cfg)
	}
	return NewClient(cfg, nil)
}

func realAgent(t *testing.T, opts ...testagent.Option) (*httptest.Server, *counting, *testagent.Model) {
	t.Helper()
	model := testagent.NewModel(append([]testagent.Option{testagent.WithWordDelay(0)}, opts...)...)
	h, err := testagent.NewHandler(model)
	require.NoError(t, err)
	wrapped := &counting{Handler: h}
	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)
	return srv, wrapped, model
}

func request(text string) domain.AgentRequest {
	return domain.AgentRequest{UserID: uuid.New(), ConversationID: uuid.New(), Text: text}
}

func drain(t *testing.T, c *Client, req domain.AgentRequest) ([]domain.AgentEvent, error) {
	t.Helper()
	var out []domain.AgentEvent
	for ev, err := range c.Run(t.Context(), req) {
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
	return out, nil
}

func TestClient_Run_AgainstTheRealADKServer(t *testing.T) {
	t.Run("streams a reply and creates the session lazily, once per conversation", func(t *testing.T) {
		srv, counts, _ := realAgent(t)
		c := newClient(t, srv)
		req := request("hello brave world")

		evs, err := drain(t, c, req)

		require.NoError(t, err)
		assert.Equal(t, []string{"You ", "said: ", "hello ", "brave ", "world"}, texts(evs, domain.AgentDelta))
		assert.Equal(t, []string{"You said: hello brave world"}, texts(evs, domain.AgentFinal))
		assert.EqualValues(t, 1, counts.creates.Load())
		assert.EqualValues(t, 2, counts.runs.Load(), "the first run is refused (404), then retried after the session is created")

		_, err = drain(t, c, domain.AgentRequest{UserID: req.UserID, ConversationID: req.ConversationID, Text: "again"})
		require.NoError(t, err)
		assert.EqualValues(t, 1, counts.creates.Load(), "the session is reused")
		assert.EqualValues(t, 3, counts.runs.Load())

		_, err = drain(t, c, request("another conversation"))
		require.NoError(t, err)
		assert.EqualValues(t, 2, counts.creates.Load(), "a new conversation gets its own session")
	})

	t.Run("reports tool calls, usage and skips reasoning", func(t *testing.T) {
		srv, _, _ := realAgent(t)
		c := newClient(t, srv)

		evs, err := drain(t, c, request("#tool find the answer"))

		require.NoError(t, err)
		var tools []string
		for _, e := range evs {
			if e.Kind == domain.AgentToolCall {
				tools = append(tools, e.Tool)
			}
		}
		assert.Equal(t, []string{"lookup"}, tools)
		assert.Equal(t, []string{"The lookup returned: forty-two"}, texts(evs, domain.AgentFinal))

		think, err := drain(t, c, request("#think hmm"))
		require.NoError(t, err)
		assert.NotContains(t, strings.Join(texts(think, domain.AgentDelta), ""), "Let me think")
	})

	t.Run("an error from the agent is a *domain.AgentError", func(t *testing.T) {
		srv, _, _ := realAgent(t)

		_, err := drain(t, newClient(t, srv), request("please #error now"))

		var ae *domain.AgentError
		require.ErrorAs(t, err, &ae)
		assert.Contains(t, ae.Detail, "scripted failure")
	})

	t.Run("stopping the iteration cancels the upstream run", func(t *testing.T) {
		srv, _, model := realAgent(t, testagent.WithSlow(20*time.Millisecond, 1000))
		c := newClient(t, srv)

		seen := 0
		for _, err := range c.Run(t.Context(), request("#slow")) {
			require.NoError(t, err)
			if seen++; seen == 3 {
				break
			}
		}

		require.Eventually(t, func() bool { return model.Probe().Cancelled() == 1 },
			3*time.Second, 20*time.Millisecond, "the agent must see the cancellation")
	})

	t.Run("canceling the context cancels the run and reports context.Canceled", func(t *testing.T) {
		srv, _, model := realAgent(t, testagent.WithSlow(20*time.Millisecond, 1000))
		c := newClient(t, srv)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		var last error
		seen := 0
		for _, err := range c.Run(ctx, request("#slow")) {
			if err != nil {
				last = err
				break
			}
			if seen++; seen == 2 {
				cancel()
			}
		}

		require.ErrorIs(t, last, context.Canceled)
		assert.NotErrorIs(t, last, domain.ErrAgentUnavailable, "a user stop is not an outage")
		require.Eventually(t, func() bool { return model.Probe().Cancelled() == 1 },
			3*time.Second, 20*time.Millisecond)
	})
}

// recorded is an answer of the real ADK server, kept in testdata: its status, its
// headers and its body are sent as they were recorded. The fake agent answers
// with these wherever there is one, so that the texts the client looks for in an
// answer are the ones ADK sends.
func recorded(t *testing.T, name string) http.Handler {
	t.Helper()
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(readFixture(t, name))), nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "the body is as long as the recording says")
	resp.Header.Del("Date") // a placeholder in the recordings; the test server writes its own
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		maps.Copy(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	})
}

// fakeAgent is a scriptable agent for failure modes.
type fakeAgent struct {
	runAnswers   []http.Handler // one for each run request, in order; after them runs succeed
	createAnswer http.Handler   // for a session creation; nil answers 200 with nothing
	runs         atomic.Int32
	creates      atomic.Int32
	lastRunBody  atomic.Value
	lastHeader   atomic.Value
	createPath   atomic.Value
}

const finalFrame = `data: {"turnComplete":true,"content":{"role":"model","parts":[{"text":"ok"}]}}` + "\n\n"

func (f *fakeAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/run_sse"):
		n := int(f.runs.Add(1)) - 1
		body, _ := io.ReadAll(r.Body)
		f.lastRunBody.Store(string(body))
		f.lastHeader.Store(r.Header.Clone())
		if n < len(f.runAnswers) {
			f.runAnswers[n].ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, finalFrame)
	case strings.Contains(r.URL.Path, "/sessions/"):
		f.creates.Add(1)
		f.createPath.Store(r.URL.EscapedPath())
		if f.createAnswer != nil {
			f.createAnswer.ServeHTTP(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func TestClient_Run_SessionRecovery(t *testing.T) {
	noSession, unknownApp := recorded(t, "run_no_session.http"), recorded(t, "run_unknown_app.http")
	created, exists := recorded(t, "session_create_new.http"), recorded(t, "session_exists.http")
	diskFull := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "disk full", http.StatusInternalServerError)
	})
	tests := []struct {
		name        string
		agent       *fakeAgent
		wantErr     string
		wantCreates int32
		wantRuns    int32
	}{
		{
			name:        "a lost session (agent restarted) is recreated and the run retried",
			agent:       &fakeAgent{runAnswers: []http.Handler{noSession}, createAnswer: created},
			wantCreates: 1, wantRuns: 2,
		},
		{
			name:        "creating a session that already exists (a race) is fine",
			agent:       &fakeAgent{runAnswers: []http.Handler{noSession}, createAnswer: exists},
			wantCreates: 1, wantRuns: 2,
		},
		{
			// An app that the agent does not serve gets the 404 of a lost session, every time.
			name:        "a second 404 is reported, not retried forever",
			agent:       &fakeAgent{runAnswers: []http.Handler{unknownApp, unknownApp}, createAnswer: created},
			wantErr:     "HTTP 404",
			wantCreates: 1, wantRuns: 2,
		},
		{
			name:        "a failing session creation is reported",
			agent:       &fakeAgent{runAnswers: []http.Handler{noSession}, createAnswer: diskFull},
			wantErr:     "creating session",
			wantCreates: 1, wantRuns: 1,
		},
		{
			name:        "a 404 that is not about the session is reported at once",
			agent:       &fakeAgent{runAnswers: []http.Handler{http.NotFoundHandler()}},
			wantErr:     "HTTP 404",
			wantCreates: 0, wantRuns: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.agent)
			defer srv.Close()

			evs, err := drain(t, newClient(t, srv), request("hi"))

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, []string{"ok"}, texts(evs, domain.AgentFinal))
			}
			assert.Equal(t, tc.wantCreates, tc.agent.creates.Load())
			assert.Equal(t, tc.wantRuns, tc.agent.runs.Load())
		})
	}
}

func TestClient_Run_Failures(t *testing.T) {
	status := func(code int, body string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, body, code) })
	}
	tests := []struct {
		name            string
		handler         http.Handler
		wantUnavailable bool
		wantStatus      int
		wantInErr       string
	}{
		{"502 means the agent is unavailable", status(502, "bad gateway"), true, 502, "HTTP 502"},
		{"503 means the agent is unavailable", status(503, "overloaded"), true, 503, "HTTP 503"},
		{"504 means the agent is unavailable", status(504, "timeout"), true, 504, "HTTP 504"},
		{"500 is an error but not an outage", status(500, "panic: nil map"), false, 500, "panic: nil map"},
		{"400 is an error but not an outage", status(400, "bad request body"), false, 400, "bad request body"},
		{"an unknown field is a 400 that is reported", recorded(t, "run_unknown_field.http"), false, 400, `unknown field "bogus"`},
		{
			name:       "a very long error body is truncated",
			handler:    status(500, strings.Repeat("e", 5000)),
			wantStatus: 500,
			wantInErr:  "HTTP 500",
		},
		{
			name: "a non-SSE success response is refused",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, "<html>login page</html>")
			}),
			wantInErr: "content type",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			_, err := drain(t, newClient(t, srv), request("hi"))

			require.Error(t, err)
			assert.Equal(t, tc.wantUnavailable, errors.Is(err, domain.ErrAgentUnavailable))
			if tc.wantStatus != 0 {
				he, ok := errors.AsType[*HTTPError](err)
				require.True(t, ok, "got %T: %v", err, err)
				assert.Equal(t, tc.wantStatus, he.Status)
				assert.LessOrEqual(t, len(he.Body), maxErrorBody)
			}
			assert.Contains(t, err.Error(), tc.wantInErr)
		})
	}

	t.Run("a refused connection means the agent is unavailable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		c := newClient(t, srv)
		srv.Close()

		_, err := drain(t, c, request("hi"))

		require.ErrorIs(t, err, domain.ErrAgentUnavailable)
	})

	t.Run("a stalled agent hits the run timeout", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-release
		}))
		defer srv.Close()
		defer close(release)
		c := newClient(t, srv, func(a *config.Agent) { a.RequestTimeout = 150 * time.Millisecond })

		start := time.Now()
		_, err := drain(t, c, request("hi"))

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Contains(t, err.Error(), "timed out")
		assert.Less(t, time.Since(start), 3*time.Second)
	})

	t.Run("an agent that never answers hits the run timeout", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		defer srv.Close()
		defer close(release)
		c := newClient(t, srv, func(a *config.Agent) { a.RequestTimeout = 150 * time.Millisecond })

		start := time.Now()
		_, err := drain(t, c, request("hi"))

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Contains(t, err.Error(), "timed out")
		assert.NotErrorIs(t, err, domain.ErrAgentUnavailable, "an agent that is slow is not an agent that is down")
		assert.Less(t, time.Since(start), 3*time.Second)
	})

	t.Run("a caller that leaves before the agent answers", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			cancel() // the caller leaves while the agent has the request and has said nothing
			<-release
		}))
		defer srv.Close()
		defer close(release)

		var last error
		for _, err := range newClient(t, srv).Run(ctx, request("hi")) {
			last = err
		}

		require.ErrorIs(t, last, context.Canceled)
		assert.NotErrorIs(t, last, domain.ErrAgentUnavailable, "a caller that leaves is not an outage")
	})
}

func TestClient_Run_Request(t *testing.T) {
	agent := &fakeAgent{
		runAnswers:   []http.Handler{recorded(t, "run_no_session.http")},
		createAnswer: recorded(t, "session_create_new.http"),
	}
	srv := httptest.NewServer(agent)
	defer srv.Close()
	c := newClient(t, srv, func(a *config.Agent) { a.AppName = "my app" })
	req := domain.AgentRequest{UserID: uuid.New(), ConversationID: uuid.New(), Text: "What is 2+2? \"quoted\" é"}

	_, err := drain(t, c, req)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(agent.lastRunBody.Load().(string)), &got))
	assert.Equal(t, map[string]any{
		"appName":   "my app",
		"userId":    req.UserID.String(),
		"sessionId": req.ConversationID.String(),
		"streaming": true,
		"newMessage": map[string]any{
			"role":  "user",
			"parts": []any{map[string]any{"text": req.Text}},
		},
	}, got)

	header := agent.lastHeader.Load().(http.Header)
	assert.Equal(t, "application/json", header.Get("Content-Type"))
	assert.Equal(t, "text/event-stream", header.Get("Accept"))

	assert.Equal(t,
		fmt.Sprintf("/api/apps/my%%20app/users/%s/sessions/%s", req.UserID, req.ConversationID),
		agent.createPath.Load(), "the app name is path-escaped")
}

func TestClient_Ping(t *testing.T) {
	apps := func(body string, code int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/list-apps" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		})
	}
	tests := []struct {
		name            string
		handler         http.Handler
		wantErr         string
		wantUnavailable bool
	}{
		{"serves the configured app", apps(`["other","ui_agent"]`, 200), "", false},
		{"does not serve the configured app", apps(`["other"]`, 200), `"ui_agent"`, false},
		{"an empty app list", apps(`[]`, 200), `"ui_agent"`, false},
		{"not JSON", apps(`<html>`, 200), "list-apps", false},
		{"a gateway error", apps(`bad gateway`, 502), "HTTP 502", true},
		{"an error status", apps(`nope`, 500), "HTTP 500", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			err := newClient(t, srv).Ping(t.Context())

			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Equal(t, tc.wantUnavailable, errors.Is(err, domain.ErrAgentUnavailable))
		})
	}

	t.Run("works against the real ADK server", func(t *testing.T) {
		srv, _, _ := realAgent(t)
		require.NoError(t, newClient(t, srv).Ping(t.Context()))
	})

	t.Run("an unreachable agent is unavailable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		c := newClient(t, srv)
		srv.Close()

		require.ErrorIs(t, c.Ping(t.Context()), domain.ErrAgentUnavailable)
	})
}

// Every reply being written holds a connection to the agent for as long as it
// lasts. The default of two idle connections per host would make every run
// after the second dial the agent anew, and again for each one that ends.
func TestNewClient_KeepsConnectionsForAsManyRunsAsItWillHave(t *testing.T) {
	c := NewClient(config.Agent{BaseURL: "http://agent:9093/api", AppName: "a", RequestTimeout: time.Minute}, nil)

	transport, ok := c.http.Transport.(*http.Transport)

	require.True(t, ok)
	assert.GreaterOrEqual(t, transport.MaxIdleConnsPerHost, 32)
	assert.Zero(t, c.http.Timeout, "and a stream is not cut by a client timeout; cfg.RequestTimeout bounds each run")
}
