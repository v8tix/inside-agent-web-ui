package scripts_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
)

// fakeSample is a stand-in for the ADK sample agent: it serves the one route the
// script waits for, and listens on every interface unless FAKE_BIND says
// otherwise, as the ADK launcher does.
const fakeSample = `package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
)

func main() {
	port := ""
	for _, a := range os.Args {
		if v, ok := strings.CutPrefix(a, "--port="); ok {
			port = v
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/list-apps", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, ` + "`" + `["ui_agent"]` + "`" + `) })
	mux.HandleFunc("GET /api/args", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, strings.Join(os.Args[1:], " ")) })
	// The fake listens on every interface, as the ADK before 2.5 does. It must not hand
	// out the rest of its environment to whoever finds the port: only the settings the
	// tests look at, and only to this computer.
	served := map[string]bool{"MODEL_TYPE": true, "GEMINI_MODEL": true, "GOOGLE_AI_STUDIO_API_KEY": true, "ADMIN_PASSWORD": true, "HF_TOKEN": true, "DB_URL": true, "OLLAMA_MODEL": true, "OLLAMA_BASE_URL": true}
	mux.HandleFunc("GET /api/env/{name}", func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() || !served[r.PathValue("name")] {
			http.Error(w, "not given out", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, os.Getenv(r.PathValue("name")))
	})
	ln, err := net.Listen("tcp", net.JoinHostPort(os.Getenv("FAKE_BIND"), port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = http.Serve(ln, mux)
}
`

// goCommand is the go command to build the fake sample with: the one .env says
// (GO), or the one on the path.
func goCommand(t *testing.T) string {
	t.Helper()
	if lookup, err := envcfg.EnvLookup("../../.env"); err == nil {
		if path, ok := lookup("GO"); ok && path != "" {
			return path
		}
	}
	path, err := exec.LookPath("go")
	if err != nil {
		t.Skip("there is no go command to build the fake agent with")
	}
	return path
}

// rig is the script, a fake sample to run, a settings file of its own (the
// script reads its settings from a file, never from the environment) and a place
// for its files.
type rig struct {
	t        *testing.T
	script   string
	sample   string // the fake sample's directory
	settings string // the settings file the script is told to read
	env      []string
	port     int
	logs     string
}

