package devtools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// clientVersion is what the evaluator calls itself in the MCP handshake.
const clientVersion = "0.1.0"

// Default timeouts.
const (
	defaultStartTimeout = 3 * time.Minute // the first run may download the package
	defaultCallTimeout  = time.Minute
	terminateWait       = 5 * time.Second
	stderrKept          = 4096
)

// Options say how to start the server and how long to wait for it.
type Options struct {
	// Command and Args are EVAL_MCP_COMMAND and EVAL_MCP_ARGS.
	Command, Args []string
	// Environ is the environment to take the server's from; nil means this
	// process's. Only what ChildEnv lets through is passed on.
	Environ []string
	// StartTimeout bounds the start and the handshake.
	StartTimeout time.Duration
	// CallTimeout bounds one tool call, unless the call asks for another.
	CallTimeout time.Duration
	Logger      *slog.Logger
}

// ServerInfo is what the server says about itself in the handshake.
type ServerInfo struct{ Name, Version string }

// Session is a connection to one Chrome DevTools MCP server, and so to one
// headless Chrome. It is for one driver: calls to it should not overlap.
type Session struct {
	cs *mcp.ClientSession
	// kill ends the server and what it started; nil when the server runs in
	// this process (tests).
	kill        context.CancelFunc
	stderr      *tailBuffer
	callTimeout time.Duration
	log         *slog.Logger

	dead      atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

// Open starts the server and completes the handshake. The server is not tied to
// ctx, which only bounds the start: it runs until Close.
func Open(ctx context.Context, opts Options) (*Session, error) {
	environ := opts.Environ
	if environ == nil {
		environ = os.Environ()
	}
	procCtx, kill := context.WithCancel(context.WithoutCancel(ctx))
	cmd, err := BuildCommand(procCtx, opts.Command, opts.Args, environ)
	if err != nil {
		kill()
		return nil, err
	}
	stderr := &tailBuffer{max: stderrKept}
	cmd.Stderr = stderr

	// A server that does not answer is not waited for politely: when the time
	// for starting is up, it is killed.
	startCtx, cancel := context.WithTimeout(ctx, cmp.Or(opts.StartTimeout, defaultStartTimeout))
	defer cancel()
	giveUp := context.AfterFunc(startCtx, kill)
	client := mcp.NewClient(&mcp.Implementation{Name: "agent-eval", Version: clientVersion}, nil)
	cs, err := client.Connect(startCtx, &mcp.CommandTransport{Command: cmd, TerminateDuration: terminateWait}, nil)
	if !giveUp() && err == nil { // the time ran out just as it answered
		err = startCtx.Err()
		_ = cs.Close()
	}
	if err != nil {
		kill()
		return nil, fmt.Errorf("starting Chrome DevTools MCP (%s): %w%s", strings.Join(opts.Command, " "), err, outputOf(stderr))
	}
	s := newSession(cs, opts)
	s.kill, s.stderr = kill, stderr
	return s, nil
}

func newSession(cs *mcp.ClientSession, opts Options) *Session {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Session{cs: cs, callTimeout: cmp.Or(opts.CallTimeout, defaultCallTimeout), log: log}
}

// outputOf is what an error says of the output a server left behind: the end of
// it, which is where a program that fails says why.
func outputOf(t *tailBuffer) string {
	if t == nil || t.String() == "" {
		return ""
	}
	return "; its output ended with: " + ending(t.String(), 600)
}

// Info returns what the server called itself and its version.
func (s *Session) Info() ServerInfo {
	if r := s.cs.InitializeResult(); r != nil && r.ServerInfo != nil {
		return ServerInfo{Name: r.ServerInfo.Name, Version: r.ServerInfo.Version}
	}
	return ServerInfo{}
}

// Image is a picture a tool returned inline.
type Image struct {
	MIMEType string
	Data     []byte
}

// Result is what a tool returned.
type Result struct {
	// Text is the text content, one block per line.
	Text   string
	Images []Image
}

// ToolError is a tool that ran and said no: the element is not there, the
// script threw. The browser is fine; the call was not.
type ToolError struct {
	Tool    string
	Message string
}

func (e *ToolError) Error() string { return fmt.Sprintf("%s: %s", e.Tool, e.Message) }

// CallOption changes one call.
type CallOption func(*callConfig)

type callConfig struct{ timeout time.Duration }

// WithTimeout gives one call more (or less) time than the session's default,
// for tools that are slow by nature, such as a Lighthouse audit.
func WithTimeout(d time.Duration) CallOption { return func(c *callConfig) { c.timeout = d } }

// Call runs a tool.
//
// Errors are told apart by what the caller can do about them: ToolError means
// the page refused and the session is fine; the caller's context ending is
// returned as that context's error; anything that means the server or the
// browser is gone, or stopped answering, wraps domain.ErrDriverDied, and the
// session says why in its log and refuses further calls. A tool that does not
// answer in time counts as gone: a browser stuck in the middle of a call is not
// one to carry on with.
//
// Pattern: Three-Outcome Call
func (s *Session) Call(ctx context.Context, tool string, args map[string]any, opts ...CallOption) (Result, error) {
	if s.dead.Load() {
		return Result{}, fmt.Errorf("%w: the session is closed or was lost (calling %s)", domain.ErrDriverDied, tool)
	}
	cfg := callConfig{timeout: s.callTimeout}
	for _, o := range opts {
		o(&cfg)
	}
	callCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	res, err := s.cs.CallTool(callCtx, &mcp.CallToolParams{Name: tool, Arguments: args})
	switch {
	case err == nil:
		return toResult(tool, res)
	case ctx.Err() != nil:
		return Result{}, fmt.Errorf("calling %s: %w", tool, ctx.Err())
	case errors.Is(callCtx.Err(), context.DeadlineExceeded):
		return Result{}, s.lost(ctx, tool, fmt.Errorf("%s did not answer within %s", tool, cfg.timeout))
	case isGone(err):
		return Result{}, s.lost(ctx, tool, fmt.Errorf("calling %s: %v", tool, err))
	}
	return Result{}, fmt.Errorf("calling %s: %w", tool, err)
}

// lost declares the session lost, and says why in the log: a caller that takes
// domain.ErrDriverDied as its cue to start another browser may not report the
// cause, and the calls that follow are refused without a word.
func (s *Session) lost(ctx context.Context, tool string, cause error) error {
	s.dead.Store(true)
	err := fmt.Errorf("%w: %v%s", domain.ErrDriverDied, cause, outputOf(s.stderr))
	s.log.WarnContext(ctx, "the browser session was lost", "tool", tool, "error", err)
	return err
}

// isGone reports whether an error from the SDK means the server went away.
func isGone(err error) bool {
	if errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrProcessDone) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "file already closed") || strings.Contains(msg, "broken pipe") || strings.Contains(msg, "connection closed")
}

