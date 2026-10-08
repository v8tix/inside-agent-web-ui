package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
)

const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
)

// Server runs the HTTP handler and shuts it down gracefully.
//
// It sets no global WriteTimeout on purpose: a server-wide write deadline
// would cut long-lived SSE streams. Streams manage their own deadlines.
type Server struct {
	http  *http.Server
	log   *slog.Logger
	grace time.Duration
}

// NewServer wires h into a server that honors cfg.App.ShutdownGrace. Each
// onShutdown function runs as shutdown begins, before the server waits for
// requests to finish: that is the moment to end streams (see App.EndStreams),
// which would otherwise keep the server waiting until the grace period is up.
//
// Pattern: Shutdown Hook Before Wait
func NewServer(cfg config.Config, log *slog.Logger, h http.Handler, onShutdown ...func()) *Server {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	for _, f := range onShutdown {
		srv.RegisterOnShutdown(f)
	}
	return &Server{http: srv, log: log, grace: cfg.App.ShutdownGrace}
}

// Serve accepts connections on ln until ctx is canceled, then waits up to the
// grace period for in-flight requests before closing them.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.http.Serve(ln) }()
	s.log.Info("listening", "addr", ln.Addr().String())

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "grace", s.grace)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.grace)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		_ = s.http.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
