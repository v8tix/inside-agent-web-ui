package web

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// readinessTimeout bounds each check, so a hung dependency makes /readyz answer
// "not ready" rather than hang.
const readinessTimeout = 2 * time.Second

// ReadinessCheck is one dependency /readyz looks at.
type ReadinessCheck struct {
	Name  string
	Check func(ctx context.Context) error
}

type readiness struct {
	Status string        `json:"status"`
	Checks []checkResult `json:"checks"`
}

type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// readinessTTL is how long an answer to /readyz is kept. The endpoint is public
// and each answer costs a database round trip and a call to the agent, so asking
// it a thousand times a second must not do that a thousand times a second. A
// second is far fresher than any probe needs.
const readinessTTL = time.Second

// readyAnswer is one run of the checks, as it is sent.
type readyAnswer struct {
	body   []byte
	status int
}

// readyCache holds the latest answer. The lock is held while the checks run, so
// callers that arrive meanwhile wait for that run instead of starting their own.
type readyCache struct {
	mu     sync.Mutex
	at     time.Time
	answer readyAnswer
}

// readyz is the readiness probe: 200 when every dependency answers, 503 when
// one does not. The body names what failed and nothing else; the reason goes to
// the log, because this endpoint is public.
func (a *App) readyz(w http.ResponseWriter, r *http.Request) {
	answer, err := a.readiness(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(answer.status)
	_, _ = w.Write(answer.body)
}

// readiness gives the latest answer, running the checks if it is older than
// readinessTTL.
func (a *App) readiness(ctx context.Context) (readyAnswer, error) {
	a.ready.mu.Lock()
	defer a.ready.mu.Unlock()
	if !a.ready.at.IsZero() && time.Since(a.ready.at) < readinessTTL {
		return a.ready.answer, nil
	}
	// The answer is for everyone who asks in the next second, so the checks do
	// not stop when the request that started them does.
	answer, err := a.runChecks(context.WithoutCancel(ctx))
	if err != nil {
		return readyAnswer{}, err
	}
	a.ready.at, a.ready.answer = time.Now(), answer
	return answer, nil
}

func (a *App) runChecks(ctx context.Context) (readyAnswer, error) {
	checks := a.opts.ReadinessChecks
	errs := make([]error, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
			defer cancel()
			// A check runs out of the recovery middleware's reach, and a panic on
			// this goroutine would end the process. This endpoint is public.
			defer func() {
				if rec := recover(); rec != nil {
					logPanic(ctx, a.log, rec, "check", c.Name)
					errs[i] = fmt.Errorf("the check panicked: %v", rec)
				}
			}()
			errs[i] = c.Check(ctx)
		})
	}
	wg.Wait()

	body := readiness{Status: "ok", Checks: make([]checkResult, len(checks))}
	for i, c := range checks {
		body.Checks[i] = checkResult{Name: c.Name, Status: "ok"}
		if errs[i] != nil {
			body.Status, body.Checks[i].Status = "unavailable", "failed"
			a.log.WarnContext(ctx, "readiness check failed", "check", c.Name, "error", errs[i])
		}
	}
	out, err := json.Marshal(body)
	if err != nil {
		return readyAnswer{}, err
	}
	status := http.StatusOK
	if body.Status != "ok" {
		status = http.StatusServiceUnavailable
	}
	return readyAnswer{body: out, status: status}, nil
}
