package devtools_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
)

func TestCall_Results(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G'}
	s, _ := connectInMemory(t, map[string]toolFunc{
		"echo": func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			return textResult("pageId=" + strconv.Itoa(int(args["pageId"].(float64))) + " uid=" + args["uid"].(string)), nil
		},
		"two_blocks": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return textResult("Took a screenshot.", "Saved to <home>"), nil
		},
		"shot": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			res := textResult("Took a screenshot of the current page's viewport.")
			res.Content = append(res.Content, &mcp.ImageContent{MIMEType: "image/png", Data: png})
			return res, nil
		},
		"nothing": func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return textResult(), nil },
	}, devtools.Options{})

	t.Run("the arguments arrive and the text comes back", func(t *testing.T) {
		got, err := s.Call(t.Context(), "echo", map[string]any{"pageId": 2, "uid": "1_7"})

		require.NoError(t, err)
		assert.Equal(t, "pageId=2 uid=1_7", got.Text)
		assert.Empty(t, got.Images)
	})
	t.Run("blocks of text are joined line by line", func(t *testing.T) {
		got, err := s.Call(t.Context(), "two_blocks", nil)

		require.NoError(t, err)
		assert.Equal(t, "Took a screenshot.\nSaved to <home>", got.Text)
	})
	t.Run("an image comes back whole", func(t *testing.T) {
		got, err := s.Call(t.Context(), "shot", nil)

		require.NoError(t, err)
		require.Len(t, got.Images, 1)
		assert.Equal(t, "image/png", got.Images[0].MIMEType)
		assert.Equal(t, png, got.Images[0].Data)
		assert.Contains(t, got.Text, "Took a screenshot")
	})
	t.Run("a tool with nothing to say", func(t *testing.T) {
		got, err := s.Call(t.Context(), "nothing", nil)

		require.NoError(t, err)
		assert.Empty(t, got.Text)
	})
	t.Run("the server introduces itself", func(t *testing.T) {
		assert.Equal(t, devtools.ServerInfo{Name: "chrome_devtools", Version: "1.10.1"}, s.Info())
	})
}

func TestCall_ToolErrors(t *testing.T) {
	// What a tool prints when it says no, as two of them were recorded doing it.
	gone, goneIsError := fixture(t, "click_bad_uid")
	threw, threwIsError := fixture(t, "evaluate_script_throws")
	require.True(t, goneIsError && threwIsError, "the recordings are of tools that reported an error")
	s, _ := connectInMemory(t, map[string]toolFunc{
		"click":           func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return errorResult(gone), nil },
		"evaluate_script": func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return errorResult(threw), nil },
		"ok":              func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return textResult("fine"), nil },
	}, devtools.Options{})

	tests := []struct {
		name    string
		tool    string
		message string // the tool's own words, without the "Error: " they open with
	}{
		{"an element that is no longer on the page", "click", `Element uid "9_99" not found on page 2.`},
		{"a script that throws", "evaluate_script", "boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Call(t.Context(), tc.tool, nil)

			var toolErr *devtools.ToolError
			require.ErrorAs(t, err, &toolErr)
			assert.Equal(t, tc.tool, toolErr.Tool)
			assert.Equal(t, tc.message, toolErr.Message, "without the Error: prefix")
			assert.EqualError(t, err, tc.tool+": "+tc.message)
			assert.Contains(t, got.Text, tc.message, "the text is still there")
			assert.NotErrorIs(t, err, domain.ErrDriverDied)
		})
	}

	t.Run("the session goes on", func(t *testing.T) {
		res, err := s.Call(t.Context(), "ok", nil)

		require.NoError(t, err)
		assert.Equal(t, "fine", res.Text)
	})
	t.Run("a long message is cut", func(t *testing.T) {
		long, _ := connectInMemory(t, map[string]toolFunc{
			"boom": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
				return errorResult("Error: " + strings.Repeat("x", 2000)), nil
			},
		}, devtools.Options{})

		_, err := long.Call(t.Context(), "boom", nil)

		var toolErr *devtools.ToolError
		require.ErrorAs(t, err, &toolErr)
		assert.LessOrEqual(t, len([]rune(toolErr.Message)), 501)
	})
}

