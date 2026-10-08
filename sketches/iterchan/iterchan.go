// Package iterchan puts an iterator behind a channel, so that one loop can wait on its items and on the clock.
package iterchan

import (
	"context"
	"fmt"
	"iter"
)

// Feed ranges over seq in a goroutine of its own and hands each value over. When ctx ends, the
// goroutine returns, which stops seq. A panic of seq ends only this stream: the channel is closed,
// and failed, which is read after the channel is closed, returns the panic as an ordinary error.
func Feed[T any](ctx context.Context, seq iter.Seq[T]) (items <-chan T, failed func() error) {
	out, err := make(chan T), error(nil)
	go func() {
		defer close(out)
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("the stream panicked: %v", rec)
			}
		}()
		for v := range seq {
			select {
			case out <- v:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, func() error { return err }
}
