// Package runregistry keeps the jobs a process runs, one for each id.
package runregistry

import (
	"context"
	"sync"
)

// Registry maps a job id to the function that cancels the job.
type Registry struct{ jobs sync.Map }

// Begin refuses an id that already has a job; otherwise it returns the function that ends the job.
func (r *Registry) Begin(id string, cancel context.CancelFunc) (end func(), ok bool) {
	if _, busy := r.jobs.LoadOrStore(id, cancel); busy {
		return nil, false
	}
	return func() { r.jobs.Delete(id) }, true
}

// Cancel stops the job of id. False means that nothing runs the job.
func (r *Registry) Cancel(id string) bool {
	cancel, ok := r.jobs.Load(id)
	if ok {
		cancel.(context.CancelFunc)()
	}
	return ok
}
