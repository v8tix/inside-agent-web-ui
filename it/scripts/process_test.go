// The shell helpers the scripts share, run for real in bash. No Docker needed.
package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lib = "../../scripts/lib/process.sh"

// shell runs a bash snippet that has sourced the library, and returns what it
// printed and the exit code.
func shell(t *testing.T, env []string, snippet string) (string, int) {
	t.Helper()
	return shellIn(t, "", env, snippet)
}

// shellIn is shell, run in a directory ("" is this one).
func shellIn(t *testing.T, dir string, env []string, snippet string) (string, int) {
	t.Helper()
	abs, err := filepath.Abs(lib)
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), "bash", "-c", "source '"+abs+"'; "+snippet)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return strings.TrimSpace(string(out)), exit.ExitCode()
	}
	require.NoError(t, err)
	return strings.TrimSpace(string(out)), 0
}

// A settings file is read by a bash started for it, which is what makes the
// reading as strict under one bash as under another (see read_setting). One thing
// shows that shell whatever the bash is: the file sees the environment, and not a
// variable that the script which asks has only set for itself.
func TestReadSetting_TheFileIsReadByAShellOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.env"), []byte("A=$WHERE\n"), 0o600))

	t.Run("a variable of the script is not there for the file", func(t *testing.T) {
		out, code := shellIn(t, dir, nil, `WHERE=in-the-script; read_setting settings.env A`)

		assert.Equal(t, 1, code, out)
		assert.Contains(t, out, "cannot read the settings in settings.env")
		assert.NotContains(t, out, "in-the-script")
	})
	t.Run("a variable of the environment is", func(t *testing.T) {
		out, code := shellIn(t, dir, []string{"WHERE=in-the-environment"}, `read_setting settings.env A`)

		assert.Zero(t, code, out)
		assert.Equal(t, "in-the-environment", out)
	})
	// BASH_ENV names a file that a bash which is not interactive reads when it
	// starts. One written for an ordinary shell need not be clean under set -eu,
	// and what it prints is no part of a setting, so the shell that reads the
	// settings file is started without it.
	t.Run("the file a shell is told to read at its start is not read", func(t *testing.T) {
		startup := filepath.Join(dir, "startup.sh")
		require.NoError(t, os.WriteFile(startup, []byte("echo from-the-startup-file\n: \"$NOT_SET_IN_ANY_SHELL\"\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "plain.env"), []byte("A=1\n"), 0o600))

		out, code := shellIn(t, dir, nil, `export BASH_ENV='`+startup+`'; read_setting plain.env A`)

		assert.Zero(t, code, out)
		assert.Equal(t, "1", out)
	})
}

// A pid file outlives the process it was written for, and the number can be
// given to an unrelated one. Stopping by pid alone would signal that one.
func TestIsProcess(t *testing.T) {
	sleeper := exec.CommandContext(t.Context(), "sleep", "60")
	require.NoError(t, sleeper.Start())
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	pid := strconv.Itoa(sleeper.Process.Pid)

	tests := []struct {
		name    string
		pid     string
		program string
		want    int
	}{
		{"a running process with that name", pid, "sleep", 0},
		{"a running process with another name", pid, "agent-web-ui", 1},
		{"a pid nothing runs under", "999999", "sleep", 1},
		{"no pid at all", "", "sleep", 1},
		{"a name with characters a pattern would read", pid, "sl.*p", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, code := shell(t, nil, `is_process "`+tc.pid+`" "`+tc.program+`"`)

			assert.Equal(t, tc.want, code)
		})
	}
}

// fakeLsof writes a lsof that prints out, and returns the env that makes the
// library use it.
func fakeLsof(t *testing.T, out string) []string {
	t.Helper()
	dir := t.TempDir()
	printed := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(printed, []byte(out), 0o600))
	fake := filepath.Join(dir, "lsof")
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\ncat '"+printed+"'\n"), 0o700))
	return []string{"LSOF=" + fake}
}

const lsofHeader = "COMMAND   PID USER   FD   TYPE             DEVICE SIZE/OFF NODE NAME\n"

func lsofLine(name string) string {
	return "real-agen 4242 me     7u  IPv6 0x1234567890      0t0  TCP " + name + " (LISTEN)\n"
}