func TestCall_NotAnswering(t *testing.T) {
	slow := func(d time.Duration) toolFunc {
		return func(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
			select {
			case <-time.After(d):
				return textResult("done"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}

	t.Run("a tool that takes too long means the browser is lost", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var served atomic.Int32
			s, _ := connectInMemory(t, map[string]toolFunc{
				"hang": slow(time.Hour),
				"ok": func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
					served.Add(1)
					return textResult("fine"), nil
				},
			}, devtools.Options{CallTimeout: 5 * time.Second})
			start := time.Now()

			_, err := s.Call(t.Context(), "hang", nil)

			require.ErrorIs(t, err, domain.ErrDriverDied)
			assert.ErrorContains(t, err, "hang did not answer within 5s")
			assert.Equal(t, 5*time.Second, time.Since(start))

			_, err = s.Call(t.Context(), "ok", nil)
			require.ErrorIs(t, err, domain.ErrDriverDied, "and the session refuses more")
			assert.Zero(t, served.Load(), "without asking the server")
		})
	})
	t.Run("a call can be given more time", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _ := connectInMemory(t, map[string]toolFunc{"audit": slow(40 * time.Second)}, devtools.Options{CallTimeout: 5 * time.Second})

			got, err := s.Call(t.Context(), "audit", nil, devtools.WithTimeout(2*time.Minute))

			require.NoError(t, err)
			assert.Equal(t, "done", got.Text)
		})
	})
	t.Run("or less", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _ := connectInMemory(t, map[string]toolFunc{"quick": slow(10 * time.Second)}, devtools.Options{})

			_, err := s.Call(t.Context(), "quick", nil, devtools.WithTimeout(time.Second))

			require.ErrorIs(t, err, domain.ErrDriverDied)
		})
	})
	t.Run("the default is a minute", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _ := connectInMemory(t, map[string]toolFunc{"hang": slow(time.Hour)}, devtools.Options{})
			start := time.Now()

			_, err := s.Call(t.Context(), "hang", nil)

			require.ErrorIs(t, err, domain.ErrDriverDied)
			assert.Equal(t, time.Minute, time.Since(start))
		})
	})
	t.Run("the caller giving up is not the browser dying", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _ := connectInMemory(t, map[string]toolFunc{
				"hang": slow(time.Hour),
				"ok":   func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return textResult("fine"), nil },
			}, devtools.Options{})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()

			_, err := s.Call(ctx, "hang", nil)

			require.ErrorIs(t, err, context.DeadlineExceeded)
			assert.NotErrorIs(t, err, domain.ErrDriverDied)
			res, err := s.Call(t.Context(), "ok", nil)
			require.NoError(t, err, "the session is still good")
			assert.Equal(t, "fine", res.Text)
		})
	})
	t.Run("Ctrl-C is Ctrl-C", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _ := connectInMemory(t, map[string]toolFunc{"hang": slow(time.Hour)}, devtools.Options{})
			ctx, cancel := context.WithCancel(t.Context())
			time.AfterFunc(time.Second, cancel)

			_, err := s.Call(ctx, "hang", nil)

			require.ErrorIs(t, err, context.Canceled)
			assert.NotErrorIs(t, err, domain.ErrDriverDied)
		})
	})
}

func TestCall_TheServerGoesAway(t *testing.T) {
	t.Run("between calls", func(t *testing.T) {
		s, server := connectInMemory(t, map[string]toolFunc{
			"ok": func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return textResult("fine"), nil },
		}, devtools.Options{})
		require.NoError(t, server.Close())

		_, err := s.Call(t.Context(), "ok", nil)

		require.ErrorIs(t, err, domain.ErrDriverDied)
		_, err = s.Call(t.Context(), "ok", nil)
		require.ErrorIs(t, err, domain.ErrDriverDied, "and stays that way")
	})
	// Some of the ways a server ends reach the caller as words alone, with no
	// error underneath to compare with.
	for _, words := range []string{"write |1: broken pipe", "read |0: file already closed", "the connection closed under the call"} {
		t.Run("in an error known only by its words: "+words, func(t *testing.T) {
			s, _ := connectInMemory(t, map[string]toolFunc{
				"lost": func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return nil, errors.New(words) },
				"ok":   func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return textResult("fine"), nil },
			}, devtools.Options{})

			_, err := s.Call(t.Context(), "lost", nil)

			require.ErrorIs(t, err, domain.ErrDriverDied)
			assert.ErrorContains(t, err, words)
			_, err = s.Call(t.Context(), "ok", nil)
			require.ErrorIs(t, err, domain.ErrDriverDied, "and stays that way")
		})
	}
}

