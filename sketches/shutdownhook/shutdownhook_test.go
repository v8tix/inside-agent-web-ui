package shutdownhook

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// stream is a response that stays open: it writes "tick", then waits for the
// client to leave or for the server to start shutting down, and then says "bye".
func stream(ending context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, "tick")
		w.(http.Flusher).Flush()
		select {
		case <-ending.Done():
			_, _ = fmt.Fprintln(w, "bye")
		case <-r.Context().Done():
		}
	})
}

func TestTheHookEndsAnOpenStreamBeforeTheWait(t *testing.T) {
	srv := New(stream)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	resp, err := http.Get("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	lines := bufio.NewScanner(resp.Body)
	if !lines.Scan() || lines.Text() != "tick" {
		t.Fatalf("the stream did not start: %q", lines.Text())
	}

	// The stream would stay open for ever. The grace period is two seconds.
	if err := Stop(srv, 2*time.Second); err != nil {
		t.Fatalf("Stop waited out the grace period: %v", err)
	}

	if !lines.Scan() || lines.Text() != "bye" {
		t.Errorf("the client was not told: %q", lines.Text())
	}
	if lines.Scan() {
		t.Errorf("the stream is still open: %q", lines.Text())
	}
	if err := <-served; err != http.ErrServerClosed {
		t.Errorf("Serve returned %v", err)
	}
}
