package lazybucket

import (
	"testing"
	"time"
)

func TestTheBucketRefillsFromTheTimeThatPassed(t *testing.T) {
	b := &Bucket{Rate: 2, Burst: 3}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	take := func(at time.Time) (bool, time.Duration) { return b.Allow(at) }

	for range 3 {
		if ok, _ := take(t0); !ok {
			t.Fatal("the burst was refused")
		}
	}
	if ok, wait := take(t0); ok || wait != 500*time.Millisecond {
		t.Fatalf("fourth request: %v, wait %v; want a refusal and 500ms", ok, wait)
	}
	if ok, wait := take(t0.Add(499 * time.Millisecond)); ok || wait != time.Millisecond {
		t.Fatalf("at 499ms: %v, wait %v; want a refusal and 1ms", ok, wait)
	}
	if ok, _ := take(t0.Add(500 * time.Millisecond)); !ok {
		t.Fatal("a token was due at 500ms")
	}
	if ok, _ := take(t0.Add(500 * time.Millisecond)); ok {
		t.Fatal("only one token came back")
	}
	admitted := 0
	for range 10 {
		if ok, _ := take(t0.Add(time.Hour)); ok {
			admitted++
		}
	}
	if admitted != 3 {
		t.Fatalf("after an hour %d requests passed; a rest never banks more than the burst", admitted)
	}
}