func TestSession_SaysWhyItWasLost(t *testing.T) {
	// Whoever made the call may only start another browser and try again, so the
	// log is where the cause is kept: one warning, with the tool and the reason.
	waits := func(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	answers := func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return textResult("fine"), nil }
	refuses := func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
		return errorResult("Error: no such element"), nil
	}
	connect := func(t *testing.T, log *bytes.Buffer) (*devtools.Session, *mcp.ServerSession) {
		t.Helper()
		return connectInMemory(t, map[string]toolFunc{"hang": waits, "ok": answers, "no": refuses},
			devtools.Options{CallTimeout: 5 * time.Second, Logger: slog.New(slog.NewTextHandler(log, nil))})
	}
	tests := []struct {
		name  string
		lose  func(t *testing.T, s *devtools.Session, server *mcp.ServerSession) error
		tool  string
		cause string
	}{
		{
			name: "a tool that does not answer in time", tool: "hang", cause: "hang did not answer within 5s",
			lose: func(t *testing.T, s *devtools.Session, _ *mcp.ServerSession) error {
				_, err := s.Call(t.Context(), "hang", nil)
				return err
			},
		},
		{
			name: "a server that went away", tool: "ok", cause: "calling ok: ",
			lose: func(t *testing.T, s *devtools.Session, server *mcp.ServerSession) error {
				require.NoError(t, server.Close())
				_, err := s.Call(t.Context(), "ok", nil)
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var log bytes.Buffer
				s, server := connect(t, &log)

				err := tc.lose(t, s, server)

				require.ErrorIs(t, err, domain.ErrDriverDied)
				_, err = s.Call(t.Context(), "ok", nil)
				require.ErrorIs(t, err, domain.ErrDriverDied, "the session refuses what follows")
				assert.Equal(t, 1, strings.Count(log.String(), "\n"), "it says so once, and not again for each call it then refuses: %s", log.String())
				assert.Contains(t, log.String(), "level=WARN")
				assert.Contains(t, log.String(), "tool="+tc.tool)
				assert.Contains(t, log.String(), tc.cause)
			})
		})
	}
	t.Run("a call that fails with the session still good says nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var log bytes.Buffer
			s, _ := connect(t, &log)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			_, gaveUp := s.Call(ctx, "hang", nil)
			_, refused := s.Call(t.Context(), "no", nil)
			_, unknown := s.Call(t.Context(), "no_such_tool", nil)

			require.ErrorIs(t, gaveUp, context.DeadlineExceeded)
			require.Error(t, refused)
			require.Error(t, unknown)
			_, err := s.Call(t.Context(), "ok", nil)
			require.NoError(t, err, "the session is still good")
			assert.Empty(t, log.String())
		})
	})
}

func TestOutputOf_KeepsTheEnd(t *testing.T) {
	// A server that fails says why in the last thing it prints, after whatever it
	// printed on its way there.
	const complaint = "fatal: Chrome could not be started"
	tests := []struct {
		name    string
		printed []string
		want    func(t *testing.T, got string)
	}{
		{name: "nothing was printed", want: func(t *testing.T, got string) { assert.Empty(t, got) }},
		{
			name: "a short output is given whole, on one line", printed: []string{"warn: no display\n", complaint + "\n"},
			want: func(t *testing.T, got string) {
				assert.Equal(t, "; its output ended with: warn: no display "+complaint, got)
			},
		},
		{
			name: "of a long output, the end", printed: append(slices.Repeat([]string{"debug: still starting the browser\n"}, 100), complaint+"\n"),
			want: func(t *testing.T, got string) {
				assert.True(t, strings.HasSuffix(got, complaint), "the complaint is the last thing in it: %s", got)
				kept, cut := strings.CutPrefix(got, "; its output ended with: …")
				assert.True(t, cut, "a mark says that the start was left out")
				assert.Equal(t, 600, utf8.RuneCountInString(kept), "and no more than fits in a message")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			output := devtools.NewTailBuffer(4096)
			for _, line := range tc.printed {
				_, _ = output.Write([]byte(line))
			}

			tc.want(t, devtools.OutputOf(output))
		})
	}
}

func TestCall_ProtocolErrors(t *testing.T) {
	s, _ := connectInMemory(t, map[string]toolFunc{
		"ok": func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return textResult("fine"), nil },
	}, devtools.Options{})

	_, err := s.Call(t.Context(), "no_such_tool", nil)

	require.Error(t, err)
	assert.ErrorContains(t, err, "calling no_such_tool")
	assert.NotErrorIs(t, err, domain.ErrDriverDied, "the server answered; it is only that the request was wrong")
	res, err := s.Call(t.Context(), "ok", nil)
	require.NoError(t, err)
	assert.Equal(t, "fine", res.Text)
}

func TestClose(t *testing.T) {
	s, _ := connectInMemory(t, map[string]toolFunc{
		"ok": func(context.Context, map[string]any) (*mcp.CallToolResult, error) { return textResult("fine"), nil },
	}, devtools.Options{})

	require.NoError(t, s.Close(t.Context()))
	require.NoError(t, s.Close(t.Context()), "twice is fine")
	_, err := s.Call(t.Context(), "ok", nil)

	require.ErrorIs(t, err, domain.ErrDriverDied)
	assert.ErrorContains(t, err, "closed or was lost")
}
