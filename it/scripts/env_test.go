package scripts_test

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// load_env reads the settings the scripts share.
func TestLoadEnv(t *testing.T) {
	t.Run("puts the settings in the environment of what the script starts", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.env"), []byte("GREETING=hello\nNAME='a b'\n"), 0o600))

		out, code := shellIn(t, dir, nil, `load_env settings.env; env | grep -E '^(GREETING|NAME)=' | sort`)

		assert.Zero(t, code, out)
		assert.Equal(t, "GREETING=hello\nNAME=a b", out)
	})
	t.Run("only what is in the file, and nothing assigned afterwards", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.env"), []byte("FROM_FILE=1\n"), 0o600))

		out, code := shellIn(t, dir, nil, `load_env settings.env; LATER=2; env | grep -E '^(FROM_FILE|LATER)=' | sort`)

		assert.Zero(t, code, out)
		assert.Equal(t, "FROM_FILE=1", out, "a variable the script sets itself afterwards is not exported")
	})
	t.Run("a name without a directory is read from here, not from the PATH", func(t *testing.T) {
		here, elsewhere := t.TempDir(), t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(here, "settings.env"), []byte("WHERE=here\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(elsewhere, "settings.env"), []byte("WHERE=elsewhere\n"), 0o600))

		out, code := shellIn(t, here, []string{"PATH=" + elsewhere + string(os.PathListSeparator) + os.Getenv("PATH")}, `load_env settings.env; echo "$WHERE"`)

		assert.Zero(t, code, out)
		assert.Equal(t, "here", out)
	})
	t.Run("a path is read where it says", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "elsewhere", "settings.env")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("WHERE=there\n"), 0o600))

		out, code := shellIn(t, t.TempDir(), nil, `load_env "`+path+`"; echo "$WHERE"`)

		assert.Zero(t, code, out)
		assert.Equal(t, "there", out)
	})
	t.Run("a file that is missing stops the script", func(t *testing.T) {
		out, code := shellIn(t, t.TempDir(), nil, `set -e; load_env nothing.env; echo reached`)

		assert.NotZero(t, code)
		assert.NotContains(t, out, "reached")
	})
}

// setting gives a script one setting: what the settings file says, else what
// .env.example says. The files live in a directory of their own here.
func TestSetting(t *testing.T) {
	tests := []struct {
		name     string
		file     string // the settings file; "-" for none
		example  string
		env      []string
		snippet  string
		want     string
		wantExit int
	}{
		{"the settings file first", "A=from-file\n", "A=from-example\n", nil, "setting A", "from-file", 0},
		{"the example for what the file lacks", "B=1\n", "A=from-example\n", nil, "setting A", "from-example", 0},
		{"the example for what the file leaves empty", "A=\n", "A=from-example\n", nil, "setting A", "from-example", 0},
		{"the example when there is no settings file", "-", "A=from-example\n", nil, "setting A", "from-example", 0},
		{"a commented key is not a value", "-", "# A=nope\n", nil, "setting A", "", 0},
		{"quotes come off and spaces stay", "A='one two'\nB=\"x  y\"\n", "", nil, "setting A; echo; setting B", "one two\nx  y", 0},
		{"nothing when neither says", "B=1\n", "C=2\n", nil, "setting A", "", 0},
		{"the environment does not stand in for a missing setting", "-", "", []string{"A=from-env"}, "setting A", "", 0},
		{"the environment does not beat the file", "A=from-file\n", "A=from-example\n", []string{"A=from-env"}, "setting A", "from-file", 0},
		{"a file that cannot be read is an error, not a quiet fallback", "A=(\n", "A=from-example\n", nil, "setting A", "", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.file != "-" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.env"), []byte(tc.file), 0o600))
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "example.env"), []byte(tc.example), 0o600))

			out, code := shellIn(t, dir, tc.env, "ENV_FILE=settings.env; ENV_EXAMPLE=example.env; "+tc.snippet)

			assert.Equal(t, tc.wantExit, code, out)
			if tc.wantExit == 0 {
				assert.Equal(t, tc.want, out)
			}
		})
	}

	t.Run("the rest of the file stays out of the script's environment", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.env"), []byte("A=1\nADMIN_PASSWORD=canary-secret\n"), 0o600))

		out, code := shellIn(t, dir, nil,
			`ENV_FILE=settings.env; ENV_EXAMPLE=none.env; setting A >/dev/null; env | grep -c canary-secret || true; [ -z "${ADMIN_PASSWORD-}" ] && echo not-even-a-variable`)

		assert.Zero(t, code, out)
		assert.Equal(t, "0\nnot-even-a-variable", out, "a script can read its own settings without handing every secret to what it starts")
	})
}

