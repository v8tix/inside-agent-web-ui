// Package leak fails a test that leaves behind goroutines of the application:
// a stream nobody reads, a run nobody cancelled, a pool nobody closed.
package leak

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// ours are the marks of a goroutine that belongs to the application or to the
// libraries it starts goroutines in, in a stack dump (which names the function
// that started each goroutine too).
var ours = []string{
	"github.com/v8tix/inside-agent-web-ui/internal/",
	"github.com/v8tix/inside-agent-web-ui/cmd/",
	"google.golang.org/adk/",
	"github.com/jackc/pgx/",
}

// settle is how long a goroutine may take to notice that its work is over.
const settle = 5 * time.Second

// Check makes the test fail if goroutines of the application are still running
// after the test and its cleanups. Call it first: cleanups run last to first,
// so this one runs when everything else has been shut down.
func Check(t testing.TB) {
	t.Helper()
	t.Cleanup(func() {
		deadline := time.Now().Add(settle)
		for {
			leaked := Running()
			if len(leaked) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("goroutines of the application are still running after the test:\n\n%s", strings.Join(leaked, "\n\n"))
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	})
}

// Running returns the stacks of the goroutines of the application that are
// running now.
func Running() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var found []string
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		for _, mark := range ours {
			if strings.Contains(g, mark) {
				found = append(found, g)
				break
			}
		}
	}
	return found
}
