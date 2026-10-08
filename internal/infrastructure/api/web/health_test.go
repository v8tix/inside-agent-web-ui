package web

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pass(context.Context) error { return nil }

func failWith(msg string) func(context.Context) error {
	return func(context.Context) error { return errors.New(msg) }
}

func readyz(t *testing.T, checks ...ReadinessCheck) (*httptest.ResponseRecorder, readiness) {
	t.Helper()
	app := testAppWith(t, Services{}, Options{ReadinessChecks: checks})
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil)) // no session: it is public
	var body readiness
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return rec, body
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name       string
		checks     []ReadinessCheck
		wantStatus int
		wantBody   readiness
	}{
		{
			name:       "everything answers",
			checks:     []ReadinessCheck{{"database", pass}, {"agent", pass}},
			wantStatus: http.StatusOK,
			wantBody:   readiness{Status: "ok", Checks: []checkResult{{"database", "ok"}, {"agent", "ok"}}},
		},
		{
			name:       "the database does not",
			checks:     []ReadinessCheck{{"database", failWith("connection refused")}, {"agent", pass}},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   readiness{Status: "unavailable", Checks: []checkResult{{"database", "failed"}, {"agent", "ok"}}},
		},
		{
			name:       "the agent does not",
			checks:     []ReadinessCheck{{"database", pass}, {"agent", failWith("agent answered 502")}},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   readiness{Status: "unavailable", Checks: []checkResult{{"database", "ok"}, {"agent", "failed"}}},
		},
		{
			name:       "neither does",
			checks:     []ReadinessCheck{{"database", failWith("x")}, {"agent", failWith("y")}},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   readiness{Status: "unavailable", Checks: []checkResult{{"database", "failed"}, {"agent", "failed"}}},
		},
		{
			name:       "nothing to check is ready",
			wantStatus: http.StatusOK,
			wantBody:   readiness{Status: "ok", Checks: []checkResult{}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := readyz(t, tc.checks...)

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, tc.wantBody, body)
			assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"), "a probe must never be cached")
		})
	}

	t.Run("the reason stays in the log, not in the public answer", func(t *testing.T) {
		secret := "dial tcp 10.1.2.3:5432: password=hunter2 authentication failed"
		rec, _ := readyz(t, ReadinessCheck{"database", failWith(secret)})

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.NotContains(t, rec.Body.String(), "hunter2")
		assert.NotContains(t, rec.Body.String(), "10.1.2.3")
	})
}

func TestReadyz_Checks(t *testing.T) {
	t.Run("they run side by side", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			slow := func(ctx context.Context) error {
				select {
				case <-time.After(time.Second):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			start := time.Now()

			rec, _ := readyz(t, ReadinessCheck{"database", slow}, ReadinessCheck{"agent", slow})

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, time.Second, time.Since(start), "two one-second checks take one second, not two")
		})
	})

	t.Run("a dependency that hangs makes the probe say so after two seconds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			hangs := func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			}
			start := time.Now()

			rec, body := readyz(t, ReadinessCheck{"database", pass}, ReadinessCheck{"agent", hangs})

			assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
			assert.Equal(t, readinessTimeout, time.Since(start))
			assert.Equal(t, []checkResult{{"database", "ok"}, {"agent", "failed"}}, body.Checks)
		})
	})

	// The endpoint is public and every answer is a database round trip and a call
	// to the agent, so a loop asking a thousand times a second would load both. An
	// answer is kept for a second, which is far fresher than any probe asks.
	t.Run("an answer is kept for a second, and then each check is asked again", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var calls atomic.Int32
			app := testAppWith(t, Services{}, Options{ReadinessChecks: []ReadinessCheck{
				{"database", func(context.Context) error { calls.Add(1); return nil }},
			}})
			ask := func() {
				app.Routes().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
			}

			ask()
			ask()
			time.Sleep(900 * time.Millisecond)
			ask()
			assert.EqualValues(t, 1, calls.Load(), "three asks within a second were one check")

			time.Sleep(200 * time.Millisecond)
			ask()
			assert.EqualValues(t, 2, calls.Load(), "and a probe that comes later is not given a stale answer")
		})
	})

	t.Run("a failure is kept too, so a dependency that is down is not hammered", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var calls atomic.Int32
			app := testAppWith(t, Services{}, Options{ReadinessChecks: []ReadinessCheck{
				{"database", func(context.Context) error { calls.Add(1); return errors.New("down") }},
			}})
			var codes []int
			for range 3 {
				rec := httptest.NewRecorder()
				app.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
				codes = append(codes, rec.Code)
			}

			assert.Equal(t, []int{503, 503, 503}, codes)
			assert.EqualValues(t, 1, calls.Load())
		})
	})

	// Real time, not a synctest bubble: callers waiting on a lock are not blocked
	// in the way a bubble can see, so it would never move its clock on.
	t.Run("callers at the same moment share one run of the checks", func(t *testing.T) {
		var calls atomic.Int32
		app := testAppWith(t, Services{}, Options{ReadinessChecks: []ReadinessCheck{
			{"agent", func(ctx context.Context) error {
				calls.Add(1)
				time.Sleep(150 * time.Millisecond) // a slow dependency
				return nil
			}},
		}})
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				rec := httptest.NewRecorder()
				app.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
				assert.Equal(t, http.StatusOK, rec.Code)
			})
		}
		wg.Wait()

		assert.EqualValues(t, 1, calls.Load())
	})

	t.Run("a visitor who leaves in the middle does not spoil the answer for the others", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			app := testAppWith(t, Services{}, Options{ReadinessChecks: []ReadinessCheck{
				{"database", func(ctx context.Context) error {
					time.Sleep(100 * time.Millisecond)
					return ctx.Err() // a check that stops when the request does
				}},
			}})
			gone, cancel := context.WithCancel(t.Context())
			first := httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(gone)
			time.AfterFunc(50*time.Millisecond, cancel)
			app.Routes().ServeHTTP(httptest.NewRecorder(), first)

			rec := httptest.NewRecorder()
			app.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			assert.Equal(t, http.StatusOK, rec.Code, "the checks ran for everyone, not for the request that gave up")
		})
	})
}

// A check runs on its own goroutine, out of the recovery middleware's reach, so a
// panic in one would take the whole server down. /readyz is public.
func TestReadyz_APanickingCheckIsAFailedCheck(t *testing.T) {
	app := testAppWith(t, Services{}, Options{ReadinessChecks: []ReadinessCheck{
		{"database", pass},
		{"agent", func(context.Context) error { panic("kaboom") }},
	}})
	log, logs := capturingLogger(slog.LevelInfo)
	app.log = log
	rec := httptest.NewRecorder()

	app.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var body readiness
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	assert.Equal(t, []checkResult{{"database", "ok"}, {"agent", "failed"}}, body.Checks)
	assert.NotContains(t, rec.Body.String(), "kaboom", "the public answer names what failed and nothing else")
	assert.Contains(t, logs.String(), "kaboom")
	assert.Contains(t, logs.String(), "stack=", "the log has what is needed to find the bug")
}