func toResult(tool string, res *mcp.CallToolResult) (Result, error) {
	var r Result
	var texts []string
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			texts = append(texts, v.Text)
		case *mcp.ImageContent:
			r.Images = append(r.Images, Image{MIMEType: v.MIMEType, Data: v.Data})
		}
	}
	r.Text = strings.Join(texts, "\n")
	if res.IsError {
		return r, &ToolError{Tool: tool, Message: shorten(strings.TrimPrefix(strings.TrimSpace(r.Text), "Error: "), 500)}
	}
	return r, nil
}

// Close ends the session: the server is asked to stop by closing its input, and
// if ctx ends first it, and the browser it started, are killed. It is safe to
// call more than once.
func (s *Session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.dead.Store(true)
		done := make(chan error, 1)
		go func() { done <- s.cs.Close() }()
		select {
		case err := <-done:
			s.closeErr = ignoreExit(err)
		case <-ctx.Done():
			s.killServer()
			s.closeErr = fmt.Errorf("closing Chrome DevTools MCP: %w", ctx.Err())
		}
	})
	return s.closeErr
}

// killServer ends the server and what it started, without waiting for it to
// say goodbye.
func (s *Session) killServer() {
	if s.kill != nil {
		s.kill()
	}
}

// ignoreExit drops the complaint about how the server ended: closing it is
// done by stopping it, and it is not news that it did not exit with status 0.
func ignoreExit(err error) error {
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return nil
	}
	return err
}