// A syntax error in a file read with "." ends the shell that reads it before any
// message of its own, and a line that fails (an unquoted space) is ignored when a
// later line succeeds. Either way a script would carry on with the example's value,
// or stop, without a word about the file. The reading is strict whatever the caller
// is: the scripts run with set -euo pipefail, but a helper cannot count on that.
func TestSetting_AFileThatCannotBeReadSaysSo(t *testing.T) {
	files := []struct{ name, content string }{
		{"a syntax error", "A=(\nB=2\n"},
		{"a variable that is not set", "A=$NOT_SET_ANYWHERE_AT_ALL\nB=2\n"},
		{"a value with a space that is not quoted", "A=hello world\nB=2\n"},
		{"a command that fails", "A=1\nfalse\nB=2\n"},
	}
	callers := []struct{ name, options string }{
		{"as the scripts run", "set -euo pipefail"},
		{"called from a shell with no options", ":"},
	}
	for _, f := range files {
		for _, caller := range callers {
			t.Run(f.name+", "+caller.name, func(t *testing.T) {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.env"), []byte(f.content), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "example.env"), []byte("A=from-example\n"), 0o600))

				out, code := shellIn(t, dir, nil, caller.options+"; ENV_FILE=settings.env; ENV_EXAMPLE=example.env; setting A")

				assert.Equal(t, 1, code, out)
				assert.Contains(t, out, "cannot read the settings in settings.env")
				assert.NotContains(t, out, "from-example", "the example does not stand in for a file that is broken")
			})
		}
	}
}

// What the repository ships has to read the way the scripts read it.
func TestSetting_TheExampleReadsUnderTheStrictnessOfTheScripts(t *testing.T) {
	example, err := filepath.Abs("../../.env.example")
	require.NoError(t, err)
	var keys []string
	for key := range documentedSettings(t) {
		keys = append(keys, key)
	}

	out, code := shellIn(t, t.TempDir(), nil,
		"set -euo pipefail; ENV_FILE=none.env; ENV_EXAMPLE='"+example+"'; for key in "+strings.Join(keys, " ")+"; do setting \"$key\" >/dev/null; done; echo read-all")

	assert.Zero(t, code, out)
	assert.Equal(t, "read-all", out)
}

// freePort is a port nothing listens on.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// scriptTree is a copy of the scripts in a directory of their own. A script works in
// the tree it sits in (bin/, logs/ and the pid files are there), so a test that can
// make one fall back to the defaults, the way a wrong --env does, has to run the
// copy: the original would stop the Web UI that is running on this machine.
func scriptTree(t *testing.T) string {
	t.Helper()
	tree := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tree, "scripts", "lib"), 0o700))
	for _, name := range []string{"lib/process.sh", "run.sh", "stop.sh", "eval.sh", "check.sh", "real-agent.sh", "pr-check.sh"} {
		src, err := os.ReadFile(filepath.Join("..", "..", "scripts", name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(tree, "scripts", name), src, 0o700))
	}
	return tree
}

// A script is helped by a name, not by its settings: --help has to answer when the
// settings file is broken, since that is when somebody asks.
func TestScripts_HelpNeedsNoSettings(t *testing.T) {
	tree := scriptTree(t)
	require.NoError(t, os.WriteFile(filepath.Join(tree, ".env"), []byte("A=(\n"), 0o600)) // and no example at all

	for _, name := range []string{"run.sh", "stop.sh", "eval.sh", "check.sh", "real-agent.sh", "pr-check.sh"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(tree, "scripts", name), "--help")
			cmd.Dir, cmd.Env = tree, withoutSecrets(os.Environ())
			out, err := cmd.CombinedOutput()

			require.NoError(t, err, string(out))
			assert.Contains(t, string(out), "Usage: scripts/"+name)
		})
	}
}