// The sample agent's launcher listens on every interface and cannot be told
// otherwise, so the script has to look at what it got.
func TestListeningScope(t *testing.T) {
	tests := []struct {
		name string
		lsof string
		want string
	}{
		{"every interface", lsofHeader + lsofLine("*:9094"), "all"},
		{"every IPv4 interface", lsofHeader + lsofLine("0.0.0.0:9094"), "all"},
		{"every IPv6 interface", lsofHeader + lsofLine("[::]:9094"), "all"},
		{"one address on the network", lsofHeader + lsofLine("192.168.1.20:9094"), "all"},
		{"this computer only, IPv4", lsofHeader + lsofLine("127.0.0.1:9094"), "loopback"},
		{"this computer only, IPv6", lsofHeader + lsofLine("[::1]:9094"), "loopback"},
		{"this computer only, by name", lsofHeader + lsofLine("localhost:9094"), "loopback"},
		{"both loopback addresses", lsofHeader + lsofLine("127.0.0.1:9094") + lsofLine("[::1]:9094"), "loopback"},
		{"one loopback and one open to the network", lsofHeader + lsofLine("127.0.0.1:9094") + lsofLine("*:9094"), "all"},
		{"not listening", "", "unknown"},
		{"only the header", lsofHeader, "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, code := shell(t, fakeLsof(t, tc.lsof), `listening_scope 4242 9094`)

			assert.Zero(t, code)
			assert.Equal(t, tc.want, out)
		})
	}

	t.Run("without lsof it cannot tell", func(t *testing.T) {
		out, code := shell(t, []string{"LSOF=/nonexistent/lsof"}, `listening_scope 4242 9094`)

		assert.Zero(t, code)
		assert.Equal(t, "unknown", out)
	})
}

// procCommaLocale is a locale of this machine that writes the decimal sign as a
// comma, or "" when it has none. awk reads and prints numbers the way the locale
// of the machine says, unless a script tells it otherwise.
func procCommaLocale(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "locale", "-a").Output()
	if err != nil {
		return "" // no such command here, so no way to ask
	}
	return regexp.MustCompile(`(?m)^(?:de_DE|fr_FR|es_ES|it_IT|pt_BR|nl_NL)\.(?:UTF-8|utf8)$`).FindString(string(out))
}

