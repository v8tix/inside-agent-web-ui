package devtools_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
)

// recorded is what the observer returns for one finished attempt.
func recorded(mutate ...func(map[string]any)) map[string]any {
	m := map[string]any{
		"version":   1,
		"keydownAt": 1700000000000.5,
		"updates": []map[string]any{
			{"at": 1700000000200.25, "chars": 20}, {"at": 1700000000300.5, "chars": 80}, {"at": 1700000000400.75, "chars": 230},
		},
		"statuses": []map[string]any{
			{"at": 1700000000100.0, "status": "pending"}, {"at": 1700000000210.0, "status": "streaming"}, {"at": 1700000000500.0, "status": "complete"},
		},
		"scrolls": []map[string]any{{"at": 1700000000200.25, "fromBottomPx": 0, "scrollable": false}, {"at": 1700000000400.75, "fromBottomPx": 12.5, "scrollable": true}},
		"late":    false,
		"message": map[string]any{
			"id": "m-1", "status": "complete", "ttft": "150", "duration": "400",
			"promptTokens": "120", "completionTokens": "30", "toolCalls": "lookup,search",
		},
		"visibleText": "Thinking it over. FINAL ANSWER: 42",
		"unsafe":      map[string]any{"scripts": 0, "handlers": 0, "external": 0},
	}
	for _, f := range mutate {
		f(m)
	}
	return m
}

const emptyConsole = "## Console messages\nShowing 1-0 of 0 (Page 1 of 1)."

// collectPage is a tab the observer has recorded an attempt in.
func collectPage(rec any) *page {
	p := chatPage()
	p.scripts["collect"] = fixed(rec)
	p.tools["list_console_messages"] = ok(emptyConsole)
	p.tools["list_network_requests"] = ok("## Network requests\nShowing 1-1 of 1 (Page 1 of 1).\nreqid=1 GET http://web.test:8300/ [200]")
	return p
}

