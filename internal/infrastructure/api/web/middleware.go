package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies mws to h. The first middleware is the outermost.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for _, mw := range slices.Backward(mws) {
		h = mw(h)
	}
	return h
}

// Handler wraps routes with the standard middleware, outermost first:
// request ID, access log, panic recovery, security headers, deadlines, the
// cross-origin check (CSRF protection), body size limit.
//
// The access log sits outside recovery so it records the 500 that recovery
// writes. The security headers sit outside the two layers that refuse a
// request, the cross-origin check and the body limit, so a refusal carries
// them. The deadlines sit inside the security headers and outside those two
// layers, so a client cannot hold the connection open by being refused
// slowly; they also have to see the body before the body limit wraps it,
// because a wrapped body no longer says that a request has none. The
// cross-origin check comes before the body limit, so a request from another
// site is refused as such whatever it carries. The order of recovery,
// security headers and deadlines among themselves changes nothing that can
// be seen.
//
// Pattern: Ordered Middleware Chain
func Handler(cfg config.Config, log *slog.Logger, routes http.Handler) (http.Handler, error) {
	crossOrigin, err := CrossOrigin(cfg.App.BaseURL)
	if err != nil {
		return nil, err
	}
	return Chain(routes,
		RequestID(),
		AccessLog(log),
		Recover(log),
		SecurityHeaders(cfg.SecureCookies()),
		Deadlines(cfg.HTTP.BodyTimeout, cfg.HTTP.ResponseTimeout),
		crossOrigin,
		MaxBody(cfg.HTTP.MaxBodyBytes),
	), nil
}

type ctxKey int

const requestIDKey ctxKey = iota

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RequestID keeps a well-formed incoming X-Request-ID or generates one, stores
// it in the request context, and echoes it on the response.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if !safeRequestID.MatchString(id) {
				id = uuid.New().String()
			}
			w.Header().Set("X-Request-ID", id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
		})
	}
}

// RequestIDFrom returns the request ID stored by RequestID, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// statusWriter records what was written. It forwards Flush and unwraps, so
// http.ResponseController (used for SSE flushes and write deadlines) still
// reaches the real connection through it.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func wrap(w http.ResponseWriter) *statusWriter {
	if sw, ok := w.(*statusWriter); ok {
		return sw
	}
	return &statusWriter{ResponseWriter: w, status: http.StatusOK}
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) FlushError() error {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *statusWriter) Flush() { _ = w.FlushError() }

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// AccessLog logs one line per request. Probes and static assets log at debug.
//
// The line has seven named attributes and no more: request_id, method, path,
// status, bytes, duration and remote. The path is logged without its query,
// and no header, cookie or form value is logged, except the X-Request-ID that
// RequestID took for the request id.
//
// Pattern: Log Allow-List
func AccessLog(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := wrap(w)
			next.ServeHTTP(sw, r)

			level := slog.LevelInfo
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || strings.HasPrefix(r.URL.Path, "/static/") {
				level = slog.LevelDebug
			}
			log.LogAttrs(r.Context(), level, "request",
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.status),
				slog.Int64("bytes", sw.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
			)
		})
	}
}

// Recover turns a handler panic into a logged 500 so one bad request cannot
// take the server down. http.ErrAbortHandler is passed through, as net/http
// documents.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := wrap(w)
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				logPanic(r.Context(), log, rec)
				if !sw.wrote {
					http.Error(sw, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// logPanic records a recovered panic with the stack it came from. Call it from
// the deferred recover, while the stack is still there.
func logPanic(ctx context.Context, log *slog.Logger, rec any, attrs ...any) {
	log.ErrorContext(ctx, "panic recovered", append([]any{
		"request_id", RequestIDFrom(ctx),
		"panic", fmt.Sprint(rec),
		"stack", string(debug.Stack()),
	}, attrs...)...)
}

// Deadlines bounds how long a client can hold a connection by being slow. Once
// the headers are in, net/http puts no limit on the rest: a client could send a
// body one byte a minute, or ask for a page and never read it, and each would
// keep a goroutine and a file descriptor for as long as it liked.
//
// A body has body to arrive. net/http lifts that deadline itself when the body
// has been read, so a handler may take as long as it needs after that, and the
// request is not cancelled behind its back; a test holds it to that. A response
// has response to be taken; the event stream sets its own deadline for each
// write, after this one. A global ReadTimeout would do neither: it would cut
// every event stream.
//
// Zero turns a limit off. Only code that builds its own config.HTTP or calls
// Deadlines itself can pass one: config.Load refuses zero for both timeouts,
// so a server started from the settings has both limits.
//
// Pattern: Layered Deadlines
func Deadlines(body, response time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := http.NewResponseController(w)
			// Writers that cannot take a deadline (tests, some wrappers) are fine.
			if response > 0 {
				_ = rc.SetWriteDeadline(time.Now().Add(response))
			}
			if body > 0 && r.Body != nil && r.Body != http.NoBody {
				_ = rc.SetReadDeadline(time.Now().Add(body))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CrossOrigin rejects unsafe cross-origin browser requests (CSRF) using
// Sec-Fetch-Site and Origin. publicOrigin is trusted so the app keeps working
// behind a reverse proxy that rewrites Host.
func CrossOrigin(publicOrigin string) (Middleware, error) {
	protection := http.NewCrossOriginProtection()
	if err := protection.AddTrustedOrigin(publicOrigin); err != nil {
		return nil, fmt.Errorf("trusting public origin: %w", err)
	}
	protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "cross-origin request blocked", http.StatusForbidden)
	}))
	return protection.Handler, nil
}

// MaxBody caps request bodies. A declared length over the limit is refused
// immediately; chunked bodies fail with *http.MaxBytesError while being read
// (see writeBodyError).
func MaxBody(limit int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// writeBodyError answers 413 when err came from MaxBody's limit and reports
// whether it handled err.
func writeBodyError(w http.ResponseWriter, err error) bool {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return true
	}
	return false
}
