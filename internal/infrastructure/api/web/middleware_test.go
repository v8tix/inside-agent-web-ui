package web

import (
	"bufio"
	"bytes"
	"encoding/json/v2"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
}

func capturingLogger(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})), &buf
}

func TestRequestID(t *testing.T) {
	tests := []struct {
		name      string
		incoming  string
		wantReuse bool
	}{
		{"generated when absent", "", false},
		{"reuses a safe incoming id", "abc-123_X.9", true},
		{"replaces an id with unsafe characters", "bad id/with spaces", false},
		{"replaces an overlong id", strings.Repeat("a", 65), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			h := RequestID()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = RequestIDFrom(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.incoming != "" {
				req.Header.Set("X-Request-ID", tc.incoming)
			}
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			got := rec.Header().Get("X-Request-ID")
			assert.Equal(t, got, seen, "handler and response must agree on the id")
			if tc.wantReuse {
				assert.Equal(t, tc.incoming, got)
				return
			}
			assert.NotEqual(t, tc.incoming, got)
			_, err := uuid.Parse(got)
			assert.NoError(t, err, "generated ids are UUIDs")
		})
	}
}

func TestRecover(t *testing.T) {
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantStatus int
		wantBody   string
		wantLog    string
	}{
		{
			name:       "panic before writing becomes a 500",
			handler:    func(http.ResponseWriter, *http.Request) { panic("boom") },
			wantStatus: http.StatusInternalServerError,
			wantBody:   "Internal Server Error",
			wantLog:    "boom",
		},
		{
			name: "panic after writing keeps the status already sent",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusAccepted)
				panic("late boom")
			},
			wantStatus: http.StatusAccepted,
			wantLog:    "late boom",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log, logs := capturingLogger(slog.LevelInfo)
			// The handler of the case takes the first request only, so the second
			// one goes through the chain that recovered.
			first := true
			h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if first {
					first = false
					tc.handler(w, r)
					return
				}
				okHandler().ServeHTTP(w, r)
			}), RequestID(), Recover(log))
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.wantBody)
			assert.Contains(t, logs.String(), tc.wantLog)
			assert.Contains(t, logs.String(), "request_id="+rec.Header().Get("X-Request-ID"))

			// the chain keeps serving after a panic
			again := httptest.NewRecorder()
			h.ServeHTTP(again, httptest.NewRequest(http.MethodGet, "/", nil))
			assert.Equal(t, http.StatusOK, again.Code)
			assert.Equal(t, "ok", again.Body.String())
		})
	}

	t.Run("http.ErrAbortHandler is passed through", func(t *testing.T) {
		log, _ := capturingLogger(slog.LevelInfo)
		h := Recover(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
		assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		})
	})
}

func TestAccessLog(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantLog []string
		wantNo  bool
	}{
		{"records method, path, status and size", "/things", []string{"method=GET", "path=/things", "status=200", "bytes=2", "request_id="}, false},
		{"health checks stay at debug level", "/healthz", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log, logs := capturingLogger(slog.LevelInfo)
			h := Chain(okHandler(), RequestID(), AccessLog(log))

			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tc.path, nil))

			if tc.wantNo {
				assert.Empty(t, logs.String())
				return
			}
			for _, want := range tc.wantLog {
				assert.Contains(t, logs.String(), want)
			}
		})
	}

	// The line is a list of seven attributes, and what this request carries
	// besides is in none of them: its query, its cookie, its Authorization
	// header and the value in its form.
	t.Run("holds seven attributes and nothing the request carries", func(t *testing.T) {
		var logs bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&logs, nil))
		h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, r.ParseForm())
			require.Equal(t, "word-in-the-form", r.PostForm.Get("password"), "the request holds the form by the time the line is written")
			okHandler().ServeHTTP(w, r)
		}), RequestID(), AccessLog(log))
		req := httptest.NewRequest(http.MethodPost, "/things?token=word-in-the-query", strings.NewReader("password=word-in-the-form"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer word-in-the-header")
		req.AddCookie(&http.Cookie{Name: "awu_session", Value: "word-in-the-cookie"})

		h.ServeHTTP(httptest.NewRecorder(), req)

		for _, word := range []string{"word-in-the-query", "word-in-the-cookie", "word-in-the-header", "word-in-the-form"} {
			assert.NotContains(t, logs.String(), word)
		}
		var line map[string]any
		require.NoError(t, json.Unmarshal(logs.Bytes(), &line), logs.String())
		assert.ElementsMatch(t,
			[]string{"time", "level", "msg", "request_id", "method", "path", "status", "bytes", "duration", "remote"},
			slices.Collect(maps.Keys(line)), "what the handler adds, and the seven attributes of a request")
		assert.Equal(t, "/things", line["path"], "the path, without the query")
	})
}

