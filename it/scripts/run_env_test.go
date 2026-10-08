package scripts_test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWebUI is a stand-in for agent-web-ui. It listens where its environment
// says, answers the route run.sh waits for, and tells the arguments it was
// started with. It ends by itself after a minute, so that a test that failed to
// stop it leaves nothing running.
const fakeWebUI = `package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /args", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, strings.Join(os.Args[1:], " ")) })
	time.AfterFunc(time.Minute, func() { os.Exit(0) })
	if err := http.ListenAndServe(os.Getenv("APP_HTTP_ADDR"), mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`

// buildFakeWebUI builds the stand-in where run.sh looks for the server, so the
// script finds a binary and builds none.
func buildFakeWebUI(t *testing.T, tree string) {
	t.Helper()
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "go.mod"), []byte("module fake\n\ngo 1.27\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"), []byte(fakeWebUI), 0o600))
	build := exec.CommandContext(t.Context(), goCommand(t), "build", "-o", filepath.Join(tree, "bin", "agent-web-ui"), ".")
	build.Dir = src
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
}

// stopFakeWebUI ends the stand-in that run.sh started in the tree, if it did.
func stopFakeWebUI(tree string) {
	raw, err := os.ReadFile(filepath.Join(tree, "logs", "agent-web-ui.pid"))
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
		if started, err := os.FindProcess(pid); err == nil {
			_ = started.Kill()
		}
	}
}

// The server reads its settings itself, from .env beside it unless it is told
// another file. So run.sh passes the file it was given on: a key that the
// named file lacks or leaves empty then stays unset, and is not taken from .env.
func TestRun_StartsTheServerWithTheSettingsFileItIsTold(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed, so the script cannot tell when the server is ready")
	}
	tests := []struct {
		name string
		file string   // the settings file in the tree
		args []string // of run.sh
		want string   // the arguments of the server
	}{
		{"a named file is passed on", "custom.env", []string{"--env", "custom.env"}, "--env custom.env"},
		{"with .env nothing is, since the server reads that one unasked", ".env", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tree := scriptTree(t)
			address := "127.0.0.1:" + strconv.Itoa(freePort(t))
			require.NoError(t, os.WriteFile(filepath.Join(tree, ".env.example"), []byte("BIN_DIR=bin\nLOG_DIR=logs\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(tree, tc.file), []byte("APP_HTTP_ADDR="+address+"\n"), 0o600))
			buildFakeWebUI(t, tree)
			t.Cleanup(func() { stopFakeWebUI(tree) })

			cmd := exec.CommandContext(t.Context(), "bash", append([]string{filepath.Join(tree, "scripts", "run.sh")}, tc.args...)...)
			cmd.Dir, cmd.Env = tree, withoutSecrets(os.Environ())
			out, err := cmd.CombinedOutput()

			require.NoError(t, err, string(out))
			assert.Contains(t, string(out), "Ready: ")
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/args", nil)
			require.NoError(t, err)
			res, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = res.Body.Close() }()
			args, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(args))
		})
	}
}
