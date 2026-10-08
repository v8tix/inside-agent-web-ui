package run

import "context"

// HandOutForTest feeds n slots to one worker that is already waiting for a slot
// when the feeding starts, and returns how many of them the worker got. waiting
// returns once the worker waits (in a synctest bubble, synctest.Wait).
func HandOutForTest(ctx context.Context, n int, waiting func()) int {
	queue := make(chan slot)
	got := make(chan int)
	go func() {
		count := 0
		for range queue {
			count++
		}
		got <- count
	}()
	waiting()
	feed(ctx, queue, make([]slot, n))
	close(queue)
	return <-got
}
