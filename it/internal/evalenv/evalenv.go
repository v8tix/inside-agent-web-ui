// Package evalenv is what the evaluator's integration tests need: the Web UI
// started for real inside the test process (see package stack), with a scripted
// agent behind it and a real PostgreSQL, and the way the evaluator starts Chrome.
//
// It needs Docker (for PostgreSQL, see pgenv) and, for tests that open a
// browser, Node and Chrome.
package evalenv

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
	"github.com/v8tix/inside-agent-web-ui/it/internal/stack"
)

// The first user of every environment.
const (
	Email    = "eval@example.com"
	Password = "an-evaluator-password-1"
)

// Env is a running Web UI.
type Env struct {
	// URL is where a browser reaches it, such as http://127.0.0.1:41233.
	URL             string
	Email, Password string
	// Agent is the scripted agent behind the Web UI.
	Agent *stack.Agent
	// Model is the model the agent runs, for assertions on what it saw.
	Model *testagent.Model
	// Logs is what the Web UI logged: one line per request, and its warnings and
	// errors. Print the lines that matter when a test fails.
	Logs *Logs
}

// Logs is a log that a server writes to while a test reads it.
type Logs struct {
	mu sync.Mutex
	b  strings.Builder
}

// Write implements io.Writer.
func (l *Logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// String returns everything logged so far.
func (l *Logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Lines returns the logged lines that contain every one of the words.
func (l *Logs) Lines(words ...string) []string {
	var out []string
	for line := range strings.SplitSeq(l.String(), "\n") {
		if line != "" && !slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(line, w) }) {
			out = append(out, line)
		}
	}
	return out
}

// Start runs the Web UI and its agent until the test ends. modelOptions set the
// pace and script of the scripted agent.
func Start(t testing.TB, modelOptions ...testagent.Option) *Env {
	t.Helper()
	logs := &Logs{}
	s := stack.Start(t,
		stack.WithModel(modelOptions...),
		stack.WithLogs(logs), stack.WithSetting("APP_LOG_LEVEL", "info"), stack.WithSetting("APP_LOG_FORMAT", "text"),
		stack.WithSetting("ADMIN_EMAIL", Email), stack.WithSetting("ADMIN_PASSWORD", Password),
	)
	return &Env{URL: s.URL, Email: Email, Password: Password, Agent: s.Agent, Model: s.Model, Logs: logs}
}

// RequireBrowser is for the tests that open Chrome: it asks for npx, and for a
// node that the pinned package runs on. A missing prerequisite is a failure, not
// a skip: a run that leaves the browser suites out must say so, or the gate reads
// as green for what was never run. -short leaves them out on purpose.
func RequireBrowser(t testing.TB) {
	t.Helper()
	if testing.Short() {
		t.Skip("opens a browser")
	}
	if _, err := exec.LookPath("npx"); err != nil {
		t.Fatal("npx is not installed: the browser tests drive Chrome through Chrome DevTools MCP (use -short to leave them out)")
	}
	// npx runs the package on the node of the path. A node that is too old for it
	// fails as the server starts, in words that do not name the cause.
	out, err := exec.Command("node", "--version").Output()
	if err != nil {
		t.Fatalf("node --version: %v: the browser tests run Chrome DevTools MCP on Node (use -short to leave them out)", err)
	}
	if version := strings.TrimSpace(string(out)); !nodeIsNewEnough(version) {
		t.Fatalf("the node on the path is %s, and %s (use -short to leave the browser tests out)", version, nodeNeeded)
	}
}

// nodeNeeded says which versions of Node the pinned package runs on: the
// "engines" of chrome-devtools-mcp 1.10.1.
const nodeNeeded = "the pinned chrome-devtools-mcp needs Node 20.19 or later in 20, 22.12 or later in 22, or 23 and later"

// nodeIsNewEnough reports whether a version of Node, as node --version prints
// it ("v22.12.0"), is one of those.
func nodeIsNewEnough(version string) bool {
	var major, minor int
	if _, err := fmt.Sscanf(strings.TrimPrefix(version, "v"), "%d.%d", &major, &minor); err != nil {
		return false
	}
	switch major {
	case 20:
		return minor >= 19
	case 22:
		return minor >= 12
	}
	return major >= 23
}

// BrowserOptions is how the evaluator starts Chrome unless it is told otherwise,
// so that these tests move with its pinned version.
func BrowserOptions(t testing.TB) devtools.Options {
	t.Helper()
	cfg, err := config.Load(func(string) (string, bool) { return "", false })
	require.NoError(t, err)
	return devtools.Options{Command: cfg.MCP.Command, Args: cfg.MCP.Args}
}