// newRig makes a rig whose settings file holds the lines given after the basics;
// a later line for the same name wins.
func newRig(t *testing.T, settings ...string) *rig {
	t.Helper()
	lsofIsInstalled(t) // the script asks it where the agent listens
	sample := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(sample, "cmd", "ui-agent"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sample, "go.mod"), []byte("module fake\n\ngo 1.27\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(sample, "cmd", "ui-agent", "main.go"), []byte(fakeSample), 0o600))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	script, err := filepath.Abs("../../scripts/real-agent.sh")
	require.NoError(t, err)
	r := &rig{t: t, script: script, sample: sample, port: port, logs: t.TempDir(), settings: filepath.Join(t.TempDir(), "settings.env")}
	lines := append([]string{
		"ADK_TRAINING_DIR='" + sample + "'",
		"REAL_AGENT_PORT=" + strconv.Itoa(port),
		"BIN_DIR='" + t.TempDir() + "'",
		"LOG_DIR='" + r.logs + "'",
		"GO='" + goCommand(t) + "'",
	}, settings...)
	require.NoError(t, os.WriteFile(r.settings, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	t.Cleanup(func() { r.run("stop") })
	return r
}

// withoutSecrets is the environment without the variables a developer's shell may
// export: the ones the script and the agent care about, and anything whose name says
// it is a key, a token, a secret or a password. They must not reach a test: the
// result would depend on the machine, and a failed comparison could print one.
// (A test that needs one adds it to rig.env.)
func withoutSecrets(env []string) []string {
	var kept []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !isSecretName(name) {
			kept = append(kept, kv)
		}
	}
	return kept
}

func isSecretName(name string) bool {
	switch name {
	case "HF_TOKEN", "ADMIN_PASSWORD", "DB_URL", "GOOGLE_AI_STUDIO_API_KEY", "GEMINI_MODEL":
		return true
	}
	upper := strings.ToUpper(name)
	for _, word := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "API_KEY", "PRIVATE_KEY"} {
		if strings.Contains(upper, word) {
			return true
		}
	}
	return strings.HasSuffix(upper, "_KEY")
}

// newLoopbackRig is a rig whose fake sample listens on this computer only, as the
// ADK launcher does from 2.5.
func newLoopbackRig(t *testing.T, settings ...string) *rig {
	t.Helper()
	r := newRig(t, settings...)
	r.env = append(r.env, "FAKE_BIND=127.0.0.1")
	return r
}

// exampleSetting is what .env.example says a setting is.
func exampleSetting(t *testing.T, name string) string {
	t.Helper()
	example, err := os.ReadFile("../../.env.example")
	require.NoError(t, err)
	m := regexp.MustCompile(`(?m)^` + name + `=(.*)$`).FindStringSubmatch(string(example))
	require.NotNil(t, m, "%s is in .env.example", name)
	return strings.Trim(m[1], `"'`)
}

// run runs the script with the rig's settings file.
func (r *rig) run(args ...string) (stdout, stderr string, code int) {
	r.t.Helper()
	return r.runBare(append([]string{"--env", r.settings}, args...)...)
}

// runBare runs the script with exactly the arguments given. Any command that gets
// no settings of its own would use the repository's (the real logs, the real
// agent), so a test that wants that must say so and the rig's cleanup never does.
func (r *rig) runBare(args ...string) (stdout, stderr string, code int) {
	r.t.Helper()
	// Not the test's own context: the cleanup that stops the agent runs after it ends.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", append([]string{r.script}, args...)...)
	cmd.Env = append(withoutSecrets(os.Environ()), r.env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return out.String(), errOut.String(), exit.ExitCode()
	}
	require.NoError(r.t, err)
	return out.String(), errOut.String(), 0
}

func (r *rig) listening() bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(r.port)), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (r *rig) pidFile() string { return filepath.Join(r.logs, "real-agent.pid") }

func lsofIsInstalled(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof is not installed, so the script cannot tell where the agent listens")
	}
}

// The fake agent listens on every interface, as the ADK launcher before 2.5 does, so
// anyone on the network could ask it for what is in its environment. It gives out
// only the few settings the tests look at.
func TestRealAgent_TheFakeSampleGivesOutOnlyWhatTheTestsAskFor(t *testing.T) {
	r := newRig(t, "REAL_AGENT_ALLOW_LAN=1")
	r.env = append(r.env, "CANARY_NOT_ASKED_FOR=canary-value")

	_, stderr, code := r.run("start")

	require.Zero(t, code, stderr)
	assert.False(t, strings.Contains(r.get("/api/env/CANARY_NOT_ASKED_FOR"), "canary"), "a variable of its environment nobody asked about")
	assert.False(t, strings.Contains(r.get("/api/env/PATH"), "/"), "not even the path")
	assert.Equal(t, "gemini", r.get("/api/env/MODEL_TYPE"), "while the ones the tests look at are given")
}

// Secrets a developer's shell exports must not reach the scripts under test, whatever
// they are called: the result would depend on the machine, and a failed comparison
// could print one.
func TestWithoutSecrets(t *testing.T) {
	tests := []struct {
		variable string
		kept     bool
	}{
		{"PATH=/usr/bin", true},
		{"HOME=/home/someone", true},
		{"GOCACHE=/tmp/gocache", true},
		{"TERM=xterm", true},
		{"KEYBOARD_LAYOUT=us", true},
		{"HF_TOKEN=x", false},
		{"ADMIN_PASSWORD=x", false},
		{"DB_URL=x", false},
		{"GOOGLE_AI_STUDIO_API_KEY=x", false},
		{"GEMINI_MODEL=x", false},
		{"OPENAI_API_KEY=x", false},
		{"ANTHROPIC_API_KEY=x", false},
		{"GITHUB_TOKEN=x", false},
		{"AWS_SECRET_ACCESS_KEY=x", false},
		{"SOME_SERVICE_PASSWORD=x", false},
		{"SIGNING_PRIVATE_KEY=x", false},
		{"DEPLOY_KEY=x", false},
	}
	for _, tc := range tests {
		t.Run(strings.SplitN(tc.variable, "=", 2)[0], func(t *testing.T) {
			got := withoutSecrets([]string{"A=1", tc.variable, "B=2"})

			assert.Equal(t, tc.kept, slices.Contains(got, tc.variable))
			assert.Equal(t, []string{"A=1", "B=2"}, slices.DeleteFunc(slices.Clone(got), func(kv string) bool { return kv == tc.variable }), "and nothing else goes")
		})
	}
}

