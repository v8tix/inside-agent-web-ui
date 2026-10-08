// Integration tests for the assembled application: real PostgreSQL, the
// scripted ADK agent, and the real HTTP handler. Group A: needs Docker.
package app_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/app"
	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/postgres"
	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
	"github.com/v8tix/inside-agent-web-ui/it/internal/pgenv"
)

const (
	adminEmail    = "admin@example.com"
	adminPassword = "a-long-enough-password"
)

func testConfig(t *testing.T, dbURL, agentURL string) config.Config {
	t.Helper()
	env := map[string]string{
		"DB_URL":         dbURL,
		"AGENT_BASE_URL": agentURL + "/api",
		"AGENT_APP_NAME": testagent.AppName,
		"ADMIN_EMAIL":    adminEmail,
		"ADMIN_PASSWORD": adminPassword,
		"APP_BASE_URL":   "http://localhost:8300",
	}
	cfg, err := config.Load(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	require.NoError(t, err)
	return cfg
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func startAgent(t *testing.T) *httptest.Server {
	t.Helper()
	h, err := testagent.NewHandler(testagent.NewModel(testagent.WithWordDelay(0)))
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// browser is an HTTP client with a cookie jar that does not follow redirects,
// so tests can see them.
func browser(t *testing.T, base string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func do(t *testing.T, c *http.Client, method, target string, form url.Values) *http.Response {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(t.Context(), method, target, body)
	require.NoError(t, err)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	res, err := c.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func bodyOf(t *testing.T, res *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return string(b)
}

func TestAppSuite(t *testing.T) {
	dbURL := pgenv.NewDatabase(t)
	agent := startAgent(t)
	cfg := testConfig(t, dbURL, agent.URL)

	application, err := app.Build(t.Context(), cfg, quietLogger())
	require.NoError(t, err)
	t.Cleanup(application.Close)
	srv := httptest.NewServer(application.Handler)
	t.Cleanup(srv.Close)

	t.Run("the schema is migrated and the first user can sign in", func(t *testing.T) {
		b := browser(t, srv.URL)

		login := do(t, b, http.MethodPost, srv.URL+"/login", url.Values{"email": {adminEmail}, "password": {adminPassword}})

		assert.Equal(t, http.StatusSeeOther, login.StatusCode)
		assert.Equal(t, "/", login.Header.Get("Location"))
		home := do(t, b, http.MethodGet, srv.URL+"/", nil)
		assert.Equal(t, http.StatusOK, home.StatusCode)
		body := bodyOf(t, home)
		assert.Contains(t, body, adminEmail)
		assert.Contains(t, body, `data-testid="logout"`)
	})

	t.Run("anonymous visitors are sent to sign in, and wrong passwords are refused", func(t *testing.T) {
		b := browser(t, srv.URL)

		anon := do(t, b, http.MethodGet, srv.URL+"/", nil)
		assert.Equal(t, http.StatusSeeOther, anon.StatusCode)
		assert.Equal(t, "/login?next=%2F", anon.Header.Get("Location"))

		bad := do(t, b, http.MethodPost, srv.URL+"/login", url.Values{"email": {adminEmail}, "password": {"wrong-password-1"}})
		assert.Equal(t, http.StatusUnprocessableEntity, bad.StatusCode)
		unknown := do(t, b, http.MethodPost, srv.URL+"/login", url.Values{"email": {"nobody@example.com"}, "password": {adminPassword}})
		assert.Equal(t, http.StatusUnprocessableEntity, unknown.StatusCode)
		// The form keeps the typed email; apart from that the two pages are identical.
		wrongPassword := strings.ReplaceAll(bodyOf(t, bad), adminEmail, "EMAIL")
		unknownEmail := strings.ReplaceAll(bodyOf(t, unknown), "nobody@example.com", "EMAIL")
		assert.Equal(t, wrongPassword, unknownEmail, "an unknown email looks exactly like a wrong password")
	})

	t.Run("signing out ends the session on the server", func(t *testing.T) {
		b := browser(t, srv.URL)
		do(t, b, http.MethodPost, srv.URL+"/login", url.Values{"email": {adminEmail}, "password": {adminPassword}})
		sessionURL, err := url.Parse(srv.URL)
		require.NoError(t, err)
		stolen := b.Jar.Cookies(sessionURL)
		require.NotEmpty(t, stolen)

		out := do(t, b, http.MethodPost, srv.URL+"/logout", url.Values{})
		assert.Equal(t, "/login", out.Header.Get("Location"))

		replay := browser(t, srv.URL)
		replay.Jar.SetCookies(sessionURL, stolen) // someone who kept the old cookie
		res := do(t, replay, http.MethodGet, srv.URL+"/", nil)
		assert.Equal(t, http.StatusSeeOther, res.StatusCode, "the old cookie is dead")
	})

	t.Run("the middleware is in place", func(t *testing.T) {
		res := do(t, browser(t, srv.URL), http.MethodGet, srv.URL+"/healthz", nil)

		assert.Equal(t, http.StatusOK, res.StatusCode)
		assert.NotEmpty(t, res.Header.Get("X-Request-Id"))
		assert.Contains(t, res.Header.Get("Content-Security-Policy"), "script-src 'self'")
	})
}

func TestApp_Restart(t *testing.T) {
	dbURL := pgenv.NewDatabase(t)
	agent := startAgent(t)
	cfg := testConfig(t, dbURL, agent.URL)

	first, err := app.Build(t.Context(), cfg, quietLogger())
	require.NoError(t, err)
	first.Close()

	// A crash leaves a reply marked as streaming.
	pool, err := postgres.NewPool(t.Context(), cfg.DB)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var userID uuid.UUID
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT id FROM users WHERE email = $1`, adminEmail).Scan(&userID))
	convID, msgID := uuid.NewV7(), uuid.NewV7()
	_, err = pool.Exec(t.Context(), `INSERT INTO conversations (id, user_id, title) VALUES ($1, $2, 'c')`, convID, userID)
	require.NoError(t, err)
	_, err = pool.Exec(t.Context(),
		`INSERT INTO messages (id, conversation_id, seq, role, status, content) VALUES ($1, $2, 1, 'assistant', 'streaming', 'half')`,
		msgID, convID)
	require.NoError(t, err)

	second, err := app.Build(t.Context(), cfg, quietLogger())
	require.NoError(t, err)
	t.Cleanup(second.Close)

	t.Run("the first user is not created twice", func(t *testing.T) {
		var n int
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&n))
		assert.Equal(t, 1, n)
	})
	t.Run("a reply the crash left streaming is marked as failed", func(t *testing.T) {
		var status, errText, content string
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT status, error, content FROM messages WHERE id = $1`, msgID).Scan(&status, &errText, &content))
		assert.Equal(t, "failed", status)
		assert.Equal(t, domain.FailureInterrupted, errText)
		assert.Equal(t, "half", content, "the text that was saved is kept")
	})
}

func TestApp_BuildFailures(t *testing.T) {
	agent := startAgent(t)

	t.Run("an unreachable database is an error and nothing stays open", func(t *testing.T) {
		cfg := testConfig(t, "postgres://u:p@127.0.0.1:1/db", agent.URL)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()

		application, err := app.Build(ctx, cfg, quietLogger())

		require.Error(t, err)
		assert.Nil(t, application)
	})

	t.Run("a bootstrap password the service refuses stops the start", func(t *testing.T) {
		cfg := testConfig(t, pgenv.NewDatabase(t), agent.URL)
		cfg.Admin.Password = "short" // bypasses config validation to prove the service guards it too

		application, err := app.Build(t.Context(), cfg, quietLogger())

		require.Error(t, err)
		assert.Contains(t, err.Error(), "first user")
		assert.Nil(t, application)
	})
}