func TestWrappersKeepResponseController(t *testing.T) {
	var flushErr, deadlineErr error
	h := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			rc := http.NewResponseController(w)
			flushErr = rc.Flush()
			deadlineErr = rc.SetWriteDeadline(time.Now().Add(time.Second))
		}),
		RequestID(), AccessLog(slog.New(slog.NewTextHandler(io.Discard, nil))), Recover(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	require.NoError(t, flushErr, "SSE needs Flush to reach the real writer")
	assert.ErrorIs(t, deadlineErr, http.ErrNotSupported, "the call must reach the underlying writer (a recorder has no deadlines)")
}

func TestSecurityHeaders(t *testing.T) {
	const wantCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
	const wantPermissions = "camera=(), microphone=(), geolocation=(), payment=(), usb=()"
	const wantHSTS = "max-age=31536000; includeSubDomains"

	tests := []struct {
		name     string
		hsts     bool
		path     string
		wantHSTS bool
	}{
		{"plain http", false, "/", false},
		{"https adds HSTS", true, "/", true},
		{"error responses carry the headers too", false, "/missing", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.Handle("GET /{$}", okHandler())
			h := SecurityHeaders(tc.hsts)(mux)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			hd := rec.Header()
			assert.Equal(t, wantCSP, hd.Get("Content-Security-Policy"))
			assert.Equal(t, "nosniff", hd.Get("X-Content-Type-Options"))
			assert.Equal(t, "same-origin", hd.Get("Referrer-Policy"))
			assert.Equal(t, "DENY", hd.Get("X-Frame-Options"))
			assert.Equal(t, "same-origin", hd.Get("Cross-Origin-Opener-Policy"))
			assert.Equal(t, "same-origin", hd.Get("Cross-Origin-Resource-Policy"))
			assert.Equal(t, wantPermissions, hd.Get("Permissions-Policy"), "the page uses no device it doesn't need")
			assert.Equal(t, "no-store", hd.Get("Cache-Control"),
				"a signed-in page must not be kept by the browser: the next person at this computer presses Back")
			if tc.wantHSTS {
				assert.Equal(t, wantHSTS, hd.Get("Strict-Transport-Security"), "for a year, and for every subdomain")
			} else {
				assert.Empty(t, hd.Get("Strict-Transport-Security"))
			}
		})
	}
}

// no-store is the default for every answer; the routes that are meant to be kept
// or are not pages say so themselves, later, and win.
func TestSecurityHeaders_ARoutesOwnCachePolicyWins(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /asset", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	})
	h := SecurityHeaders(false)(mux)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/asset", nil))

	assert.Equal(t, []string{"public, max-age=31536000, immutable"}, rec.Header().Values("Cache-Control"))
}

