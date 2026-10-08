//go:build unix

package evalselftest_test

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/run"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/store"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/synthetic"
	"github.com/v8tix/inside-agent-web-ui/it/internal/evalenv"
)

// EC-6, with a real browser: Ctrl-C leaves a valid results file, and running the
// same run again skips what is finished and ends with the results the golden
// file says.
func TestSelftest_InterruptedThenResumed(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := selftestEnv(t)
	expected, err := synthetic.Expected()
	require.NoError(t, err)
	st := store.New(t.TempDir())
	runner := selftestRunner(t, env, st, nil)
	req := selftestRequest(env, "interrupted")

	ctx, ctrlC := context.WithCancel(t.Context())
	defer ctrlC()
	go func() { // Ctrl-C once a few attempts are on disk
		for ctx.Err() == nil {
			if rows, err := st.Load(ctx, req.RunID); err == nil && len(rows) >= 4 {
				ctrlC()
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	first, err := runner.Run(ctx, req)

	require.NoError(t, err, "an interrupted run is not a failed one")
	assert.True(t, first.Interrupted)
	done, err := st.Load(t.Context(), req.RunID)
	require.NoError(t, err, "every line of the results file is whole")
	assert.GreaterOrEqual(t, len(done), 4)
	assert.Less(t, len(done), 13, "and it stopped before the end")
	started, err := st.LoadMeta(t.Context(), req.RunID)
	require.NoError(t, err)
	assert.Zero(t, started.FinishedAt, "it is not marked as finished")

	second, err := runner.Run(t.Context(), req) // the same run id: --resume

	require.NoError(t, err)
	assert.False(t, second.Interrupted)
	all, err := st.Load(t.Context(), req.RunID)
	require.NoError(t, err)
	require.Len(t, all, 13, "every attempt exactly once")
	assert.Equal(t, done, all[:len(done)], "what was finished is as it was; nothing was asked again")
	keys := map[string]bool{}
	for _, r := range all {
		assert.False(t, keys[r.Key().String()], "%s once", r.Key())
		keys[r.Key().String()] = true
	}
	assert.Empty(t, expected.Compare(second.Summary), "and the whole run finds what the golden file says")
	finished, err := st.LoadMeta(t.Context(), req.RunID)
	require.NoError(t, err)
	assert.NotZero(t, finished.FinishedAt)
	assert.Equal(t, started.StartedAt, finished.StartedAt, "a resumed run keeps its start")
}

// EC-6: a browser that is killed in the middle of a run is replaced, and the
// attempt it was in is tried again. The run ends as if nothing had happened.
func TestSelftest_BrowsersThatDieAreReplaced(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := selftestEnv(t)
	expected, err := synthetic.Expected()
	require.NoError(t, err)
	st := store.New(t.TempDir())
	var logs syncBuffer
	runner := selftestRunner(t, env, st, slog.New(slog.NewTextHandler(&logs, nil)))
	req := selftestRequest(env, "killed")

	var killed atomic.Int64
	go func() { // once a first attempt has been through a browser, every browser goes at the same time
		for t.Context().Err() == nil {
			rows, err := st.Load(t.Context(), req.RunID)
			if err == nil && slices.ContainsFunc(rows, func(r domain.Result) bool { return r.Status != domain.StatusSkipped }) {
				killed.Store(int64(killBrowsers(t)))
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	out, err := runner.Run(t.Context(), req)

	require.NoError(t, err)
	assert.Positive(t, killed.Load(), "there were browsers to kill")
	assert.Contains(t, logs.String(), "the browser session ended; starting another and trying again")
	assert.NotContains(t, logs.String(), "the browser session ended again", "one replacement was enough")
	assert.Empty(t, expected.Compare(out.Summary), "and the results are the golden file's all the same")
	rows, err := st.Load(t.Context(), req.RunID)
	require.NoError(t, err)
	require.Len(t, rows, 13)
	for _, r := range rows {
		assert.NotEqual(t, run.ClassDriverDied, r.ErrorClass, "%s", r.Key())
		assert.NotEqual(t, domain.StatusUIError, r.Status, "%s", r.Key())
	}
}

// killBrowsers kills the Chrome DevTools MCP servers this test started, each
// with the Chrome it started, and returns how many there were. They are the
// children of the test process, and each is the leader of its own process group.
func killBrowsers(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid())).Output()
	if err != nil { // pgrep exits 1 when there is nothing to find
		return 0
	}
	var killed int
	for pid := range strings.FieldsSeq(string(out)) {
		n, err := strconv.Atoi(pid)
		if err != nil {
			continue
		}
		if pgid, err := syscall.Getpgid(n); err == nil && pgid == n {
			if syscall.Kill(-pgid, syscall.SIGKILL) == nil {
				killed++
			}
		}
	}
	return killed
}

// syncBuffer is a log that several workers write to.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