func TestCollect(t *testing.T) {
	t.Run("turns what was observed into observations", func(t *testing.T) {
		p := collectPage(recorded())
		p.tools["list_console_messages"] = ok("## Console messages\nShowing 1-2 of 2 (Page 1 of 1).\nmsgid=1 [log] ready (0 args)\nmsgid=2 [error] boom (1 args)")
		p.tools["list_network_requests"] = ok("## Network requests\nShowing 1-3 of 3 (Page 1 of 1).\n" +
			"reqid=1 GET http://web.test:8300/ [200]\nreqid=2 POST http://web.test:8300/c [200]\nreqid=3 GET http://web.test:8300/static/x.js [404]")
		d := p.driver(t, devtools.DriverConfig{})

		got, err := d.Collect(t.Context())

		require.NoError(t, err)
		assert.Equal(t, domain.UIObservations{
			KeydownAt: 1700000000000.5,
			Updates:   []domain.Update{{At: 1700000000200.25, Chars: 20}, {At: 1700000000300.5, Chars: 80}, {At: 1700000000400.75, Chars: 230}},
			Statuses: []domain.StatusChange{
				{At: 1700000000100, Status: domain.ReplyPending}, {At: 1700000000210, Status: domain.ReplyStreaming}, {At: 1700000000500, Status: domain.ReplyComplete},
			},
			Scrolls: []domain.ScrollSample{{At: 1700000000200.25, FromBottomPx: 0}, {At: 1700000000400.75, FromBottomPx: 12.5, Scrollable: true}},
			Console: []domain.ConsoleMessage{{Type: "log", Text: "ready (0 args)"}, {Type: "error", Text: "boom (1 args)"}},
			Requests: []domain.NetworkRequest{
				{Method: "GET", URL: "http://web.test:8300/", Status: 200},
				{Method: "POST", URL: "http://web.test:8300/c", Status: 200},
				{Method: "GET", URL: "http://web.test:8300/static/x.js", Status: 404},
			},
			Message: domain.MessageInfo{
				ID: "m-1", Status: domain.ReplyComplete, TTFTMs: new(int64(150)), DurationMs: new(int64(400)),
				PromptTokens: new(120), CompletionTokens: new(30), ToolCalls: []string{"lookup", "search"},
			},
			VisibleText: "Thinking it over. FINAL ANSWER: 42",
		}, got)

		ms, ok := got.UIOverhead()
		require.True(t, ok)
		assert.InDelta(t, 49.75, ms, 1e-9, "TTFT in the UI (199.75, Enter at .5 and the first text at .25) minus the agent's own (150)")
		latency, ok := got.Latency()
		require.True(t, ok)
		assert.InDelta(t, 499.5, latency, 1e-9)
	})
	t.Run("asks the page, not the tab list, and does not wait for it to settle", func(t *testing.T) {
		p := collectPage(recorded())
		d := p.driver(t, devtools.DriverConfig{})

		_, err := d.Collect(t.Context())

		require.NoError(t, err)
		assert.Equal(t, []string{"eval:collect", "list_console_messages", "list_network_requests"}, p.sequence())
		assert.Equal(t, false, p.called("evaluate_script")[0].args["waitForStableDom"])
	})
	t.Run("when Stop was pressed", func(t *testing.T) {
		p := collectPage(recorded(func(m map[string]any) { m["stopAt"] = 1700000000450.5 }))
		d := p.driver(t, devtools.DriverConfig{})

		got, err := d.Collect(t.Context())

		require.NoError(t, err)
		assert.InDelta(t, 1700000000450.5, got.StopAt, 1e-6)
	})
	t.Run("unsafe content in the answer body is counted", func(t *testing.T) {
		p := collectPage(recorded(func(m map[string]any) {
			m["unsafe"] = map[string]any{"scripts": 1, "handlers": 2, "external": 3}
		}))
		d := p.driver(t, devtools.DriverConfig{})

		got, err := d.Collect(t.Context())

		require.NoError(t, err)
		assert.Equal(t, domain.BodyFindings{Scripts: 1, EventHandlerAttrs: 2, ExternalImages: 3}, got.Unsafe)
	})
	t.Run("a page with no reply", func(t *testing.T) {
		p := collectPage(recorded(func(m map[string]any) {
			m["message"], m["unsafe"], m["updates"], m["statuses"], m["keydownAt"] = nil, nil, nil, nil, nil
			m["visibleText"] = ""
		}))
		d := p.driver(t, devtools.DriverConfig{})

		got, err := d.Collect(t.Context())

		require.NoError(t, err)
		assert.Equal(t, domain.MessageInfo{}, got.Message)
		assert.Zero(t, got.KeydownAt)
		assert.False(t, got.Unsafe.Any())
	})
	t.Run("telemetry the message does not have stays unknown, not zero", func(t *testing.T) {
		p := collectPage(recorded(func(m map[string]any) {
			m["message"] = map[string]any{"id": "m-1", "status": "complete", "ttft": nil, "duration": "oops", "promptTokens": nil, "completionTokens": "", "toolCalls": ""}
		}))
		d := p.driver(t, devtools.DriverConfig{})

		got, err := d.Collect(t.Context())

		require.NoError(t, err)
		assert.Nil(t, got.Message.TTFTMs)
		assert.Nil(t, got.Message.DurationMs, "a value that is not a number is not a number")
		assert.Nil(t, got.Message.PromptTokens)
		assert.Nil(t, got.Message.CompletionTokens)
		assert.Empty(t, got.Message.ToolCalls)
	})
	t.Run("a message without an id or status", func(t *testing.T) {
		p := collectPage(recorded(func(m map[string]any) { m["message"] = map[string]any{"id": nil, "status": nil} }))
		d := p.driver(t, devtools.DriverConfig{})

		got, err := d.Collect(t.Context())

		require.NoError(t, err)
		assert.Empty(t, got.Message.ID)
		assert.Empty(t, got.Message.Status)
	})
}