// The file --env names is the one the script reads its settings from. stop.sh finds
// the Web UI by the pid file in LOG_DIR, so a bystander that looks like it and a pid
// file in a folder only the named file knows prove it: reading the example instead
// would look in logs/ and find nothing.
func TestStop_ReadsTheSettingsFileItIsTold(t *testing.T) {
	tree := scriptTree(t)
	require.NoError(t, os.WriteFile(filepath.Join(tree, ".env.example"),
		[]byte("BIN_DIR=bin\nLOG_DIR=logs\nAPP_HTTP_ADDR=127.0.0.1:"+strconv.Itoa(freePort(t))+"\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(tree, "elsewhere"), 0o700))
	// Looks like the Web UI to is_process: its command line has the name in it.
	fake := filepath.Join(tree, "agent-web-ui-fake.sh")
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/bash\nwhile true; do sleep 1; done\n"), 0o700))
	bystander := exec.Command(fake)
	require.NoError(t, bystander.Start())
	t.Cleanup(func() { _ = bystander.Process.Kill(); _ = bystander.Wait() })
	require.NoError(t, os.WriteFile(filepath.Join(tree, "elsewhere", "agent-web-ui.pid"), []byte(strconv.Itoa(bystander.Process.Pid)+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "custom.env"), []byte("LOG_DIR=elsewhere\nAPP_SHUTDOWN_GRACE=1s\n"), 0o600))

	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(tree, "scripts", "stop.sh"), "--env", "custom.env")
	cmd.Dir, cmd.Env = tree, withoutSecrets(os.Environ())
	out, err := cmd.CombinedOutput()

	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "Stopping agent-web-ui (pid "+strconv.Itoa(bystander.Process.Pid)+")", "it found the pid file the named file points to")
	assert.NoFileExists(t, filepath.Join(tree, "elsewhere", "agent-web-ui.pid"))
}

// required_setting says which setting is missing, and the script has to stop right
// there. Written as $(f "$(required_setting X)"), the failure of the inner call never
// reaches set -e, and the script carries on with an empty value.
func TestRealAgent_StopsAtASettingTheExampleLost(t *testing.T) {
	example, err := os.ReadFile("../../.env.example")
	require.NoError(t, err)
	lost := regexp.MustCompile(`(?m)^BIN_DIR=.*\n`).ReplaceAll(example, nil)
	require.Less(t, len(lost), len(example), "the line was there")
	// Ports nothing listens on, as in every tree that runs a script that could reach the real services.
	for _, name := range []string{"REAL_AGENT_PORT=", "APP_HTTP_ADDR=127.0.0.1:"} {
		lost = regexp.MustCompile(`(?m)^`+name+`.*$`).ReplaceAll(lost, []byte(name+strconv.Itoa(freePort(t))))
	}
	tree := scriptTree(t)
	require.NoError(t, os.WriteFile(filepath.Join(tree, ".env.example"), lost, 0o600))

	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(tree, "scripts", "real-agent.sh"), "status")
	cmd.Dir, cmd.Env = tree, withoutSecrets(os.Environ())
	out, err := cmd.CombinedOutput()

	exit, ok := err.(*exec.ExitError)
	require.True(t, ok, "it stops: %s", out)
	assert.Equal(t, 1, exit.ExitCode(), string(out))
	assert.Contains(t, string(out), "BIN_DIR is not set")
	assert.NotContains(t, string(out), "not running", "and does not go on to answer")
}

