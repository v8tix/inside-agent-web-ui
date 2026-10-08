package chat

import (
	"context"
	"sync"
	"uuid"
)

// activeRun is a reply being written right now.
type activeRun struct {
	cancel context.CancelFunc
	done   chan struct{} // closed once the outcome is saved
}

// runs keeps the replies this process is writing, so a stop request can end
// one. Memory is the right place for it: a reply is written by the process that
// claimed it, and the claim in the database already keeps the others away.
//
// Pattern: Run Registry
type runs struct {
	mu     sync.Mutex
	active map[uuid.UUID]*activeRun
}

// begin registers a run for the message. It reports false when the message
// already has one.
func (r *runs) begin(id uuid.UUID, cancel context.CancelFunc) (*activeRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, busy := r.active[id]; busy {
		return nil, false
	}
	if r.active == nil {
		r.active = make(map[uuid.UUID]*activeRun)
	}
	run := &activeRun{cancel: cancel, done: make(chan struct{})}
	r.active[id] = run
	return run, true
}

// end forgets the run and wakes up whoever waits for it.
func (r *runs) end(id uuid.UUID, run *activeRun) {
	r.mu.Lock()
	delete(r.active, id)
	r.mu.Unlock()
	close(run.done)
}

func (r *runs) get(id uuid.UUID) (*activeRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.active[id]
	return run, ok
}
