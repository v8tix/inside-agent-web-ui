package runregistry

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRegistryKnowsWhoIsRunning(t *testing.T) {
	var r Registry
	ctx, cancel := context.WithCancel(t.Context())
	end, ok := r.Begin("reply-1", cancel)
	if !ok {
		t.Fatal("the first Begin was refused")
	}
	if _, ok := r.Begin("reply-1", cancel); ok {
		t.Fatal("a second job for the same id was accepted")
	}
	if !r.Cancel("reply-1") || ctx.Err() == nil {
		t.Fatal("Cancel did not stop the job")
	}
	end()
	if r.Cancel("reply-1") {
		t.Fatal("Cancel found a job that has ended; a repair path would wait for nothing")
	}
	if _, ok := r.Begin("reply-1", cancel); !ok {
		t.Fatal("the id was not free after the job ended")
	}
}

func TestExactlyOneOfManyWins(t *testing.T) {
	var (
		r       Registry
		wg      sync.WaitGroup
		started atomic.Int32
	)
	for range 50 {
		wg.Go(func() {
			if _, ok := r.Begin("reply-2", func() {}); ok {
				started.Add(1)
			}
		})
	}
	wg.Wait()
	if n := started.Load(); n != 1 {
		t.Fatalf("%d jobs started for one id, want 1", n)
	}
}
