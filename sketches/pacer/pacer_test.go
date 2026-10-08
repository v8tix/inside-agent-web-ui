package pacer

import (
	"testing"
	"time"
)

var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return start.Add(time.Duration(ms) * time.Millisecond) }

func TestTheFirstGoesAtOnceAndABurstBecomesOneUpdate(t *testing.T) {
	p := &Pacer{Base: 100 * time.Millisecond, Factor: 4}

	if got, ok := p.Offer(at(0), "a"); !ok || got != "a" {
		t.Fatalf("the first text must go at once, got %q %v", got, ok)
	}
	p.Sent(at(0), 0)
	if _, ok := p.Offer(at(10), "ab"); ok {
		t.Fatal("10 ms after a send is too soon")
	}
	if _, ok := p.Offer(at(20), "abc"); ok {
		t.Fatal("20 ms after a send is too soon")
	}
	if _, ok := p.Due(at(99)); ok {
		t.Fatal("the interval has not passed")
	}
	if got, ok := p.Due(at(100)); !ok || got != "abc" {
		t.Fatalf("the update after the interval must carry the newest text, got %q %v", got, ok)
	}
	p.Sent(at(100), 0)
	if _, ok := p.Due(at(500)); ok {
		t.Fatal("nothing is waiting, so nothing is due")
	}
}

func TestACostlySendStretchesTheInterval(t *testing.T) {
	p := &Pacer{Base: 100 * time.Millisecond, Factor: 4}
	p.Offer(at(0), "a")
	p.Sent(at(50), 50*time.Millisecond) // the send took 50 ms: 4 x 50 ms is more than the base

	p.Offer(at(60), "ab")

	if _, ok := p.Due(at(249)); ok {
		t.Fatal("199 ms after a send that cost 50 ms is too soon: the interval is 200 ms")
	}
	if got, ok := p.Due(at(250)); !ok || got != "ab" {
		t.Fatalf("200 ms after the send the text must go, got %q %v", got, ok)
	}
}

func TestACheapSendKeepsTheBase(t *testing.T) {
	p := &Pacer{Base: 100 * time.Millisecond, Factor: 4}
	p.Offer(at(0), "a")
	p.Sent(at(5), 5*time.Millisecond) // 4 x 5 ms is less than the base

	p.Offer(at(10), "ab")

	if _, ok := p.Due(at(104)); ok {
		t.Fatal("the base is the floor")
	}
	if _, ok := p.Due(at(105)); !ok {
		t.Fatal("one base interval after the send the text must go")
	}
}
