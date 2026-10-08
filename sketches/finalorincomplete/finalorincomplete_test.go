package finalorincomplete

import (
	"errors"
	"iter"
	"strings"
	"testing"
)

// source plays events and then ends cleanly, the way a transport does when a connection closes.
// told is set when the consumer stops before the source is done.
func source(told *bool, events ...string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		for _, ev := range events {
			if !yield(ev, nil) {
				*told = true
				return
			}
		}
	}
}

func isFinal(ev string) bool { return strings.HasPrefix(ev, "final:") }

func collect(in iter.Seq2[string, error]) (events []string, last error) {
	for ev, err := range in {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func TestACleanEndWithoutAFinalIsIncomplete(t *testing.T) {
	var told bool
	events, err := collect(Complete(source(&told, "delta:a", "delta:b"), isFinal))
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("a stream with no final ended with %v, want ErrIncomplete", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events before the error, want the 2 that arrived", len(events))
	}
	if _, err := collect(Complete(source(&told), isFinal)); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("an empty stream ended with %v, want ErrIncomplete", err)
	}
}

func TestAFinalMakesTheEndClean(t *testing.T) {
	var told bool
	events, err := collect(Complete(source(&told, "delta:a", "final:ab"), isFinal))
	if err != nil || len(events) != 2 {
		t.Fatalf("got %v and %v, want both events and no error", events, err)
	}
}

func TestAnErrorOfTheSourceIsPassedOnAndNothingIsAdded(t *testing.T) {
	boom := errors.New("boom")
	failing := func(yield func(string, error) bool) {
		if yield("delta:a", nil) {
			yield("", boom)
		}
	}
	events, err := collect(Complete(failing, isFinal))
	if !errors.Is(err, boom) || len(events) != 1 {
		t.Fatalf("got %v and %v, want the event and the error of the source", events, err)
	}
}

func TestStoppingEarlyTellsTheSource(t *testing.T) {
	var told bool
	for range Complete(source(&told, "delta:a", "delta:b", "final:ab"), isFinal) {
		break
	}
	if !told {
		t.Fatal("the source was not told to stop")
	}
}
