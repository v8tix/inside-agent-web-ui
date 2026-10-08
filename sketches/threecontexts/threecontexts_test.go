package threecontexts

import (
	"context"
	"testing"
	"testing/synctest"
)

func TestTheClientLeavingEndsTheStreamAndTheRun(t *testing.T) {
	client, leave := context.WithCancel(t.Context())
	stream, run, release := Split(client, context.Background())
	defer release()

	leave()

	if stream.Err() == nil || run.Err() == nil {
		t.Fatalf("the client left: stream %v, run %v, want both ended", stream.Err(), run.Err())
	}
}

func TestAShutdownEndsOnlyTheRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		shutdown, shutDown := context.WithCancel(context.Background())
		stream, run, release := Split(t.Context(), shutdown)
		defer release()

		shutDown()
		synctest.Wait() // the hook runs in a goroutine of its own

		if run.Err() == nil {
			t.Fatal("the run goes on although the server is shutting down")
		}
		if stream.Err() != nil {
			t.Fatal("the stream ended with the run, so nobody can tell the client what happened")
		}
	})
}

func TestReleaseEndsBoth(t *testing.T) {
	stream, run, release := Split(t.Context(), context.Background())

	release()

	if stream.Err() == nil || run.Err() == nil {
		t.Fatalf("after release: stream %v, run %v, want both ended", stream.Err(), run.Err())
	}
}
