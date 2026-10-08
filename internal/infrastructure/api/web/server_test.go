package web

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestServer(t *testing.T, h http.Handler, grace time.Duration, onShutdown ...func()) (*Server, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	cfg := config.Config{App: config.App{ShutdownGrace: grace}}
	return NewServer(cfg, testLogger(), h, onShutdown...), ln
}

func TestHealthz(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantBody   string
	}{
		{"liveness ok", http.MethodGet, "/healthz", http.StatusOK, `{"status":"ok"}`},
		{"wrong method", http.MethodPost, "/healthz", http.StatusMethodNotAllowed, ""},
		{"an unknown path is private, so anonymous visitors are sent to sign in", http.MethodGet, "/nope", http.StatusSeeOther, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			testApp(t).Routes().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			assert.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantBody != "" {
				assert.JSONEq(t, tc.wantBody, rec.Body.String())
				assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			}
		})
	}
}

func TestNewServer_Timeouts(t *testing.T) {
	srv, _ := newTestServer(t, testApp(t).Routes(), time.Second)

	assert.Positive(t, srv.http.ReadHeaderTimeout, "slow header attacks need a bound")
	assert.Positive(t, srv.http.IdleTimeout)
	assert.Zero(t, srv.http.WriteTimeout, "a global write timeout would cut long SSE streams")
}

func TestServer_Serve(t *testing.T) {
	t.Run("returns nil after the context is canceled while idle", func(t *testing.T) {
		srv, ln := newTestServer(t, testApp(t).Routes(), time.Second)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- srv.Serve(ctx, ln) }()

		resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		cancel()
		require.NoError(t, waitFor(t, done))
	})

	t.Run("lets an in-flight request finish within the grace period", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			w.WriteHeader(http.StatusNoContent)
		})
		srv, ln := newTestServer(t, h, 5*time.Second)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- srv.Serve(ctx, ln) }()

		status := make(chan int, 1)
		go func() {
			resp, err := http.Get("http://" + ln.Addr().String() + "/")
			if err != nil {
				status <- -1
				return
			}
			_ = resp.Body.Close()
			status <- resp.StatusCode
		}()
		<-started
		cancel()

		select {
		case err := <-done:
			t.Fatalf("Serve returned while a request was still in flight: %v", err)
		case <-time.After(100 * time.Millisecond):
		}

		close(release)
		assert.Equal(t, http.StatusNoContent, <-status)
		require.NoError(t, waitFor(t, done))
	})

	t.Run("forces close and reports an error when the grace period is exceeded", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(started)
			<-release
		})
		srv, ln := newTestServer(t, h, 100*time.Millisecond)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- srv.Serve(ctx, ln) }()

		go func() {
			if resp, err := http.Get("http://" + ln.Addr().String() + "/"); err == nil {
				_ = resp.Body.Close()
			}
		}()
		<-started
		cancel()

		err := waitFor(t, done)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Contains(t, err.Error(), "graceful shutdown")
	})

	t.Run("returns the listener error when serving fails", func(t *testing.T) {
		srv, ln := newTestServer(t, testApp(t).Routes(), time.Second)
		require.NoError(t, ln.Close())

		err := waitFor(t, goServe(t, srv, ln))
		require.Error(t, err)
		assert.NotErrorIs(t, err, http.ErrServerClosed)
	})
}

func goServe(t *testing.T, srv *Server, ln net.Listener) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(t.Context(), ln) }()
	return done
}

func waitFor(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return in time")
		return nil
	}
}
