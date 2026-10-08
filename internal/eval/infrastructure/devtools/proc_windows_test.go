//go:build windows

package devtools_test

import (
	"encoding/csv"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
)

// A server starts Chrome, and ending it must end the process, the same way
// killTree is asked to for the group a Unix process starts. ping, not
// timeout: timeout refuses to run at all without a real console ("Input
// redirection is not supported"), which a child process started this way
// never has.
func TestKillTree_EndsTheProcess(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "ping", "-n", "300", "127.0.0.1")
	devtools.Isolate(cmd)
	require.NoError(t, cmd.Start())
	require.True(t, processAlive(t, cmd.Process.Pid))

	devtools.KillTree(cmd)

	_ = cmd.Wait() // reaps it; until then it still shows as running
	waitGone(t, cmd.Process.Pid)
}

func TestKillTree_NothingToKill(t *testing.T) {
	devtools.KillTree(nil)
	devtools.KillTree(exec.Command("cmd", "/c", "exit")) // never started
}

// --- a real process ---------------------------------------------------------------

// processAlive asks tasklist, which knows about every process on the
// machine, not just this test's children. Its CSV output quotes every field
// (including MemUsage, which itself contains a comma, e.g. "1,234 K"), so the
// PID column is read with encoding/csv rather than matched as a substring.
func processAlive(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("tasklist", "/FO", "CSV", "/NH", "/FI", "PID eq "+strconv.Itoa(pid)).Output()
	require.NoError(t, err)
	records, err := csv.NewReader(strings.NewReader(string(out))).ReadAll()
	require.NoError(t, err)
	want := strconv.Itoa(pid)
	for _, rec := range records {
		if len(rec) > 1 && rec[1] == want {
			return true
		}
	}
	return false
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	require.Eventually(t, func() bool { return !processAlive(t, pid) }, 10*time.Second, 100*time.Millisecond,
		"process %d is still running", pid)
}