// The ADK launcher binds every interface and cannot be told not to.
func TestRealAgent_RefusesAnAgentThatListensOnTheNetwork(t *testing.T) {
	r := newRig(t)

	_, stderr, code := r.run("start")

	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "every network interface")
	assert.Contains(t, stderr, "REAL_AGENT_ALLOW_LAN=1", "it says how to go on")
	assert.Contains(t, stderr, "REAL_AGENT_ADK=v2.5.0", "and the way that needs no trust in the network")
	assert.False(t, r.listening(), "the agent was stopped, not left running")
	assert.NoFileExists(t, r.pidFile())
}

// lsof is how the script learns where the agent listens. Without an answer it cannot
// tell, and stops the agent all the same (the safe side), but it must say that, not
// that the agent listens on every interface.
func TestRealAgent_SaysSoWhenItCannotTellWhereTheAgentListens(t *testing.T) {
	blind := filepath.Join(t.TempDir(), "lsof")
	require.NoError(t, os.WriteFile(blind, []byte("#!/bin/sh\nexit 1\n"), 0o700))
	r := newLoopbackRig(t)
	r.env = append(r.env, "LSOF="+blind)

	_, stderr, code := r.run("start")

	assert.Equal(t, 1, code)
	assert.Contains(t, stderr, "Cannot tell where the agent listens")
	assert.Contains(t, stderr, "REAL_AGENT_ALLOW_LAN=1", "it says how to go on")
	assert.NotContains(t, stderr, "every network interface", "it does not claim what it does not know")
	assert.False(t, r.listening(), "the agent was stopped")
}

func TestRealAgent_KeepsItWhenToldTheNetworkIsFine(t *testing.T) {
	r := newRig(t, "REAL_AGENT_ALLOW_LAN=1")

	stdout, stderr, code := r.run("start")

	assert.Zero(t, code, stderr)
	assert.Contains(t, stderr, "Warning", "and still says it")
	assert.Contains(t, stdout, "The agent is ready")
	assert.True(t, r.listening())

	stdout, _, code = r.run("stop")
	assert.Zero(t, code)
	assert.Contains(t, stdout, "stopped")
	assert.False(t, r.listening())
}

func TestRealAgent_KeepsAnAgentThatListensOnThisComputerOnly(t *testing.T) {
	r := newLoopbackRig(t)

	stdout, stderr, code := r.run("start")

	assert.Zero(t, code, stderr)
	assert.NotContains(t, stderr, "Warning")
	assert.Contains(t, stdout, "The agent is ready")
	assert.True(t, r.listening())
}