func TestCrossOrigin(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		headers    map[string]string
		wantStatus int
	}{
		{"cross-site POST is blocked", http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site (sibling subdomain) POST is blocked", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"same-origin POST passes", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"user-initiated POST passes", http.MethodPost, map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"non-browser POST without headers passes", http.MethodPost, nil, http.StatusOK},
		{"foreign Origin without fetch metadata is blocked", http.MethodPost, map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"matching Origin passes", http.MethodPost, map[string]string{"Origin": "http://app.test"}, http.StatusOK},
		{"configured public origin passes behind a proxy", http.MethodPost, map[string]string{"Origin": "https://chat.example.com"}, http.StatusOK},
		{"cross-site GET passes (safe method)", http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusOK},
		{"cross-site DELETE is blocked", http.MethodDelete, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mw, err := CrossOrigin("https://chat.example.com")
			require.NoError(t, err)
			req := httptest.NewRequest(tc.method, "http://app.test/c", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()

			mw(okHandler()).ServeHTTP(rec, req)

			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}

	t.Run("rejects an invalid public origin", func(t *testing.T) {
		_, err := CrossOrigin("not a url")
		assert.Error(t, err)
	})
}

func TestMaxBody(t *testing.T) {
	const limit = 10
	readAll := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			if writeBodyError(w, err) {
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	tests := []struct {
		name       string
		body       string
		chunked    bool
		wantStatus int
	}{
		{"small body passes", "12345", false, http.StatusOK},
		{"body exactly at the limit passes", strings.Repeat("a", limit), false, http.StatusOK},
		{"declared length over the limit is rejected early", strings.Repeat("a", limit+1), false, http.StatusRequestEntityTooLarge},
		{"chunked body over the limit is rejected while reading", strings.Repeat("a", limit+1), true, http.StatusRequestEntityTooLarge},
		{"no body passes", "", false, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			if tc.chunked {
				req.ContentLength = -1
			}
			rec := httptest.NewRecorder()

			MaxBody(limit)(readAll).ServeHTTP(rec, req)

			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

func TestHandler_ChainOrder(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /panic", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	mux.HandleFunc("POST /echo", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	log, logs := capturingLogger(slog.LevelInfo)
	cfg := config.Config{
		App:  config.App{BaseURL: "http://localhost:8300"},
		HTTP: config.HTTP{MaxBodyBytes: 1024},
	}
	h, err := Handler(cfg, log, mux)
	require.NoError(t, err)

	t.Run("a panic is a 500 that is logged with its status and still has headers and an id", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panic", nil))

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		id := rec.Header().Get("X-Request-ID")
		require.NotEmpty(t, id)
		assert.NotEmpty(t, rec.Header().Get("Content-Security-Policy"))
		assert.Contains(t, logs.String(), "kaboom")
		assert.Contains(t, logs.String(), "status=500")
		assert.Contains(t, logs.String(), "request_id="+id)
	})

	t.Run("cross-site POST is blocked before reaching the route", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/echo", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.NotEmpty(t, rec.Header().Get("Content-Security-Policy"), "denials carry the security headers")
	})

	t.Run("a body over the limit is refused before the route, and the refusal is logged with its id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(strings.Repeat("x", 2048)))
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		id := rec.Header().Get("X-Request-ID")
		require.NotEmpty(t, id)
		assert.NotEmpty(t, rec.Header().Get("Content-Security-Policy"), "denials carry the security headers")
		assert.Contains(t, logs.String(), "status=413")
		assert.Contains(t, logs.String(), "request_id="+id)
	})

	t.Run("a cross-site POST with a body over the limit is refused as cross-site", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(strings.Repeat("x", 2048)))
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code, "the cross-origin check comes before the body limit")
	})

	t.Run("HSTS only for https base URLs", func(t *testing.T) {
		https, err := Handler(config.Config{App: config.App{BaseURL: "https://chat.example.com"}, HTTP: config.HTTP{MaxBodyBytes: 1}}, log, mux)
		require.NoError(t, err)
		rec := httptest.NewRecorder()

		https.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nothing", nil))

		assert.NotEmpty(t, rec.Header().Get("Strict-Transport-Security"))
	})
}

// servedWithDeadlines runs handler behind the standard chain on a real server,
// with the given timeouts, and returns its address.
func servedWithDeadlines(t *testing.T, body, response time.Duration, handler http.Handler) string {
	t.Helper()
	return servedWithLimits(t, config.HTTP{MaxBodyBytes: 1 << 20, BodyTimeout: body, ResponseTimeout: response}, handler)
}

