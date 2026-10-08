// Package finalorincomplete turns a stream that ends without its final event into an error.
package finalorincomplete

import (
	"errors"
	"iter"
)

// ErrIncomplete means the stream ended before an event for which isFinal is true.
var ErrIncomplete = errors.New("stream ended before its final event")

// Complete passes every event through. A clean end of the stream is not enough:
// when no event was final, it yields ErrIncomplete after the last event.
func Complete[T any](in iter.Seq2[T, error], isFinal func(T) bool) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		sawFinal := false
		for ev, err := range in {
			if err != nil {
				yield(ev, err)
				return
			}
			sawFinal = sawFinal || isFinal(ev)
			if !yield(ev, nil) {
				return
			}
		}
		if !sawFinal {
			var zero T
			yield(zero, ErrIncomplete)
		}
	}
}
