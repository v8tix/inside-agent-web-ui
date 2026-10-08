// Package shutdownhook ends long-lived responses as a server starts to shut
// down, so that the wait for open connections ends before the grace period.
package shutdownhook

import (
	"context"
	"net/http"
	"time"
)

// New returns a server for the handler that build makes. build gets a context
// that ends when the server starts to shut down: a handler that keeps a
// response open (an event stream, a long poll) watches it, writes its last
// words and returns.
func New(build func(ending context.Context) http.Handler) *http.Server {
	ending, end := context.WithCancel(context.Background())
	srv := &http.Server{Handler: build(ending)}
	// The hook runs as Shutdown begins, before Shutdown waits for connections.
	srv.RegisterOnShutdown(end)
	return srv
}

// Stop shuts srv down and waits at most grace for the open responses. Without
// the hook, one open stream would hold it for the whole grace period.
func Stop(srv *http.Server, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	return srv.Shutdown(ctx)
}
