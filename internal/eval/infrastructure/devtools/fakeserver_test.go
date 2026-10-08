package devtools_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
)

// toolFunc is a pretend tool of Chrome DevTools MCP.
type toolFunc func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error)

func textResult(parts ...string) *mcp.CallToolResult {
	res := &mcp.CallToolResult{}
	for _, p := range parts {
		res.Content = append(res.Content, &mcp.TextContent{Text: p})
	}
	return res
}

func errorResult(text string) *mcp.CallToolResult {
	res := textResult(text)
	res.IsError = true
	return res
}

// newFakeServer makes a server with the tools given, saying it is
// chrome-devtools-mcp 1.10.1.
func newFakeServer(tools map[string]toolFunc) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "chrome_devtools", Version: "1.10.1"}, nil)
	for name, fn := range tools {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args map[string]any
				if len(req.Params.Arguments) > 0 {
					if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
						return nil, err
					}
				}
				return fn(ctx, args)
			})
	}
	return server
}

// connectInMemory returns a session to a fake server running in this process,
// and the server's side of the connection.
func connectInMemory(t *testing.T, tools map[string]toolFunc, opts devtools.Options) (*devtools.Session, *mcp.ServerSession) {
	t.Helper()
	serverEnd, clientEnd := mcp.NewInMemoryTransports()
	ss, err := newFakeServer(tools).Connect(t.Context(), serverEnd, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), clientEnd, nil)
	require.NoError(t, err)
	s := devtools.NewSessionForTest(cs, opts)
	t.Cleanup(func() { _ = s.Close(context.Background()); _ = ss.Close() })
	return s, ss
}

// --- the fake server as a separate process --------------------------------------

// extraTools are tools some platforms add to the fake server.
var extraTools = map[string]toolFunc{}

// fakeServerFlag makes the test binary behave as a Chrome DevTools MCP server on
// its standard input and output, so Open can start a real process.
const fakeServerFlag = "--fake-mcp-server"

func TestMain(m *testing.M) {
	if i := slices.Index(os.Args, fakeServerFlag); i >= 0 {
		os.Exit(runFakeServer(os.Args[i+1:]))
	}
	os.Exit(m.Run())
}

func runFakeServer(args []string) int {
	behaviours := map[string]bool{}
	for _, a := range args {
		if b, ok := strings.CutPrefix(a, "--behavior="); ok {
			behaviours[b] = true
		}
	}
	switch {
	case behaviours["exit-at-start"]:
		fmt.Fprintln(os.Stderr, "fatal: refusing to start")
		return 3
	case behaviours["silent"]:
		fmt.Fprintf(os.Stderr, "pid=%d\n", os.Getpid())
		time.Sleep(time.Hour) // never answers the handshake
		return 0
	}

	tools := map[string]toolFunc{
		"echo": func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			data, _ := json.Marshal(args, json.Deterministic(true))
			return textResult(string(data)), nil
		},
		"env": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			var names []string
			for _, kv := range os.Environ() {
				name, _, _ := strings.Cut(kv, "=")
				names = append(names, name)
			}
			sort.Strings(names)
			return textResult(strings.Join(names, "\n")), nil
		},
		"pid": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return textResult(strconv.Itoa(os.Getpid())), nil
		},
		"crash": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			fmt.Fprintln(os.Stderr, "fatal: simulated crash")
			os.Exit(7)
			return nil, nil
		},
	}
	maps.Copy(tools, extraTools)
	if behaviours["stubborn"] {
		signal.Ignore(syscall.SIGTERM)
	}
	err := newFakeServer(tools).Run(context.Background(), &mcp.StdioTransport{})
	if behaviours["stubborn"] {
		time.Sleep(time.Hour) // does not leave when its input closes
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// fakeNpx writes a program named npx that starts this test binary as the server,
// with the behaviours given. What the evaluator checks before it starts anything
// is npx on a pinned package, and the test binary is neither; this is.
func fakeNpx(t testing.TB, behaviours ...string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in for npx is a shell script")
	}
	var flags strings.Builder
	for _, b := range behaviours {
		flags.WriteString(" --behavior=" + b)
	}
	path := filepath.Join(t.TempDir(), "npx")
	script := fmt.Sprintf("#!/bin/sh\nexec '%s' %s%s \"$@\"\n", os.Args[0], fakeServerFlag, flags.String())
	require.NoError(t, os.WriteFile(path, []byte(script), 0o700))
	return path
}

// processOptions starts this test binary as the server, through the stand-in for
// npx, with the behaviours given.
func processOptions(t testing.TB, behaviours ...string) devtools.Options {
	t.Helper()
	return devtools.Options{
		Command: []string{fakeNpx(t, behaviours...), "-y", "chrome-devtools-mcp@1.10.1"},
		Args:    []string{"--headless", "--isolated", "--no-usage-statistics", "--no-performance-crux"},
	}
}