func TestCollect_TheObserverIsMissing(t *testing.T) {
	t.Run("it is put in late, and the result says so", func(t *testing.T) {
		p := collectPage(nil)
		p.scripts["installLate"] = fixed(recorded(func(m map[string]any) { m["late"] = true }))
		d := p.driver(t, devtools.DriverConfig{})

		got, err := d.Collect(t.Context())

		require.NoError(t, err)
		assert.True(t, got.ObserverLate)
		assert.Equal(t, []string{"eval:collect", "eval:installLate", "list_console_messages", "list_network_requests"}, p.sequence())
		var installed string
		for _, c := range p.called("evaluate_script") {
			if f := c.args["function"].(string); scriptName(f) == "installLate" {
				installed = f
			}
		}
		assert.Contains(t, installed, "window.__agentEval", "the observer itself")
		assert.Contains(t, installed, "markLate()", "marked as late, so no timing is taken from it")
	})
	t.Run("and if it cannot be put in, that is an error", func(t *testing.T) {
		p := collectPage(nil)
		p.scripts["installLate"] = fixed(nil)
		d := p.driver(t, devtools.DriverConfig{})

		_, err := d.Collect(t.Context())

		require.ErrorContains(t, err, "observer could not be installed")
	})
	t.Run("or the script fails", func(t *testing.T) {
		p := collectPage(nil)
		p.scripts["installLate"] = func(map[string]any) (any, error) { return nil, fmt.Errorf("SyntaxError") }
		d := p.driver(t, devtools.DriverConfig{})

		_, err := d.Collect(t.Context())

		require.ErrorContains(t, err, "installLate")
	})
}

func TestCollect_Failures(t *testing.T) {
	t.Run("the observer's record cannot be read", func(t *testing.T) {
		p := collectPage(nil)
		p.scripts["collect"] = func(map[string]any) (any, error) { return nil, fmt.Errorf("Execution context was destroyed") }
		d := p.driver(t, devtools.DriverConfig{})

		_, err := d.Collect(t.Context())

		require.ErrorContains(t, err, "collect: ")
	})
	t.Run("a record of the wrong shape", func(t *testing.T) {
		p := collectPage("not an object")
		d := p.driver(t, devtools.DriverConfig{})

		_, err := d.Collect(t.Context())

		require.ErrorContains(t, err, "reading the result")
	})
	t.Run("the console cannot be read", func(t *testing.T) {
		p := collectPage(recorded())
		p.tools["list_console_messages"] = func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: page closed"), nil }
		d := p.driver(t, devtools.DriverConfig{})

		_, err := d.Collect(t.Context())

		require.ErrorContains(t, err, "reading the console")
		assert.ErrorContains(t, err, "page closed")
	})
	t.Run("the network log cannot be read", func(t *testing.T) {
		p := collectPage(recorded())
		p.tools["list_network_requests"] = func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: page closed"), nil }
		d := p.driver(t, devtools.DriverConfig{})

		_, err := d.Collect(t.Context())

		require.ErrorContains(t, err, "reading the network log")
	})
}

func TestCollect_Paging(t *testing.T) {
	t.Run("every page of a long listing is read", func(t *testing.T) {
		p := collectPage(recorded())
		var mu sync.Mutex
		var asked []any
		p.tools["list_console_messages"] = func(args map[string]any) (*mcp.CallToolResult, error) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, args["pageIdx"])
			switch args["pageIdx"] {
			case float64(0):
				return textResult("## Console messages\nShowing 1-2 of 3 (Page 1 of 2).\nmsgid=1 [log] a\nmsgid=2 [warn] b"), nil
			default:
				return textResult("## Console messages\nShowing 3-3 of 3 (Page 2 of 2).\nmsgid=3 [error] c"), nil
			}
		}
		d := p.driver(t, devtools.DriverConfig{})

		got, err := d.Collect(t.Context())

		require.NoError(t, err)
		assert.Equal(t, []any{float64(0), float64(1)}, asked)
		require.Len(t, got.Console, 3)
		assert.Equal(t, "error", got.Console[2].Type)
		assert.Equal(t, float64(200), p.called("list_console_messages")[0].args["pageSize"])
		assert.Equal(t, 1, got.ConsoleErrors())
	})
}

