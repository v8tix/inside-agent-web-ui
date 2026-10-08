package iterchan

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestOneLoopWaitsOnTheItemsAndOnTheClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slow := func(yield func(string) bool) {
			for _, v := range []string{"a", "b"} {
				time.Sleep(40 * time.Second) // an iterator that is silent for a long time
				if !yield(v) {
					return
				}
			}
		}
		items, failed := Feed(t.Context(), slow)
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()

		var seen []string
		for open := true; open; {
			select {
			case v, ok := <-items:
				if open = ok; ok {
					seen = append(seen, v)
				}
			case <-tick.C:
				seen = append(seen, "tick")
			}
		}

		if want := []string{"tick", "tick", "a", "tick", "tick", "tick", "b"}; !slices.Equal(seen, want) {
			t.Fatalf("the loop saw %q, want %q", seen, want)
		}
		if err := failed(); err != nil {
			t.Fatalf("the stream ended well, but failed says: %v", err)
		}
	})
}

func TestAPanicEndsOnlyThisStream(t *testing.T) {
	broken := func(yield func(int) bool) {
		yield(1)
		panic("boom")
	}
	items, failed := Feed(t.Context(), broken)

	var seen []int
	for v := range items {
		seen = append(seen, v)
	}

	if !slices.Equal(seen, []int{1}) {
		t.Fatalf("the consumer saw %v, want [1]", seen)
	}
	if err := failed(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("the panic did not arrive as an error: %v", err)
	}
}

func TestLeavingStopsTheIterator(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stopped := make(chan struct{})
		endless := func(yield func(int) bool) {
			defer close(stopped)
			for i := 0; yield(i); i++ {
			}
		}
		ctx, leave := context.WithCancel(t.Context())
		items, _ := Feed(ctx, endless)

		<-items // the consumer takes one item and goes away
		leave()

		<-stopped // a send that cannot be given up would block here for ever, and the test would report a deadlock
	})
}