// get asks the running fake agent for a path.
func (r *rig) get(path string) string {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.t.Context(), http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(r.port)+path, nil)
	require.NoError(r.t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(r.t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(r.t, err)
	return string(body)
}

// The launcher ends a reply that has streamed for two minutes, which a large
// local model can need to write a long answer. The page shows such a reply as cut
// off, so the script raises the limit, and the flag has to follow the launcher's
// own name (api), as the launcher reads it.
func TestRealAgent_RaisesTheLimitOnHowLongOneReplyMayStream(t *testing.T) {
	tests := []struct {
		name     string
		settings []string // lines of the settings file
		want     string
	}{
		{"what .env.example says unless told otherwise", nil, "--sse-write-timeout=" + exampleSetting(t, "REAL_AGENT_SSE_TIMEOUT")},
		{"as long as asked", []string{"REAL_AGENT_SSE_TIMEOUT=45m"}, "--sse-write-timeout=45m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newLoopbackRig(t, tc.settings...)

			_, stderr, code := r.run("start")

			require.Zero(t, code, stderr)
			args := r.get("/api/args")
			assert.Contains(t, args, "--port="+strconv.Itoa(r.port)+" api "+tc.want, "after the name of the launcher it belongs to")
		})
	}
}

// A pid file outlives its process, and the number can be given to another one.
func TestRealAgent_StopLeavesAnUnrelatedProcessAlone(t *testing.T) {
	r := newRig(t)
	bystander := exec.CommandContext(t.Context(), "sleep", "60")
	require.NoError(t, bystander.Start())
	t.Cleanup(func() { _ = bystander.Process.Kill(); _ = bystander.Wait() })
	require.NoError(t, os.WriteFile(r.pidFile(), []byte(strconv.Itoa(bystander.Process.Pid)+"\n"), 0o600))

	stdout, _, code := r.run("stop")

	assert.Zero(t, code)
	assert.Contains(t, stdout, "not running")
	assert.NoError(t, bystander.Process.Signal(syscallZero), "the other process was not signalled")
	status, _, _ := r.run("status")
	assert.Contains(t, status, "not running")
	assert.True(t, strings.TrimSpace(status) != "")
}

