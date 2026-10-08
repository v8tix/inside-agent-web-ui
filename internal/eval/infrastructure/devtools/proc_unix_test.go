//go:build unix

package devtools_test

import (
	"bufio"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
)

func init() {
	extraTools["pgid"] = func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
		return textResult(strconv.Itoa(syscall.Getpgrp())), nil
	}
}

// A server starts Chrome, and Chrome has helpers of its own. Ending the server
// has to end them all, or a long run leaves browsers behind.
func TestKillTree_EndsTheWholeGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 300 & echo $!; wait")
	devtools.Isolate(cmd)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	line, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err)
	child, err := strconv.Atoi(strings.TrimSpace(line))
	require.NoError(t, err)
	require.True(t, processAlive(child))
	require.True(t, processAlive(cmd.Process.Pid))

	devtools.KillTree(cmd)

	_ = cmd.Wait() // reaps the shell; until then it is a zombie and still "exists"
	waitGone(t, child)
	waitGone(t, cmd.Process.Pid)
	assert.Equal(t, -1, cmd.ProcessState.ExitCode(), "it was killed by a signal, not asked to leave")
}

func TestKillTree_NothingToKill(t *testing.T) {
	devtools.KillTree(nil)
	devtools.KillTree(exec.Command("true")) // never started
}

// --- a real process ---------------------------------------------------------------

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitGone(t *testing.T, pid int, within ...time.Duration) {
	t.Helper()
	limit := 10 * time.Second
	if len(within) > 0 {
		limit = within[0]
	}
	require.Eventually(t, func() bool { return !processAlive(pid) }, limit, 20*time.Millisecond, "process %d is still running", pid)
}

func callPID(t *testing.T, s *devtools.Session) int {
	t.Helper()
	res, err := s.Call(t.Context(), "pid", nil)
	require.NoError(t, err)
	pid, err := strconv.Atoi(res.Text)
	require.NoError(t, err)
	return pid
}

func TestOpen_StartsAProcessAndStopsIt(t *testing.T) {
	t.Setenv("HF_TOKEN", "CANARY-token")
	t.Setenv("EVAL_PASSWORD", "CANARY-password")
	t.Setenv("DB_URL", "postgres://u:CANARY-db@h/d")
	t.Setenv("NODE_OPTIONS", "--no-warnings")
	ctx, cancel := context.WithCancel(t.Context())

	s, err := devtools.Open(ctx, processOptions(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	cancel() // the context that started it is not the one that keeps it alive

	t.Run("it introduces itself and works after the start context ended", func(t *testing.T) {
		assert.Equal(t, "1.10.1", s.Info().Version)
		got, err := s.Call(t.Context(), "echo", map[string]any{"pageId": 3})
		require.NoError(t, err)
		assert.JSONEq(t, `{"pageId":3}`, got.Text)
	})
	t.Run("it does not see the evaluator's secrets", func(t *testing.T) {
		got, err := s.Call(t.Context(), "env", nil)

		require.NoError(t, err)
		names := strings.Split(got.Text, "\n")
		assert.NotContains(t, names, "HF_TOKEN")
		assert.NotContains(t, names, "EVAL_PASSWORD")
		assert.NotContains(t, names, "DB_URL")
		assert.Contains(t, names, "PATH")
		assert.Contains(t, names, "NODE_OPTIONS")
		assert.NotContains(t, got.Text, "CANARY")
	})
	t.Run("it runs in a process group of its own, out of reach of the terminal's Ctrl-C", func(t *testing.T) {
		got, err := s.Call(t.Context(), "pgid", nil)

		require.NoError(t, err)
		assert.NotEqual(t, strconv.Itoa(syscall.Getpgrp()), got.Text)
	})
	t.Run("closing it ends the process", func(t *testing.T) {
		pid := callPID(t, s)
		require.True(t, processAlive(pid))

		require.NoError(t, s.Close(t.Context()))

		waitGone(t, pid)
		require.NoError(t, s.Close(t.Context()), "and closing again is harmless")
	})
}

func TestOpen_Failures(t *testing.T) {
	t.Run("a command that is not pinned never starts", func(t *testing.T) {
		opts := processOptions(t)
		opts.Command = []string{fakeNpx(t), "-y", "chrome-devtools-mcp@latest"}

		s, err := devtools.Open(t.Context(), opts)

		require.ErrorContains(t, err, "must pin a version")
		assert.Nil(t, s)
	})
	t.Run("a program that is not there", func(t *testing.T) {
		opts := processOptions(t)
		opts.Command = []string{"/no/such/dir/npx", "-y", "chrome-devtools-mcp@1.10.1"}

		_, err := devtools.Open(t.Context(), opts)

		require.ErrorContains(t, err, "starting Chrome DevTools MCP (/no/such/dir/npx -y chrome-devtools-mcp@1.10.1)")
	})
	t.Run("a server that quits says why", func(t *testing.T) {
		_, err := devtools.Open(t.Context(), processOptions(t, "exit-at-start"))

		require.Error(t, err)
		assert.ErrorContains(t, err, "starting Chrome DevTools MCP")
		assert.ErrorContains(t, err, "its output ended with: fatal: refusing to start")
	})
	t.Run("a server that never answers is given up on and stopped", func(t *testing.T) {
		opts := processOptions(t, "silent")
		opts.StartTimeout = 300 * time.Millisecond
		start := time.Now()

		_, err := devtools.Open(t.Context(), opts)

		require.Error(t, err)
		assert.Less(t, time.Since(start), 3*time.Second, "it was killed when the time was up, not after the polite wait")
		_, after, found := strings.Cut(err.Error(), "pid=")
		require.True(t, found, "the server printed its pid: %v", err)
		pid, convErr := strconv.Atoi(strings.TrimSpace(strings.SplitN(after, "\n", 2)[0]))
		require.NoError(t, convErr)
		waitGone(t, pid)
	})
	t.Run("a server that dies in the middle of a call", func(t *testing.T) {
		s, err := devtools.Open(t.Context(), processOptions(t))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close(context.Background()) })

		_, err = s.Call(t.Context(), "crash", nil)

		require.ErrorIs(t, err, domain.ErrDriverDied)
		assert.ErrorContains(t, err, "its output ended with: fatal: simulated crash")
		_, err = s.Call(t.Context(), "echo", nil)
		require.ErrorIs(t, err, domain.ErrDriverDied)
	})
}

func TestClose_ADeadlineKillsWhatWillNotLeave(t *testing.T) {
	s, err := devtools.Open(t.Context(), processOptions(t, "stubborn"))
	require.NoError(t, err)
	pid := callPID(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	err = s.Close(ctx)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	waitGone(t, pid, 3*time.Second) // not after the polite wait of five seconds and a SIGTERM it ignores
}
