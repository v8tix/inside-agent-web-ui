// Package stack runs the whole Web UI for integration tests, the way the
// program does: a real PostgreSQL (see pgenv), the scripted ADK agent behind a
// switch that can make it vanish, and the application served by the production
// server type, so that starting and shutting down are tested too.
//
// Tests talk to it over HTTP with a Client. They can also look into the
// database through Pool, and at what the agent saw through Model.
//
// It needs Docker (for PostgreSQL).
package stack

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/app"
	authinfra "github.com/v8tix/inside-agent-web-ui/internal/infrastructure/auth"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/logging"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/postgres"
	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
	"github.com/v8tix/inside-agent-web-ui/it/internal/pgenv"
)

// The first user of every stack.
const (
	Email    = "owner@example.com"
	Password = "a-long-enough-password"
)

// Stack is a running Web UI.
type Stack struct {
	// URL is where a browser reaches it, such as http://127.0.0.1:41233.
	URL string
	// Config is what the application was built from.
	Config config.Config
	// Agent is the scripted agent behind the Web UI. It can be taken down.
	Agent *Agent
	// Model is the model the agent runs, for assertions on what it saw.
	Model *testagent.Model
	// Pool reads the database the Web UI writes.
	Pool *pgxpool.Pool

	t        testing.TB
	cancel   context.CancelFunc
	served   chan error
	shutdown func() time.Duration

	mu      sync.Mutex
	clients []*Client // those handed out, so that their idle connections can be closed first
}

type settings struct {
	model  []testagent.Option
	agent  []testagent.HandlerOption
	values map[string]string
	// agentURL, when set, is a real agent to use instead of the scripted one.
	agentURL, appName string
	// logs, when set, receives the application's log at its most talkative.
	logs io.Writer
}

// Option customizes a Stack.
type Option func(*settings)

// WithModel sets the pace and script of the scripted agent.
func WithModel(opts ...testagent.Option) Option {
	return func(s *settings) { s.model = append(s.model, opts...) }
}

// WithAgentServer sets how the scripted agent's server behaves, such as how long
// it lets one reply stream.
func WithAgentServer(opts ...testagent.HandlerOption) Option {
	return func(s *settings) { s.agent = append(s.agent, opts...) }
}

// WithExternalAgent uses a real agent (its ADK REST base URL, such as
// http://localhost:9093/api, and its application name) instead of the scripted
// one. Agent and Model are then nil.
func WithExternalAgent(baseURL, appName string) Option {
	return func(s *settings) { s.agentURL, s.appName = baseURL, appName }
}

// WithLogs sends the application's log to w, as the program's own logger would
// write it, at the debug level so that nothing is held back. Read w once the
// stack is shut down; until then the server may be writing to it.
func WithLogs(w io.Writer) Option {
	return func(s *settings) { s.logs, s.values["APP_LOG_LEVEL"] = w, "debug" }
}

// WithSetting sets a configuration key, as if it were in .env.
func WithSetting(key, value string) Option {
	return func(s *settings) { s.values[key] = value }
}