// servedWithLimits is servedWithDeadlines for a test that sets the body limit too.
func servedWithLimits(t *testing.T, limits config.HTTP, handler http.Handler) string {
	t.Helper()
	log, _ := capturingLogger(slog.LevelError)
	cfg := config.Config{App: config.App{BaseURL: "http://localhost:8300"}, HTTP: limits}
	h, err := Handler(cfg, log, handler)
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// Only the headers have a deadline in net/http once ReadHeaderTimeout has passed,
// so a client could hold a connection, and a goroutine, for as long as it liked
// by sending a body one byte a minute. A global ReadTimeout would cut every
// event stream, so the limit is on the body, and only while it is being read.
func TestDeadlines_ASlowBodyIsCutOff(t *testing.T) {
	failed := make(chan error, 1)
	addr := servedWithDeadlines(t, 200*time.Millisecond, time.Minute, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		failed <- err
		if err != nil {
			http.Error(w, "request body not received in time", http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = io.WriteString(conn, "POST /echo HTTP/1.1\r\nHost: x\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\nabcdef")
	require.NoError(t, err)
	started := time.Now()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	line, err := bufio.NewReader(conn).ReadString('\n')

	require.NoError(t, err, "the server answers instead of waiting for the rest of the body")
	assert.Contains(t, line, "408", "the answer is a timeout")
	assert.Less(t, time.Since(started), 2*time.Second)
	assert.Error(t, <-failed, "the handler's read failed")
}

// A request that is refused is answered without its body being read, and net/http
// reads what is left of a body before it sends an answer. The deadlines sit
// outside the two layers that refuse, the cross-origin check and the body limit,
// so that read ends when the body deadline passes: a client cannot hold a
// connection by being refused slowly.
func TestDeadlines_ARefusalDoesNotWaitForASlowBody(t *testing.T) {
	tests := []struct {
		name       string
		maxBody    int64
		header     string
		wantStatus string
	}{
		{"a request from another site", 1 << 20, "Sec-Fetch-Site: cross-site\r\n", "403"},
		{"a body over the limit", 10, "", "413"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var reached atomic.Bool
			limits := config.HTTP{MaxBodyBytes: tc.maxBody, BodyTimeout: 200 * time.Millisecond, ResponseTimeout: time.Minute}
			addr := servedWithLimits(t, limits, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
			conn, err := net.Dial("tcp", addr)
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			// A hundred bytes are promised, six are sent, and the rest never comes.
			_, err = io.WriteString(conn, "POST /echo HTTP/1.1\r\nHost: x\r\n"+tc.header+
				"Content-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\nabcdef")
			require.NoError(t, err)
			started := time.Now()

			require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
			line, err := bufio.NewReader(conn).ReadString('\n')

			require.NoError(t, err, "the server answers instead of waiting for the rest of the body")
			assert.Contains(t, line, tc.wantStatus, "the answer is the refusal")
			assert.Less(t, time.Since(started), 2*time.Second)
			assert.False(t, reached.Load(), "a refused request does not reach the route")
		})
	}
}

// The deadline is for receiving the body. Once it is in, the handler may take as
// long as it needs, and the request must not be cancelled behind its back (net/http
// cancels a request whose connection read fails while the handler runs).
func TestDeadlines_AHandlerMayTakeLongerThanTheBodyDeadlineOnceItHasTheBody(t *testing.T) {
	cancelled := make(chan bool, 1)
	addr := servedWithDeadlines(t, 150*time.Millisecond, time.Minute, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		time.Sleep(500 * time.Millisecond) // well past the body deadline
		cancelled <- r.Context().Err() != nil
		w.WriteHeader(http.StatusNoContent)
	}))
	resp, err := http.Post("http://"+addr+"/echo", "application/x-www-form-urlencoded", strings.NewReader("a=b"))
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.False(t, <-cancelled, "the request was not cancelled while the handler worked")
}

// A client that asks for a big page and then does not read it would hold the
// handler in its write for ever.
func TestDeadlines_AClientThatDoesNotReadIsCutOff(t *testing.T) {
	wrote := make(chan error, 1)
	addr := servedWithDeadlines(t, time.Minute, 300*time.Millisecond, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for range 64 { // far more than the sockets can hold
			if _, err := w.Write(chunk); err != nil {
				wrote <- err
				return
			}
		}
		wrote <- nil
	}))
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = io.WriteString(conn, "GET /big HTTP/1.1\r\nHost: x\r\n\r\n") // and never read
	require.NoError(t, err)

	select {
	case err := <-wrote:
		assert.Error(t, err, "the write gave up")
	case <-time.After(3 * time.Second):
		t.Fatal("the handler is still waiting for a client that is not reading")
	}
}

// The event stream sets a deadline for each write of its own, and goes on for as
// long as the reply does: neither deadline is for it. In particular a read
// deadline set on a request that has no body would, when it passed, make net/http
// cancel the request behind the handler's back.
func TestDeadlines_AStreamLivesLongerThanBothDeadlines(t *testing.T) {
	cancelled := make(chan bool, 1)
	addr := servedWithDeadlines(t, 200*time.Millisecond, 200*time.Millisecond, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		w.Header().Set("Content-Type", "text/event-stream")
		for range 4 {
			_ = rc.SetWriteDeadline(time.Now().Add(time.Second)) // as the SSE writer does for each write
			_, _ = io.WriteString(w, ": tick\n\n")
			_ = rc.Flush()
			time.Sleep(150 * time.Millisecond)
		}
		cancelled <- r.Context().Err() != nil
	}))
	resp, err := http.Get("http://" + addr + "/stream")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)

	require.NoError(t, err)
	assert.Equal(t, 4, strings.Count(string(body), ": tick"), "all four ticks arrived, over more than the deadlines")
	assert.False(t, <-cancelled, "and the request was not cancelled behind the handler's back")
}