// What list_console_messages printed for a small page, three times. The driver
// reads each as one page of one, and of what it holds only the errors count
// against the page: an issue that the browser raises itself does not.
func TestCollect_TheRecordedConsole(t *testing.T) {
	whole, _ := fixture(t, "list_console_messages")
	errorsOnly, _ := fixture(t, "list_console_messages_errors")
	afterReload, _ := fixture(t, "list_console_messages_after_reload")
	tests := []struct {
		name     string
		listing  string
		messages int
		errors   int
	}{
		{"an issue and three errors", whole, 4, 3},
		{"the errors alone", errorsOnly, 3, 3},
		{"after a reload there is what the new page logged, an issue", afterReload, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := collectPage(recorded())
			p.tools["list_console_messages"] = ok(tc.listing)
			d := p.driver(t, devtools.DriverConfig{})

			got, err := d.Collect(t.Context())

			require.NoError(t, err)
			assert.Len(t, got.Console, tc.messages)
			assert.Equal(t, tc.errors, got.ConsoleErrors())
			assert.Len(t, p.called("list_console_messages"), 1, "the listing says that it is one page of one")
		})
	}
}

// A page that logs without end must not keep the evaluator reading: a listing is
// read up to a page limit, and one that is cut there says so, since what came
// after is not in the observations.
func TestCollect_AListingThatDoesNotEndIsCutAndSaysSo(t *testing.T) {
	tests := []struct {
		name   string
		listed int // the pages the listing says it has
		warned bool
	}{
		{name: "a listing as long as the limit is read whole, without a word", listed: 50},
		{name: "a longer one is cut at the limit, with a warning", listed: 9999, warned: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := collectPage(recorded())
			p.tools["list_network_requests"] = func(args map[string]any) (*mcp.CallToolResult, error) {
				page := int(args["pageIdx"].(float64)) + 1
				return textResult(fmt.Sprintf("## Network requests\nShowing %d-%d of %d (Page %d of %d).\nreqid=%d GET http://x/ [200]",
					page, page, tc.listed, page, tc.listed, page)), nil
			}
			var log bytes.Buffer
			d := p.driver(t, devtools.DriverConfig{Logger: slog.New(slog.NewTextHandler(&log, nil))})

			got, err := d.Collect(t.Context())

			require.NoError(t, err)
			assert.Len(t, p.called("list_network_requests"), 50)
			assert.Len(t, got.Requests, 50)
			if !tc.warned {
				assert.Empty(t, log.String())
				return
			}
			assert.Equal(t, 1, strings.Count(log.String(), "\n"), "one warning, and none for the console, whose listing ended: %s", log.String())
			assert.Contains(t, log.String(), "level=WARN")
			assert.Contains(t, log.String(), "tool=list_network_requests")
			assert.Contains(t, log.String(), "pages=50")
			assert.Contains(t, log.String(), "pages_listed=9999")
		})
	}
}

func TestObserverScript(t *testing.T) {
	src := devtools.ScriptForTest("observer")

	// The observer runs in a real browser (see the integration tests); these
	// guard the properties the numbers depend on, so that an edit that loses one
	// is noticed without a browser.
	assert.Contains(t, src, "performance.timeOrigin + performance.now()", "timing uses the page's clock, not the duration of tool calls")
	assert.Contains(t, src, "if (window.__agentEval) return;", "installing it twice does nothing")
	assert.Contains(t, src, "[data-testid=composer-input]", "the start of an attempt is Enter in the composer")
	assert.Contains(t, src, "[data-testid=stop]", "and Stop is timed from the click")
	assert.Contains(t, src, "aria-hidden=true", "a typing indicator is not text")
	assert.Contains(t, src, "requestAnimationFrame", "the scroll position is read after the page has had its turn")
	assert.NotContains(t, src, "fetch(", "the observer only watches; it makes no requests")
	assert.NotContains(t, src, "XMLHttpRequest")
	assert.NotContains(t, strings.ToLower(src), "sendbeacon")
}