// Start runs the Web UI and its agent until the test ends.
//
// Pattern: Test Through Serve
func Start(t testing.TB, opts ...Option) *Stack {
	t.Helper()
	s := &settings{
		model: []testagent.Option{testagent.WithWordDelay(2 * time.Millisecond)},
		// Longer than the five seconds the server waits for a connection that has
		// not sent a request yet before it counts it as idle (see closeClientConnections).
		values: map[string]string{"APP_SHUTDOWN_GRACE": "10s"},
	}
	for _, opt := range opts {
		opt(s)
	}
	dbURL := pgenv.NewDatabase(t)

	var (
		model    *testagent.Model
		agent    *Agent
		agentURL = s.agentURL
		agentApp = s.appName
	)
	if agentURL == "" {
		model = testagent.NewModel(s.model...)
		handler, err := testagent.NewHandler(model, s.agent...)
		require.NoError(t, err)
		agent = newAgent(handler)
		t.Cleanup(agent.Close)
		agentURL, agentApp = agent.URL+"/api", testagent.AppName
	}

	// The Web UI is told its own address (it decides on secure cookies from it),
	// so the listener has to exist before the application is built.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	base := "http://" + ln.Addr().String()

	values := map[string]string{
		"DB_URL":         dbURL,
		"AGENT_BASE_URL": agentURL,
		"AGENT_APP_NAME": agentApp,
		"ADMIN_EMAIL":    Email,
		"ADMIN_PASSWORD": Password,
		"APP_BASE_URL":   base,
	}
	maps.Copy(values, s.values)
	cfg, err := config.Load(func(k string) (string, bool) { v, ok := values[k]; return v, ok })
	require.NoError(t, err)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if s.logs != nil {
		log = logging.New(cfg.App, s.logs)
	}
	pool, err := postgres.NewPool(t.Context(), cfg.DB)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// The program's own run, from building the application to shutting it down.
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	served := make(chan error, 1)
	go func() { served <- app.Serve(ctx, cfg, log, ln) }()
	waitUntilServing(t, base, served)

	st := &Stack{URL: base, Config: cfg, Agent: agent, Model: model, Pool: pool, t: t, cancel: cancel, served: served}
	var stopped atomic.Pointer[time.Duration]
	st.shutdown = func() time.Duration {
		if d := stopped.Load(); d != nil {
			return *d
		}
		started := time.Now()
		st.closeClientConnections()
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("the server did not shut down cleanly: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Errorf("the server did not shut down within 30 s")
		}
		took := time.Since(started)
		stopped.Store(&took)
		return took
	}
	t.Cleanup(func() { st.shutdown() })
	return st
}

// closeClientConnections closes the connections the test's clients are only
// keeping. A client sometimes dials a second connection and then finds the first
// free again; the second is never used, and the server, which cannot tell it from
// one that is about to send a request, waits for it to be five seconds old before
// it counts it as idle. That is as long as the grace a test gives the shutdown.
func (s *Stack) closeClientConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clients {
		c.http.CloseIdleConnections()
	}
}

// waitUntilServing returns once the server answers, or fails the test if the
// application could not be built.
func waitUntilServing(t testing.TB, base string, served <-chan error) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-served:
			require.NoError(t, err, "the server stopped before it served")
			require.FailNow(t, "the server stopped before it served")
		default:
		}
		if res, err := http.Get(base + "/healthz"); err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.FailNow(t, "the server did not start serving within 60 s")
}

// Shutdown does what SIGTERM does to the program: it ends the replies being
// written, lets requests finish, and returns how long that took. Calling it
// again returns the same answer. It is also called when the test ends.
func (s *Stack) Shutdown() time.Duration { return s.shutdown() }

// Agent is the scripted agent behind a switch. While it is down, a request is
// dropped without an answer, as when the process is gone.
type Agent struct {
	*httptest.Server
	down atomic.Bool
}

func newAgent(h http.Handler) *Agent {
	a := &Agent{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.down.Load() {
			panic(http.ErrAbortHandler) // closes the connection without a response
		}
		h.ServeHTTP(w, r)
	}))
	return a
}

// Down makes the agent stop answering.
func (a *Agent) Down() { a.down.Store(true) }

// Up makes it answer again.
func (a *Agent) Up() { a.down.Store(false) }

// AddUser creates another user, the way an operator would with SQL: the Web UI
// has no sign-up, and the first user comes from the settings.
func (s *Stack) AddUser(email, password string) {
	s.t.Helper()
	hash, err := authinfra.NewArgon2().Hash(s.t.Context(), password)
	require.NoError(s.t, err)
	_, err = s.Pool.Exec(s.t.Context(), `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, $3)`, uuid.NewV7(), email, hash)
	require.NoError(s.t, err)
}