// A script that waits as long as a program does has to read a duration as the
// program reads it. The programs read theirs with time.ParseDuration, and the
// server takes a grace period only when it is longer than zero, so each case is
// also put to that function: the seconds are the program's own, with a part of a
// second counted as a whole one. A program reads 1.5s the same on every machine,
// so each case is read a second time in a locale that writes a decimal comma,
// when the machine has one.
func TestDurationSeconds(t *testing.T) {
	locales := [][]string{nil}
	if comma := procCommaLocale(t); comma != "" {
		locales = append(locales, []string{"LC_ALL=" + comma})
	} else {
		t.Log("this machine has no locale with a decimal comma: each duration is read once, as the machine is set")
	}
	tests := []struct {
		duration string
		want     string // "" when it is no duration, or none longer than zero
	}{
		{"10s", "10"},
		{"500ms", "1"},
		{"1500ms", "2"},
		{"1.5s", "2"},
		{"2m", "120"},
		{"1m30s", "90"},
		{"1h", "3600"},
		{"1h0m0s", "3600"},
		{"1h30m", "5400"},
		{"1.5h", "5400"},
		{"1.1h", "3960"},
		{"2h45m30.5s", "9931"},
		{"1m0.5s", "61"},
		{"90s", "90"},
		{".5s", "1"},
		{"1.s", "1"},
		{"+10s", "10"},
		{"100us", "1"},
		{"1µs", "1"},
		{"1μs", "1"},
		{"1ns", "1"},
		{"1000000000ns", "1"},
		{"1000000001ns", "2"},
		{"1.000000001s", "2"},
		{" 10s ", "10"},
		{"", ""},
		{"10", ""},
		{"ten", ""},
		{"10x", ""},
		{"1h30", ""},
		{"1.2.3s", ""},
		{".s", ""},
		{"1m.s", ""},
		{"s10s", ""},
		{"1 s", ""},
		{"5S", ""},
		{"1e3s", ""},
		{`1\163`, ""}, // an escape for s is no s
		{`10s\`, ""},
		{"-5s", ""},
		{"0s", ""},
		{"0", ""},
	}
	for _, tc := range tests {
		t.Run("'"+tc.duration+"'", func(t *testing.T) {
			// What a program makes of it: the loaders trim a value before they read it.
			var program string
			if d, err := time.ParseDuration(strings.TrimSpace(tc.duration)); err == nil && d > 0 {
				program = strconv.FormatInt(int64((d+time.Second-1)/time.Second), 10)
			}
			require.Equal(t, program, tc.want, "the case says what the program reads")

			for _, locale := range locales {
				out, code := shell(t, append([]string{"GIVEN=" + tc.duration}, locale...), `duration_seconds "$GIVEN"`)

				assert.Equal(t, tc.want, out, "%v", locale)
				assert.Equal(t, tc.want == "", code != 0, "it fails for what is no duration, and only for that (%v)", locale)
			}
		})
	}
}

// procStopTree is a copy of the scripts with settings of its own for stop.sh: a
// port nothing listens on, so that the script cannot find the Web UI of this
// machine, and the grace period given ("" for none).
func procStopTree(t *testing.T, grace string) string {
	t.Helper()
	tree := scriptTree(t)
	settings := "BIN_DIR=bin\nLOG_DIR=logs\nAPP_HTTP_ADDR=127.0.0.1:" + strconv.Itoa(freePort(t)) + "\n"
	if grace != "" {
		settings += "APP_SHUTDOWN_GRACE=" + grace + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(tree, ".env.example"), []byte(settings), 0o600))
	return tree
}

// procStop runs the stop.sh of the tree and returns what it printed. The example
// it falls back on is the tree's, whatever a developer's shell says: another one
// could send it to the port of the Web UI that runs on this machine.
func procStop(t *testing.T, tree string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(tree, "scripts", "stop.sh"))
	cmd.Dir, cmd.Env = tree, append(withoutSecrets(os.Environ()), "ENV_EXAMPLE=.env.example", "BASH_ENV=")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return string(out)
}

// stop.sh gives the server as long as the server gives its replies, and two seconds
// more, before it kills it. A stand-in that does not stop when asked shows how long
// that was: half a second of grace is one second of waiting, plus the two.
func TestStop_WaitsAsLongAsTheGracePeriod(t *testing.T) {
	tree := procStopTree(t, "0.5s")
	// It looks like the Web UI to is_process (the name is in its command line), it
	// ignores SIGTERM, and it says when it does: asked to stop before that, it would.
	ignoring := filepath.Join(tree, "ignoring")
	fake := filepath.Join(tree, "agent-web-ui-fake.sh")
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/bash\ntrap '' TERM\n: >'"+ignoring+"'\nwhile true; do sleep 1; done\n"), 0o700))
	standIn := exec.Command(fake)
	require.NoError(t, standIn.Start())
	t.Cleanup(func() { _ = standIn.Process.Kill(); _ = standIn.Wait() })
	require.Eventually(t, func() bool { _, err := os.Stat(ignoring); return err == nil }, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, os.MkdirAll(filepath.Join(tree, "logs"), 0o700))
	pidFile := filepath.Join(tree, "logs", "agent-web-ui.pid")
	require.NoError(t, os.WriteFile(pidFile, []byte(strconv.Itoa(standIn.Process.Pid)+"\n"), 0o600))

	out := procStop(t, tree)

	assert.Contains(t, out, "Stopping agent-web-ui (pid "+strconv.Itoa(standIn.Process.Pid)+")")
	assert.Contains(t, out, "It did not stop within 3s; killing it.")
	assert.Contains(t, out, "Killed.")
	assert.NoFileExists(t, pidFile)
}

// A grace period that the settings lack, or that is no duration, does not keep
// stop.sh from its work: it goes on with the ten seconds the server has by itself.
// (How long it then waits is not timed here: that takes twelve seconds.)
func TestStop_WorksWithoutAGracePeriodItCanRead(t *testing.T) {
	for name, grace := range map[string]string{"none in the settings": "", "one that is no duration": "soon"} {
		t.Run(name, func(t *testing.T) {
			out := procStop(t, procStopTree(t, grace))

			assert.Contains(t, out, "agent-web-ui is not running.")
		})
	}
}

// syscallZero checks that a process exists without signalling it.
var syscallZero = syscall.Signal(0)
