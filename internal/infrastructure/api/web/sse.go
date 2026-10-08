package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// writeTimeout bounds each write to a streaming client, so one that stopped
// reading cannot hold a connection (and its agent run) open forever. The server
// has no global write timeout on purpose; streams manage their own.
const writeTimeout = 15 * time.Second

// sseWriter writes a server-sent event stream. Every method flushes, so the
// browser sees events as they happen.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

// newSSEWriter sends the stream headers and a first comment straight away, so
// the browser considers the stream open even if the first event is far off.
func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // reverse proxies must not buffer the stream
	s := &sseWriter{w: w, rc: http.NewResponseController(w)}
	return s, s.write(": ok\n\n")
}

// event sends a named event. data may span lines.
func (s *sseWriter) event(name, data string) error {
	var b strings.Builder
	b.WriteString("event: " + name + "\n")
	for line := range strings.SplitSeq(strings.ReplaceAll(data, "\r", ""), "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	return s.write(b.String())
}

// comment sends a comment line, which clients ignore: a keep-alive.
func (s *sseWriter) comment(text string) error {
	return s.write(": " + strings.ReplaceAll(text, "\n", " ") + "\n\n")
}

// retry tells the browser how long to wait before it reconnects.
func (s *sseWriter) retry(d time.Duration) error {
	return s.write(fmt.Sprintf("retry: %d\n\n", d.Milliseconds()))
}

func (s *sseWriter) write(chunk string) error {
	// Writers that cannot take a deadline (tests, some wrappers) are fine.
	if err := s.rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := io.WriteString(s.w, chunk); err != nil {
		return err
	}
	if err := s.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}