// stubGo is a go command that writes down what it is asked and lets the real one
// do the builds. get and mod would need the network and an ADK requirement,
// which the fake sample does not have, so they only succeed.
func stubGo(t *testing.T) (command, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	command = filepath.Join(dir, "go")
	script := "#!/usr/bin/env bash\n" +
		"echo \"$PWD :: $*\" >> \"" + calls + "\"\n" +
		"case \"$1\" in get | mod) exit 0 ;; esac\n" +
		"exec \"" + goCommand(t) + "\" \"$@\"\n"
	require.NoError(t, os.WriteFile(command, []byte(script), 0o700))
	return command, calls
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// The ADK launcher before 2.5 listens on every interface. REAL_AGENT_ADK builds the
// sample against a newer ADK, from a copy of go.mod, without changing the sample's
// own files. "sample" asks for the version its go.mod pins.
func TestRealAgent_BuildsAgainstTheADKItIsToldTo(t *testing.T) {
	t.Run("the sample's own pin when told so", func(t *testing.T) {
		goStub, calls := stubGo(t)
		r := newLoopbackRig(t, "GO='"+goStub+"'", "REAL_AGENT_ADK=sample")

		stdout, stderr, code := r.run("start")

		require.Zero(t, code, stderr)
		assert.NotContains(t, stdout, "against ADK")
		log := readFile(t, calls)
		assert.Contains(t, log, "build -trimpath")
		assert.NotContains(t, log, "-modfile")
		assert.NotContains(t, log, ":: get ")
	})
	t.Run("another ADK from a copy of go.mod, leaving the sample alone", func(t *testing.T) {
		goStub, calls := stubGo(t)
		r := newLoopbackRig(t, "GO='"+goStub+"'", "REAL_AGENT_ADK=v2.5.0")
		sampleMod := filepath.Join(r.sample, "go.mod")
		before := readFile(t, sampleMod)

		stdout, stderr, code := r.run("start")

		require.Zero(t, code, stderr)
		assert.Contains(t, stdout, "against ADK v2.5.0")
		assert.Contains(t, stdout, "The agent is ready")
		log := readFile(t, calls)
		for _, step := range []string{":: get -modfile=", ":: mod tidy -modfile=", ":: build -modfile="} {
			assert.Contains(t, log, step)
		}
		assert.Contains(t, log, "google.golang.org/adk/v2@v2.5.0")
		modfiles := regexp.MustCompile(`-modfile=(\S+)`).FindAllStringSubmatch(log, -1)
		require.Len(t, modfiles, 3, "get, tidy and build")
		assert.Equal(t, modfiles[0][1], modfiles[1][1], "one copy for all three")
		assert.Equal(t, modfiles[0][1], modfiles[2][1], "one copy for all three")
		assert.NotContains(t, modfiles[0][1], r.sample, "the copy is not in the sample")
		assert.NoFileExists(t, modfiles[0][1], "and is gone once the build is done")
		assert.Equal(t, before, readFile(t, sampleMod), "the sample's go.mod is as it was")
	})
	t.Run("what .env.example says when the settings file does not", func(t *testing.T) {
		goStub, calls := stubGo(t)
		r := newLoopbackRig(t, "GO='"+goStub+"'")

		_, stderr, code := r.run("start")

		require.Zero(t, code, stderr)
		assert.Contains(t, readFile(t, calls), "google.golang.org/adk/v2@"+exampleSetting(t, "REAL_AGENT_ADK"),
			"the safe build is the default, so nobody has to ask for it")
	})
}

// The real agent runs on Gemini and on nothing else. The sample can also run a
// model on this computer, which takes half of its memory, so the script names the
// model itself (MODEL_TYPE) and takes no setting for it: nothing a shell or an
// old settings file says starts another model.
func TestRealAgent_RunsOnGeminiOnly(t *testing.T) {
	t.Run("whatever the shell says", func(t *testing.T) {
		r := newLoopbackRig(t)
		r.env = append(r.env, "MODEL_TYPE=ollama", "REAL_AGENT_MODEL=ollama")

		_, stderr, code := r.run("start")

		require.Zero(t, code, stderr)
		assert.Equal(t, "gemini", r.get("/api/env/MODEL_TYPE"))
	})
	t.Run("whatever a settings file from before says", func(t *testing.T) {
		// Port 9 is the discard service: nothing answers there, and nothing is asked.
		r := newLoopbackRig(t, "REAL_AGENT_MODEL=ollama", "OLLAMA_BASE_URL=http://127.0.0.1:9/v1", "OLLAMA_MODEL=a-local-model")

		stdout, stderr, code := r.run("start")

		require.Zero(t, code, stderr)
		assert.Equal(t, "gemini", r.get("/api/env/MODEL_TYPE"))
		assert.Empty(t, r.get("/api/env/OLLAMA_MODEL"), "a local model is not handed to the agent")
		assert.Empty(t, r.get("/api/env/OLLAMA_BASE_URL"))
		assert.Contains(t, stdout, "Gemini", "the script says which model the agent runs on")
	})
	t.Run("the repository offers no other", func(t *testing.T) {
		for _, name := range []string{"scripts/real-agent.sh", ".env.example", "README.md", "docs/spec.md"} {
			text, err := os.ReadFile(filepath.Join("../..", name))
			require.NoError(t, err)
			for n, line := range strings.Split(strings.ToLower(string(text)), "\n") {
				// The line number, not the line: a settings file is no text for a test's output.
				assert.False(t, strings.Contains(line, "ollama"), "%s:%d names another model", name, n+1)
				assert.False(t, strings.Contains(line, "real_agent_model"), "%s:%d names a setting that chooses a model", name, n+1)
			}
		}
	})
}

// The settings are in the settings file. The environment is not looked at, so a
// variable left in a shell cannot change them, and the rest of the file
// (passwords, tokens) does not reach the agent, which is somebody else's program.
func TestRealAgent_TakesItsSettingsFromTheSettingsFile(t *testing.T) {
	t.Run("not from the environment", func(t *testing.T) {
		r := newLoopbackRig(t, "REAL_AGENT_SSE_TIMEOUT=45m")
		r.env = append(r.env, "REAL_AGENT_SSE_TIMEOUT=1m")

		_, stderr, code := r.run("start")

		require.Zero(t, code, stderr)
		assert.Contains(t, r.get("/api/args"), "--sse-write-timeout=45m")
	})
	t.Run("the model the file names reaches the agent", func(t *testing.T) {
		r := newLoopbackRig(t, "GEMINI_MODEL=gemini-test-model")

		_, stderr, code := r.run("start")

		require.Zero(t, code, stderr)
		assert.Equal(t, "gemini", r.get("/api/env/MODEL_TYPE"))
		assert.Equal(t, "gemini-test-model", r.get("/api/env/GEMINI_MODEL"))
	})
	t.Run("no Gemini model named uses this repository's shipped default", func(t *testing.T) {
		r := newLoopbackRig(t)

		_, stderr, code := r.run("start")

		require.Zero(t, code, stderr)
		assert.Equal(t, exampleSetting(t, "GEMINI_MODEL"), r.get("/api/env/GEMINI_MODEL"))
	})
	t.Run("the Gemini key reaches the agent, and the rest of the file does not", func(t *testing.T) {
		r := newLoopbackRig(t, "GOOGLE_AI_STUDIO_API_KEY=canary-gemini-key",
			"ADMIN_PASSWORD=canary-password", "HF_TOKEN=canary-token", "DB_URL=postgres://u:canary-db@h/d")

		_, stderr, code := r.run("start")

		require.Zero(t, code, stderr)
		// Compared as true or false, so that a failure cannot print a value.
		assert.True(t, r.get("/api/env/GOOGLE_AI_STUDIO_API_KEY") == "canary-gemini-key", "the key the file holds reaches the agent")
		assert.False(t, strings.Contains(r.get("/api/args"), "canary-gemini-key"), "in its environment, never in its arguments, which ps shows to everyone")
		for _, name := range []string{"ADMIN_PASSWORD", "HF_TOKEN", "DB_URL"} {
			assert.False(t, strings.Contains(r.get("/api/env/"+name), "canary"), "%s stays out of the agent's environment", name)
		}
	})
	t.Run("a key left out of the file is not made up", func(t *testing.T) {
		r := newLoopbackRig(t)

		_, stderr, code := r.run("start")

		require.Zero(t, code, stderr)
		assert.True(t, r.get("/api/env/GOOGLE_AI_STUDIO_API_KEY") == "", "the agent finds its own key, if it has one")
	})
	t.Run("a key the environment already gives the agent is left alone, but the file's default model wins", func(t *testing.T) {
		r := newLoopbackRig(t)
		r.env = append(r.env, "GOOGLE_AI_STUDIO_API_KEY=canary-from-the-shell", "GEMINI_MODEL=canary-model-from-the-shell")

		_, stderr, code := r.run("start")

		require.Zero(t, code, stderr)
		assert.True(t, r.get("/api/env/GOOGLE_AI_STUDIO_API_KEY") == "canary-from-the-shell", "an empty setting does not blank the key")
		assert.Equal(t, exampleSetting(t, "GEMINI_MODEL"), r.get("/api/env/GEMINI_MODEL"),
			"GEMINI_MODEL has a real default now, so it is no longer empty, and a named setting always wins over whatever the agent's own environment already had")
	})
	t.Run("the file's key beats the one in the environment", func(t *testing.T) {
		r := newLoopbackRig(t, "GOOGLE_AI_STUDIO_API_KEY=canary-from-the-file")
		r.env = append(r.env, "GOOGLE_AI_STUDIO_API_KEY=canary-from-the-shell")

		_, stderr, code := r.run("start")

		require.Zero(t, code, stderr)
		assert.True(t, r.get("/api/env/GOOGLE_AI_STUDIO_API_KEY") == "canary-from-the-file")
	})
	t.Run("a settings file that is named and does not exist is an error, not a quiet fallback", func(t *testing.T) {
		r := newLoopbackRig(t) // its own file stays, for the cleanup

		_, stderr, code := r.runBare("--env", filepath.Join(t.TempDir(), "nothing.env"), "status")

		assert.Equal(t, 2, code)
		assert.Contains(t, stderr, "no settings file")
	})
}