// --env names the settings file to read. A name with no file behind it is a mistake
// in the call, not a reason to fall back to the example: stop.sh --env typo.env
// would say "not running" about a server that was started from another file.
func TestScripts_ANamedSettingsFileHasToExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nothing.env")
	tests := []struct {
		script string
		args   []string
	}{
		{"run.sh", nil},
		{"stop.sh", nil},
		{"eval.sh", []string{"selftest"}},
		{"real-agent.sh", []string{"status"}},
		{"pr-check.sh", nil},
	}
	for _, tc := range tests {
		t.Run(tc.script, func(t *testing.T) {
			// A copy, with an example of its own: if a script does fall back to the example,
			// it reaches this tree's logs and pid files, and the port it would look for the
			// Web UI on is one nothing listens on. (stop.sh stops whatever program of its
			// name listens on that port, and the real one listens on 8300.)
			tree := scriptTree(t)
			example := "BIN_DIR=bin\nLOG_DIR=logs\nAPP_HTTP_ADDR=127.0.0.1:" + strconv.Itoa(freePort(t)) +
				"\nREAL_AGENT_PORT=" + strconv.Itoa(freePort(t)) + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(tree, ".env.example"), []byte(example), 0o600))
			cmd := exec.CommandContext(t.Context(), "bash", append([]string{filepath.Join(tree, "scripts", tc.script), "--env", missing}, tc.args...)...)
			cmd.Dir, cmd.Env = tree, withoutSecrets(os.Environ())
			out, err := cmd.CombinedOutput()

			exit, ok := err.(*exec.ExitError)
			require.True(t, ok, "it stops: %s", out)
			assert.Equal(t, 2, exit.ExitCode(), string(out))
			assert.Contains(t, string(out), "no settings file")
		})
	}
}

// The file init-env.sh writes holds passwords. It is readable by its owner only
// from the moment it exists; chmod afterwards leaves a gap in which anyone on the
// machine could read it, so the test takes chmod away and leaves the umask alone.
func TestInitEnv_TheFileIsPrivateFromTheStart(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "scripts"), 0o700))
	for _, name := range []string{"scripts/init-env.sh", ".env.example"} {
		data, err := os.ReadFile(filepath.Join("..", "..", name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(repo, name), data, 0o700))
	}
	shim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(shim, "chmod"), []byte("#!/bin/sh\nexit 0\n"), 0o700))

	cmd := exec.CommandContext(t.Context(), "bash", "-c", "umask 000; exec bash scripts/init-env.sh")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	info, err := os.Stat(filepath.Join(repo, ".env"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.NotContains(t, string(out), "ADMIN_PASSWORD=", "and the passwords are not printed")
}

// The password of the database is written into DB_URL as it is, by the script
// and by Docker Compose, and the server refuses a URL whose password holds an
// @, a slash, a question mark or a hash sign as it is. So the passwords the
// script generates are letters and digits, whatever base64 makes of the random
// bytes: the stand-in for it here hands on the signs a URL gives a meaning to.
func TestInitEnv_ThePasswordsAreLettersAndDigits(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "scripts"), 0o700))
	for _, name := range []string{"scripts/init-env.sh", ".env.example"} {
		data, err := os.ReadFile(filepath.Join("..", "..", name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(repo, name), data, 0o700))
	}
	shim := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(shim, "base64"),
		[]byte("#!/bin/sh\ncat >/dev/null\necho 'a@b/c?d#e:f%g+h=i 0123456789jklmnopqrstuvwxyz'\n"), 0o700))

	cmd := exec.CommandContext(t.Context(), "bash", "scripts/init-env.sh")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	written, err := os.ReadFile(filepath.Join(repo, ".env"))
	require.NoError(t, err)
	lettersAndDigits := regexp.MustCompile(`^[A-Za-z0-9]{20,}$`)
	for _, password := range []string{`^DB_URL=postgres://agentwebui:(.*)@localhost:`, `^POSTGRES_PASSWORD=(.*)$`, `^ADMIN_PASSWORD=(.*)$`} {
		m := regexp.MustCompile(`(?m)` + password).FindSubmatch(written)
		require.NotNil(t, m, "the file has a line like %s", password)
		// As a truth value: a password is not for the output of a test.
		assert.True(t, lettersAndDigits.Match(m[1]), "the password in %s holds something else than letters and digits", password)
	}
}
