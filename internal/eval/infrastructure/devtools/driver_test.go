package devtools_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/contract"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
)

const baseURL = "http://web.test:8300"

// call is one tool call the driver made.
type call struct {
	tool string
	args map[string]any
}

// page is a pretend browser tab: what each tool answers. The driver is judged by
// what it asks for and what it does with the answers.
type page struct {
	mu    sync.Mutex
	calls []call

	// tools answers a tool by name; a tool with no entry is not expected.
	tools map[string]func(args map[string]any) (*mcp.CallToolResult, error)
	// scripts answers evaluate_script by the name on the function's first line.
	scripts map[string]func(args map[string]any) (any, error)
}

func newPage() *page {
	return &page{
		tools:   map[string]func(map[string]any) (*mcp.CallToolResult, error){},
		scripts: map[string]func(map[string]any) (any, error){},
	}
}

func (p *page) record(tool string, args map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call{tool: tool, args: args})
}

// toolNames lists the tools called, with the script's name for evaluate_script.
func (p *page) sequence() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.calls {
		name := c.tool
		if c.tool == "evaluate_script" {
			name = "eval:" + scriptName(c.args["function"].(string))
		}
		out = append(out, name)
	}
	return out
}

func (p *page) called(tool string) []call {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []call
	for _, c := range p.calls {
		if c.tool == tool {
			out = append(out, c)
		}
	}
	return out
}

func scriptName(function string) string {
	first, _, _ := strings.Cut(function, "\n")
	return strings.TrimSpace(strings.TrimPrefix(first, "// eval:"))
}

func (p *page) serverTools() map[string]toolFunc {
	tools := map[string]toolFunc{}
	register := func(name string) {
		tools[name] = func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			p.record(name, args)
			if name == "evaluate_script" {
				fn, _ := args["function"].(string)
				handler := p.scripts[scriptName(fn)]
				if handler == nil {
					return errorResult("Error: no script " + scriptName(fn) + " is expected"), nil
				}
				value, err := handler(args)
				if err != nil {
					return errorResult("Error: " + err.Error()), nil
				}
				data, _ := json.Marshal(value)
				return textResult("Script ran on page and returned:\n```json\n" + string(data) + "\n```"), nil
			}
			handler := p.tools[name]
			if handler == nil {
				return errorResult("Error: tool " + name + " is not expected"), nil
			}
			return handler(args)
		}
	}
	for _, name := range []string{"new_page", "navigate_page", "take_snapshot", "fill", "click", "press_key", "type_text",
		"evaluate_script", "take_screenshot", "list_console_messages", "list_network_requests", "emulate",
		"lighthouse_audit", "performance_start_trace", "performance_stop_trace"} {
		register(name)
	}
	return tools
}

// driver connects a driver to the pretend tab.
func (p *page) driver(t *testing.T, cfg devtools.DriverConfig) *devtools.Driver {
	t.Helper()
	if cfg.BaseURL == "" {
		cfg.BaseURL = baseURL
	}
	s, _ := connectInMemory(t, p.serverTools(), devtools.Options{})
	return devtools.NewDriver(s, cfg)
}

// ok answers a tool with a plain message.
func ok(msg string) func(map[string]any) (*mcp.CallToolResult, error) {
	return func(map[string]any) (*mcp.CallToolResult, error) { return textResult(msg), nil }
}

// fixed answers evaluate_script with one value.
func fixed(v any) func(map[string]any) (any, error) {
	return func(map[string]any) (any, error) { return v, nil }
}

// sequenceOf answers successive calls with successive values; the last repeats.
func sequenceOf(values ...any) func(map[string]any) (any, error) {
	var mu sync.Mutex
	i := 0
	return func(map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		v := values[min(i, len(values)-1)]
		i++
		if err, isErr := v.(error); isErr {
			return nil, err
		}
		return v, nil
	}
}

func snapshotOf(lines ...string) string {
	var b strings.Builder
	b.WriteString("## Latest page snapshot\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.String()
}

const pagesAfterNew = "## Pages\n1: about:blank\n2: Sign in (http://web.test:8300/login) [selected]"

var (
	loginSnapshot = snapshotOf(
		`uid=1_0 RootWebArea "Sign in"`,
		`  uid=1_1 textbox "Email" focusable focused`,
		`  uid=1_2 textbox "Password" focusable`,
		`  uid=1_3 button "Sign in"`,
	)
	homeSnapshot = snapshotOf(
		`uid=3_0 RootWebArea "Agent"`,
		`  uid=3_1 link "New chat"`,
		`  uid=3_2 textbox "Message" focusable multiline`,
		`  uid=3_3 button "Send message"`,
	)
)

func state(contract any, mutate ...func(map[string]any)) map[string]any {
	m := map[string]any{"contract": contract, "path": "/login", "composer": false, "login": true, "alert": nil}
	for _, f := range mutate {
		f(m)
	}
	return m
}

func signedIn(m map[string]any) { m["path"], m["login"], m["composer"] = "/", false, true }

func argOf(c call, key string) any { return c.args[key] }

// --- Login ---------------------------------------------------------------------------

func loginPage() *page {
	p := newPage()
	p.tools["new_page"] = ok(pagesAfterNew)
	p.tools["emulate"] = ok("Emulation configured successfully")
	p.tools["take_snapshot"] = ok(loginSnapshot)
	p.tools["fill"] = ok("Successfully filled out the element")
	p.tools["click"] = ok("Successfully clicked on the element")
	return p
}

func TestLogin(t *testing.T) {
	t.Run("signs in", func(t *testing.T) {
		p := loginPage()
		p.scripts["pageState"] = sequenceOf(state("1"), state("1"), state("1", signedIn), state("1", signedIn))
		d := p.driver(t, devtools.DriverConfig{PollInterval: time.Millisecond})

		err := d.Login(t.Context(), "eval@example.com", "s3cret-pw")

		require.NoError(t, err)
		assert.Equal(t, []string{
			"new_page", "emulate", "eval:pageState", "take_snapshot", "fill", "fill", "take_snapshot", "click",
			"eval:pageState", "eval:pageState", "eval:pageState",
		}, p.sequence())
		assert.Equal(t, baseURL+"/login", argOf(p.called("new_page")[0], "url"))
		assert.Equal(t, map[string]any{"viewport": "1280x800x1", "pageId": float64(2)}, p.called("emulate")[0].args,
			"the window is a laptop's, not whatever the browser starts with")
		fills := p.called("fill")
		assert.Equal(t, map[string]any{"uid": "1_1", "value": "eval@example.com", "pageId": float64(2)}, fills[0].args)
		assert.Equal(t, map[string]any{"uid": "1_2", "value": "s3cret-pw", "pageId": float64(2)}, fills[1].args)
		assert.Equal(t, "1_3", argOf(p.called("click")[0], "uid"))
		for _, c := range p.called("evaluate_script") {
			assert.Equal(t, false, c.args["waitForStableDom"], "reading the page must not wait for it to settle")
			assert.Equal(t, float64(2), c.args["pageId"])
		}
	})
	t.Run("refuses a page that speaks another contract before typing anything", func(t *testing.T) {
		p := loginPage()
		p.scripts["pageState"] = fixed(state("2"))
		d := p.driver(t, devtools.DriverConfig{})

		err := d.Login(t.Context(), "a@b.c", "pw")

		require.ErrorIs(t, err, domain.ErrContractVersion)
		assert.ErrorContains(t, err, `data-ui-contract="2"`)
		assert.ErrorContains(t, err, `built for "1"`)
		assert.Empty(t, p.called("fill"), "nothing was typed into it")
	})
	t.Run("and one that declares none", func(t *testing.T) {
		p := loginPage()
		p.scripts["pageState"] = fixed(state(nil))
		d := p.driver(t, devtools.DriverConfig{})

		err := d.Login(t.Context(), "a@b.c", "pw")

		require.ErrorIs(t, err, domain.ErrContractVersion)
		assert.ErrorContains(t, err, "declares no UI contract version")
	})
	t.Run("refused credentials", func(t *testing.T) {
		p := loginPage()
		p.scripts["pageState"] = sequenceOf(state("1"), state("1", func(m map[string]any) { m["alert"] = "Invalid email or password." }))
		d := p.driver(t, devtools.DriverConfig{PollInterval: time.Millisecond})

		err := d.Login(t.Context(), "a@b.c", "wrong")

		require.ErrorIs(t, err, domain.ErrLoginFailed)
		assert.ErrorContains(t, err, "Invalid email or password.")
		assert.NotContains(t, err.Error(), "wrong", "the password is not in the error")
	})
	t.Run("a sign-in that never completes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := loginPage()
			p.scripts["pageState"] = fixed(state("1"))
			d := p.driver(t, devtools.DriverConfig{SettleTimeout: 3 * time.Second})

			err := d.Login(t.Context(), "a@b.c", "pw")

			require.Error(t, err)
			assert.ErrorContains(t, err, "waiting for sign-in to finish")
			assert.ErrorIs(t, err, context.DeadlineExceeded)
		})
	})
	t.Run("a page missing a field says what it has", func(t *testing.T) {
		p := loginPage()
		p.tools["take_snapshot"] = ok(snapshotOf(`uid=1_0 RootWebArea "Sign in"`, `  uid=1_1 textbox "Email"`, `  uid=1_3 button "Sign in"`))
		p.scripts["pageState"] = fixed(state("1"))
		d := p.driver(t, devtools.DriverConfig{})

		err := d.Login(t.Context(), "a@b.c", "pw")

		require.Error(t, err)
		assert.ErrorContains(t, err, `the sign-in page: the page has no textbox named "Password"`)
		assert.Empty(t, p.called("fill"))
	})
	t.Run("another window size, if asked", func(t *testing.T) {
		p := loginPage()
		p.scripts["pageState"] = sequenceOf(state("1"), state("1", signedIn), state("1", signedIn))
		d := p.driver(t, devtools.DriverConfig{Viewport: "1920x1080x2", PollInterval: time.Millisecond})

		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))

		assert.Equal(t, "1920x1080x2", p.called("emulate")[0].args["viewport"])
	})
	t.Run("a window that cannot be set", func(t *testing.T) {
		p := loginPage()
		p.tools["emulate"] = func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: bad viewport"), nil }
		d := p.driver(t, devtools.DriverConfig{})

		err := d.Login(t.Context(), "a@b.c", "pw")

		require.ErrorContains(t, err, "setting the size of the browser window")
		assert.Empty(t, p.called("fill"))
	})
	t.Run("the browser will not open the page", func(t *testing.T) {
		p := newPage()
		p.tools["new_page"] = func(map[string]any) (*mcp.CallToolResult, error) {
			return errorResult("Error: net::ERR_CONNECTION_REFUSED"), nil
		}
		d := p.driver(t, devtools.DriverConfig{})

		err := d.Login(t.Context(), "a@b.c", "pw")

		require.Error(t, err)
		assert.ErrorContains(t, err, "opening the Web UI at "+baseURL)
		assert.ErrorContains(t, err, "ERR_CONNECTION_REFUSED")
	})
	// EVAL_BASE_URL can carry a user and a password. The browser is given the
	// address as it is; an error names where it points and nothing else.
	t.Run("an address with credentials is named without them", func(t *testing.T) {
		const withCredentials = "http://canary-user:canary-pass@web.test:8300"
		t.Run("when the browser will not open the page", func(t *testing.T) {
			p := newPage()
			p.tools["new_page"] = func(map[string]any) (*mcp.CallToolResult, error) {
				return errorResult("Error: net::ERR_CONNECTION_REFUSED"), nil
			}
			d := p.driver(t, devtools.DriverConfig{BaseURL: withCredentials})

			err := d.Login(t.Context(), "a@b.c", "pw")

			require.ErrorContains(t, err, "opening the Web UI at "+baseURL+": ")
			assert.NotContains(t, err.Error(), "canary")
			assert.Equal(t, withCredentials+"/login", argOf(p.called("new_page")[0], "url"), "the browser needs them")
		})
		t.Run("when the page declares no contract", func(t *testing.T) {
			p := loginPage()
			p.scripts["pageState"] = fixed(state(nil))
			d := p.driver(t, devtools.DriverConfig{BaseURL: withCredentials})

			err := d.Login(t.Context(), "a@b.c", "pw")

			require.ErrorIs(t, err, domain.ErrContractVersion)
			assert.ErrorContains(t, err, "the page at "+baseURL+" declares no UI contract version")
			assert.NotContains(t, err.Error(), "canary")
		})
	})
	t.Run("a browser that reports no page", func(t *testing.T) {
		p := newPage()
		p.tools["new_page"] = ok("## Pages\n1: about:blank")
		d := p.driver(t, devtools.DriverConfig{})

		err := d.Login(t.Context(), "a@b.c", "pw")

		require.ErrorContains(t, err, "reported no page")
	})
	t.Run("a failing fill or click", func(t *testing.T) {
		for _, tool := range []string{"fill", "click"} {
			p := loginPage()
			p.scripts["pageState"] = fixed(state("1"))
			p.tools[tool] = func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: boom"), nil }
			d := p.driver(t, devtools.DriverConfig{})

			err := d.Login(t.Context(), "a@b.c", "pw")

			var toolErr *devtools.ToolError
			require.ErrorAs(t, err, &toolErr, tool)
		}
	})
	t.Run("the contract is the one the Web UI declares", func(t *testing.T) {
		assert.Equal(t, contract.Version, devtools.SupportedContract,
			"if the Web UI's contract version was raised, the driver has to be brought up to date with it before this is changed")
	})
}

// --- StartChat and Submit ------------------------------------------------------------

func chatPage() *page {
	p := loginPage()
	var mu sync.Mutex
	shown := loginSnapshot
	p.tools["take_snapshot"] = func(map[string]any) (*mcp.CallToolResult, error) {
		mu.Lock()
		defer mu.Unlock()
		return textResult(shown), nil
	}
	p.tools["click"] = func(map[string]any) (*mcp.CallToolResult, error) {
		mu.Lock()
		defer mu.Unlock()
		shown = homeSnapshot // the only click before the start page is Sign in
		return textResult("Successfully clicked on the element"), nil
	}
	p.tools["navigate_page"] = ok("Successfully navigated")
	p.tools["press_key"] = ok("Successfully pressed key: Enter")
	p.tools["type_text"] = ok("Typed text")
	p.scripts["pageState"] = fixed(state("1", signedIn))
	return p
}

func TestStartChat(t *testing.T) {
	t.Run("opens the start page with the observer", func(t *testing.T) {
		p := chatPage()
		d := p.driver(t, devtools.DriverConfig{})
		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))

		require.NoError(t, d.StartChat(t.Context()))

		nav := p.called("navigate_page")
		require.Len(t, nav, 1)
		assert.Equal(t, "url", nav[0].args["type"])
		assert.Equal(t, baseURL+"/", nav[0].args["url"])
		assert.Equal(t, float64(2), nav[0].args["pageId"])
		init, _ := nav[0].args["initScript"].(string)
		assert.Contains(t, init, "window.__agentEval", "the observer is what runs before the page's own code")
		assert.Contains(t, init, "performance.timeOrigin", "and it uses the page's own clock")
	})
	t.Run("waits for the composer", func(t *testing.T) {
		p := chatPage()
		loading := state("1", func(m map[string]any) { m["login"], m["composer"] = false, false })
		// signing in looks three times (contract, done, contract); the start page then
		// shows nothing for two looks before the composer is there
		p.scripts["pageState"] = sequenceOf(state("1"), state("1", signedIn), state("1", signedIn), loading, loading, state("1", signedIn))
		d := p.driver(t, devtools.DriverConfig{PollInterval: time.Millisecond})
		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))
		looksBefore := len(p.called("evaluate_script"))

		require.NoError(t, d.StartChat(t.Context()))

		assert.Equal(t, 3, len(p.called("evaluate_script"))-looksBefore, "it looked until the composer was there, and no longer")
	})
	t.Run("a session that ended", func(t *testing.T) {
		p := chatPage()
		p.scripts["pageState"] = sequenceOf(state("1", signedIn), state("1", signedIn), state("1"))
		d := p.driver(t, devtools.DriverConfig{})
		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))

		err := d.StartChat(t.Context())

		// Signing in again is what a new browser session does, so the batch runner
		// starts one and tries the attempt once more, as it does for a dead browser.
		// It is not a refusal of the credentials, which are not at fault.
		require.ErrorIs(t, err, domain.ErrDriverDied)
		assert.NotErrorIs(t, err, domain.ErrLoginFailed)
		assert.ErrorContains(t, err, "the session ended")
	})
	t.Run("a navigation that fails", func(t *testing.T) {
		p := chatPage()
		p.tools["navigate_page"] = func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: net::ERR_ABORTED"), nil }
		d := p.driver(t, devtools.DriverConfig{})
		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))

		err := d.StartChat(t.Context())

		require.ErrorContains(t, err, "opening the start page")
	})
}

// What a driver does when its settings leave the waiting to it: a page gets
// fifteen seconds to show what it should, and is looked at four times a second.
func TestDriver_Defaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := chatPage()
		var mu sync.Mutex
		var looks []time.Time
		// The start page opens, and its composer never shows.
		p.scripts["pageState"] = func(map[string]any) (any, error) {
			mu.Lock()
			defer mu.Unlock()
			looks = append(looks, time.Now())
			return state("1", func(m map[string]any) { m["path"], m["login"] = "/", false }), nil
		}
		d := p.driver(t, devtools.DriverConfig{})
		start := time.Now()

		err := d.StartChat(t.Context())

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.ErrorContains(t, err, "waiting for the composer")
		assert.Equal(t, 15*time.Second, time.Since(start), "the page had fifteen seconds")
		mu.Lock()
		defer mu.Unlock()
		require.GreaterOrEqual(t, len(looks), 60)
		for i := 1; i < len(looks); i++ {
			require.Equal(t, 250*time.Millisecond, looks[i].Sub(looks[i-1]), "a quarter of a second between two looks")
		}
	})
}

func TestSubmit(t *testing.T) {
	t.Run("fills the composer and presses Enter", func(t *testing.T) {
		p := chatPage()
		d := p.driver(t, devtools.DriverConfig{})
		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))
		before := len(p.sequence())

		require.NoError(t, d.Submit(t.Context(), "What is 6 times 7?"))

		assert.Equal(t, []string{"eval:lastReply", "take_snapshot", "fill", "press_key"}, p.sequence()[before:],
			"it notes the newest reply first, so that the next one can be told from it")
		fills := p.called("fill")
		assert.Equal(t, map[string]any{"uid": "3_2", "value": "What is 6 times 7?", "pageId": float64(2)}, fills[len(fills)-1].args)
		assert.Equal(t, "Enter", p.called("press_key")[0].args["key"])
	})
	t.Run("a page without a composer", func(t *testing.T) {
		p := chatPage()
		d := p.driver(t, devtools.DriverConfig{})
		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))
		p.tools["take_snapshot"] = ok(snapshotOf(`uid=3_0 RootWebArea "Agent"`, `  uid=3_3 button "Send message"`))

		err := d.Submit(t.Context(), "hello")

		require.ErrorContains(t, err, `the composer: the page has no textbox named "Message"`)
	})
	t.Run("a fill or key press that fails", func(t *testing.T) {
		for _, tool := range []string{"fill", "press_key"} {
			p := chatPage()
			d := p.driver(t, devtools.DriverConfig{})
			require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))
			p.tools[tool] = func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: boom"), nil }

			err := d.Submit(t.Context(), "hello")

			require.Error(t, err, tool)
			assert.ErrorContains(t, err, "boom")
		}
	})
	t.Run("typing like a person", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := chatPage()
			d := p.driver(t, devtools.DriverConfig{HumanTyping: true, TypingDelay: 20 * time.Millisecond})
			require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))
			start := time.Now()
			before := len(p.sequence())

			require.NoError(t, d.Submit(t.Context(), "What is\n  the answer?"))

			seq := p.sequence()[before:]
			assert.Equal(t, []string{"eval:lastReply", "take_snapshot"}, seq[:2])
			assert.Equal(t, "click", seq[2], "it focuses the field first")
			assert.Equal(t, "press_key", seq[len(seq)-1])
			var typed strings.Builder
			for _, c := range p.called("type_text") {
				typed.WriteString(c.args["text"].(string))
				assert.NotContains(t, c.args, "submitKey")
			}
			assert.Equal(t, "What is  the answer?", typed.String(), "the pieces make up the text, with the line break pressed rather than typed")
			assert.Greater(t, len(p.called("type_text")), 2, "a word at a time")
			// every piece but the last is followed by a pause of the delay per character
			assert.Equal(t, 20*time.Millisecond*time.Duration(len("What is\n  the answer?")-len("answer?")), time.Since(start))
		})
	})
	// The composer sends on Enter and adds a line on Shift+Enter. The browser's
	// keyboard types "\n" as Enter, so a prompt of several lines typed in one go
	// would be sent at its first line and the rest would never reach the agent.
	t.Run("a line break is Shift+Enter, and only the last key is Enter", func(t *testing.T) {
		for name, text := range map[string]string{
			"a line break":           "first line\nsecond line",
			"a blank line":           "first\n\nsecond",
			"a break at the end":     "ends here\n",
			"a break at the start":   "\nstarts here",
			"a Windows line break":   "first\r\nsecond",
			"a lone carriage return": "first\rsecond",
		} {
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					p := chatPage()
					d := p.driver(t, devtools.DriverConfig{HumanTyping: true, TypingDelay: time.Millisecond})
					require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))
					p.calls = nil

					require.NoError(t, d.Submit(t.Context(), text))

					for _, c := range p.called("type_text") {
						assert.NotContains(t, c.args["text"], "\n", "no line break goes through type_text")
						assert.NotContains(t, c.args["text"], "\r")
					}
					var keys []string
					for _, c := range p.called("press_key") {
						keys = append(keys, c.args["key"].(string))
					}
					breaks := strings.Count(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text), "\n")
					want := append(slices.Repeat([]string{"Shift+Enter"}, breaks), "Enter")
					assert.Equal(t, want, keys, "one Shift+Enter for each line break, then Enter to send")
				})
			})
		}
	})
	t.Run("the typed pieces and the keys come in the order of the text", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := chatPage()
			d := p.driver(t, devtools.DriverConfig{HumanTyping: true, TypingDelay: time.Millisecond})
			require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))
			p.calls = nil

			require.NoError(t, d.Submit(t.Context(), "one\ntwo"))

			var order []string
			for _, c := range p.calls {
				switch c.tool {
				case "type_text":
					order = append(order, "type:"+c.args["text"].(string))
				case "press_key":
					order = append(order, "key:"+c.args["key"].(string))
				}
			}
			assert.Equal(t, []string{"type:one", "key:Shift+Enter", "type:two", "key:Enter"}, order)
		})
	})
	// A tab typed on a keyboard moves the focus out of the field. Text with one is
	// put in the field whole, so that the question reaches the agent as it is.
	t.Run("text with a tab is filled, not typed", func(t *testing.T) {
		p := chatPage()
		d := p.driver(t, devtools.DriverConfig{HumanTyping: true, TypingDelay: time.Millisecond})
		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))
		p.calls = nil

		require.NoError(t, d.Submit(t.Context(), "name\tvalue\nnext row"))

		assert.Empty(t, p.called("type_text"))
		require.Len(t, p.called("fill"), 1)
		assert.Equal(t, "name\tvalue\nnext row", p.called("fill")[0].args["value"])
		last := p.called("press_key")
		require.Len(t, last, 1)
		assert.Equal(t, "Enter", last[0].args["key"])
	})
	t.Run("typing fails midway", func(t *testing.T) {
		p := chatPage()
		d := p.driver(t, devtools.DriverConfig{HumanTyping: true})
		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))
		p.tools["type_text"] = func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: nothing has focus"), nil }

		err := d.Submit(t.Context(), "hello there")

		require.ErrorContains(t, err, "typing: ")
	})
}

// --- AwaitReply and Stop -------------------------------------------------------------

func replyState(status string, mutate ...func(map[string]any)) map[string]any {
	m := map[string]any{"found": true, "conversation": "c-1", "id": "m-1", "status": status, "chars": 10}
	for _, f := range mutate {
		f(m)
	}
	return m
}

func TestAwaitReply(t *testing.T) {
	notThere := map[string]any{"found": false, "conversation": nil}

	t.Run("polls until the reply reaches a final status", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := chatPage()
			p.scripts["lastReply"] = sequenceOf(notThere, replyState("pending"), replyState("streaming"), replyState("streaming"), replyState("complete"))
			d := p.driver(t, devtools.DriverConfig{PollInterval: 250 * time.Millisecond})
			start := time.Now()

			reply, err := d.AwaitReply(t.Context())

			require.NoError(t, err)
			assert.Equal(t, domain.Reply{ConversationID: "c-1", MessageID: "m-1", Status: domain.ReplyComplete}, reply)
			assert.Equal(t, time.Second, time.Since(start), "four pauses of a quarter second between five looks")
		})
	})
	t.Run("a failed reply carries the error bubble's text", func(t *testing.T) {
		p := chatPage()
		p.scripts["lastReply"] = fixed(replyState("failed", func(m map[string]any) { m["error"] = "The agent could not answer." }))
		d := p.driver(t, devtools.DriverConfig{})

		reply, err := d.AwaitReply(t.Context())

		require.NoError(t, err)
		assert.Equal(t, domain.ReplyFailed, reply.Status)
		assert.Equal(t, "The agent could not answer.", reply.Error)
	})
	t.Run("a stopped reply is final too", func(t *testing.T) {
		p := chatPage()
		p.scripts["lastReply"] = fixed(replyState("stopped"))
		d := p.driver(t, devtools.DriverConfig{})

		reply, err := d.AwaitReply(t.Context())

		require.NoError(t, err)
		assert.Equal(t, domain.ReplyStopped, reply.Status)
	})
	t.Run("time running out returns what is known", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := chatPage()
			p.scripts["lastReply"] = fixed(replyState("streaming"))
			d := p.driver(t, devtools.DriverConfig{PollInterval: 250 * time.Millisecond})
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()

			reply, err := d.AwaitReply(ctx)

			require.ErrorIs(t, err, domain.ErrReplyTimeout)
			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.Equal(t, domain.ReplyStreaming, reply.Status)
			assert.Equal(t, "c-1", reply.ConversationID)
		})
	})
	t.Run("time running out before the reply exists", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := chatPage()
			p.scripts["lastReply"] = fixed(notThere)
			d := p.driver(t, devtools.DriverConfig{})
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			reply, err := d.AwaitReply(ctx)

			require.ErrorIs(t, err, domain.ErrReplyTimeout)
			assert.Equal(t, domain.Reply{}, reply)
		})
	})
	t.Run("a script that fails a few times while the page swaps is tried again", func(t *testing.T) {
		p := chatPage()
		p.scripts["lastReply"] = sequenceOf(errors.New("Execution context was destroyed"), errors.New("Execution context was destroyed"), replyState("complete"))
		d := p.driver(t, devtools.DriverConfig{PollInterval: time.Millisecond})

		reply, err := d.AwaitReply(t.Context())

		require.NoError(t, err)
		assert.Equal(t, domain.ReplyComplete, reply.Status)
	})
	t.Run("but not for ever", func(t *testing.T) {
		p := chatPage()
		p.scripts["lastReply"] = fixed(nil)
		p.scripts["lastReply"] = func(map[string]any) (any, error) { return nil, errors.New("boom") }
		d := p.driver(t, devtools.DriverConfig{PollInterval: time.Millisecond})

		_, err := d.AwaitReply(t.Context())

		require.ErrorContains(t, err, "reading the reply failed 20 times in a row")
		assert.NotErrorIs(t, err, domain.ErrReplyTimeout)
	})
	t.Run("a browser that dies is not a timeout", func(t *testing.T) {
		p := chatPage()
		p.scripts["lastReply"] = fixed(replyState("streaming"))
		s, server := connectInMemory(t, p.serverTools(), devtools.Options{CallTimeout: 50 * time.Millisecond})
		d := devtools.NewDriver(s, devtools.DriverConfig{BaseURL: baseURL, PollInterval: time.Millisecond})
		require.NoError(t, server.Close())

		_, err := d.AwaitReply(t.Context())

		require.ErrorIs(t, err, domain.ErrDriverDied)
		assert.NotErrorIs(t, err, domain.ErrReplyTimeout)
	})
}

func TestAwaitReply_TellsTheNewReplyFromTheOldOne(t *testing.T) {
	old := replyState("complete", func(m map[string]any) { m["id"] = "m-1" })
	next := func(status string) map[string]any {
		return replyState(status, func(m map[string]any) { m["id"] = "m-2" })
	}

	t.Run("a second message in a conversation waits for its own reply", func(t *testing.T) {
		p := chatPage()
		// Submit looks once; the polls then see the old reply twice, then the new one.
		p.scripts["lastReply"] = sequenceOf(old, old, old, next("pending"), next("streaming"), next("complete"))
		d := p.driver(t, devtools.DriverConfig{PollInterval: time.Millisecond})
		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))
		require.NoError(t, d.Submit(t.Context(), "a follow-up"))

		reply, err := d.AwaitReply(t.Context())

		require.NoError(t, err)
		assert.Equal(t, "m-2", reply.MessageID, "the old reply was over, but it was not the answer")
		assert.Equal(t, domain.ReplyComplete, reply.Status)
		assert.Len(t, p.called("evaluate_script"), 3+1+5, "three to sign in, one to note the old reply, five polls: two of the old, three of the new")
	})
	t.Run("the first message of a conversation has nothing to be told from", func(t *testing.T) {
		p := chatPage()
		p.scripts["lastReply"] = sequenceOf(map[string]any{"found": false}, next("complete"))
		d := p.driver(t, devtools.DriverConfig{PollInterval: time.Millisecond})
		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))
		require.NoError(t, d.Submit(t.Context(), "hello"))

		reply, err := d.AwaitReply(t.Context())

		require.NoError(t, err)
		assert.Equal(t, "m-2", reply.MessageID)
	})
	t.Run("a new conversation forgets the last one", func(t *testing.T) {
		p := chatPage()
		p.scripts["lastReply"] = sequenceOf(old, old, next("complete"))
		d := p.driver(t, devtools.DriverConfig{PollInterval: time.Millisecond})
		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))
		require.NoError(t, d.Submit(t.Context(), "one")) // notes m-1
		require.NoError(t, d.StartChat(t.Context()))
		require.NoError(t, d.Submit(t.Context(), "two")) // the new page has no reply, but the script still shows m-1 once

		reply, err := d.AwaitReply(t.Context())

		require.NoError(t, err)
		assert.Equal(t, domain.ReplyComplete, reply.Status)
	})
	t.Run("a page that will not say is not a reason to stop", func(t *testing.T) {
		p := chatPage()
		p.scripts["lastReply"] = sequenceOf(errors.New("boom"), next("complete"))
		d := p.driver(t, devtools.DriverConfig{PollInterval: time.Millisecond})
		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))

		require.NoError(t, d.Submit(t.Context(), "hello"))

		reply, err := d.AwaitReply(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "m-2", reply.MessageID)
	})
}

func TestStop(t *testing.T) {
	t.Run("presses the Stop button", func(t *testing.T) {
		p := chatPage()
		p.tools["take_snapshot"] = ok(snapshotOf(`uid=3_0 RootWebArea "Agent"`, `  uid=3_4 button "Stop generating"`))
		d := p.driver(t, devtools.DriverConfig{})

		require.NoError(t, d.Stop(t.Context()))

		assert.Equal(t, "3_4", p.called("click")[0].args["uid"])
	})
	t.Run("there is none when the reply has finished", func(t *testing.T) {
		p := chatPage()
		d := p.driver(t, devtools.DriverConfig{})

		err := d.Stop(t.Context())

		require.ErrorIs(t, err, devtools.ErrNoStopButton)
		assert.Empty(t, p.called("click"))
	})
	t.Run("a click that finds the element gone is tried again with a new snapshot", func(t *testing.T) {
		p := chatPage()
		p.tools["take_snapshot"] = ok(snapshotOf(`uid=3_4 button "Stop generating"`))
		attempts := 0
		p.tools["click"] = func(map[string]any) (*mcp.CallToolResult, error) {
			if attempts++; attempts == 1 {
				return errorResult(`Error: Element uid "3_4" not found on page 2.`), nil
			}
			return textResult("Successfully clicked on the element"), nil
		}
		d := p.driver(t, devtools.DriverConfig{})

		require.NoError(t, d.Stop(t.Context()))

		assert.Equal(t, []string{"take_snapshot", "click", "take_snapshot", "click"}, p.sequence())
	})
	t.Run("twice is enough", func(t *testing.T) {
		p := chatPage()
		p.tools["take_snapshot"] = ok(snapshotOf(`uid=3_4 button "Stop generating"`))
		p.tools["click"] = func(map[string]any) (*mcp.CallToolResult, error) {
			return errorResult(`Error: Element uid "3_4" not found on page 2.`), nil
		}
		d := p.driver(t, devtools.DriverConfig{})

		err := d.Stop(t.Context())

		require.ErrorContains(t, err, "pressing Stop")
		assert.Len(t, p.called("click"), 2)
	})
}

// --- ReadRaw, Screenshot, DeleteChat --------------------------------------------------

func TestReadRaw(t *testing.T) {
	finished := domain.Reply{ConversationID: "c-1", MessageID: "m-1", Status: domain.ReplyComplete}
	withCopy := replyState("complete", func(m map[string]any) { m["rawUrl"] = "/c/c-1/m/m-1/raw" })

	t.Run("reads the reply from the address behind the Copy button", func(t *testing.T) {
		p := chatPage()
		p.scripts["lastReply"] = fixed(withCopy)
		p.scripts["fetchRaw"] = fixed(map[string]any{"status": 200, "text": "Thinking...\nFINAL ANSWER: 42"})
		d := p.driver(t, devtools.DriverConfig{})

		raw, err := d.ReadRaw(t.Context(), finished)

		require.NoError(t, err)
		assert.Equal(t, "Thinking...\nFINAL ANSWER: 42", raw)
		var script string
		for _, c := range p.called("evaluate_script") {
			if f := c.args["function"].(string); scriptName(f) == "fetchRaw" {
				script = f
			}
		}
		assert.Contains(t, script, `fetch("/c/c-1/m/m-1/raw"`, "the address is written in as a string literal")
		assert.NotContains(t, script, "__ARG__")
	})
	t.Run("an address with quotes is never put into a script", func(t *testing.T) {
		p := chatPage()
		p.scripts["lastReply"] = fixed(replyState("complete", func(m map[string]any) { m["rawUrl"] = `/x"); alert(1); ("` }))
		p.scripts["fetchRaw"] = fixed(map[string]any{"status": 200, "text": "x"})
		d := p.driver(t, devtools.DriverConfig{})

		_, err := d.ReadRaw(t.Context(), finished)

		require.ErrorContains(t, err, "not a path on the Web UI itself")
		for _, c := range p.called("evaluate_script") {
			assert.NotEqual(t, "fetchRaw", scriptName(c.args["function"].(string)), "nothing was fetched")
		}
	})
	t.Run("what can go wrong", func(t *testing.T) {
		tests := []struct {
			name    string
			last    any
			fetched any
			want    string
		}{
			{"no reply yet", map[string]any{"found": false}, nil, "no Copy button"},
			{"a reply without a Copy button", replyState("complete"), nil, "no Copy button"},
			{"another reply is newest", replyState("complete", func(m map[string]any) { m["id"] = "m-2"; m["rawUrl"] = "/c/c-1/m/m-2/raw" }), nil, "not the one asked for"},
			{"the endpoint says no", withCopy, map[string]any{"status": 409, "text": "not finished"}, "answered HTTP 409"},
			{"an address on another host", replyState("complete", func(m map[string]any) { m["rawUrl"] = "https://evil.example/raw" }), nil, "not a path on the Web UI itself"},
			{"a protocol-relative address", replyState("complete", func(m map[string]any) { m["rawUrl"] = "//evil.example/raw" }), nil, "not a path on the Web UI itself"},
			{"a relative address", replyState("complete", func(m map[string]any) { m["rawUrl"] = "raw" }), nil, "not a path on the Web UI itself"},
			{"a backslash that some browsers read as a slash", replyState("complete", func(m map[string]any) { m["rawUrl"] = `/\evil.example/raw` }), nil, "not a path on the Web UI itself"},
			// Chrome drops tabs and line breaks from an address before it reads it,
			// so "/<tab>/evil.example/raw" is "//evil.example/raw".
			{"a tab that makes it protocol-relative", replyState("complete", func(m map[string]any) { m["rawUrl"] = "/\t/evil.example/raw" }), nil, "not a path on the Web UI itself"},
			{"a line break that makes it protocol-relative", replyState("complete", func(m map[string]any) { m["rawUrl"] = "/\n/evil.example/raw" }), nil, "not a path on the Web UI itself"},
			{"another page of the Web UI", replyState("complete", func(m map[string]any) { m["rawUrl"] = "/logout" }), nil, "not a path on the Web UI itself"},
			{"the raw path with a query on it", replyState("complete", func(m map[string]any) { m["rawUrl"] = "/c/c-1/m/m-1/raw?next=//evil.example" }), nil, "not a path on the Web UI itself"},
			{"the raw path with a way up", replyState("complete", func(m map[string]any) { m["rawUrl"] = "/c/c-1/m/m-1/raw/../../../logout" }), nil, "not a path on the Web UI itself"},
			{"a path under another conversation's name with a slash", replyState("complete", func(m map[string]any) { m["rawUrl"] = "/c/a/b/m/m-1/raw" }), nil, "not a path on the Web UI itself"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				p := chatPage()
				p.scripts["lastReply"] = fixed(tc.last)
				p.scripts["fetchRaw"] = fixed(tc.fetched)
				d := p.driver(t, devtools.DriverConfig{})

				_, err := d.ReadRaw(t.Context(), finished)

				require.ErrorContains(t, err, tc.want)
			})
		}
	})
	t.Run("a script that throws", func(t *testing.T) {
		p := chatPage()
		p.scripts["lastReply"] = fixed(withCopy)
		p.scripts["fetchRaw"] = func(map[string]any) (any, error) { return nil, errors.New("Failed to fetch") }
		d := p.driver(t, devtools.DriverConfig{})

		_, err := d.ReadRaw(t.Context(), finished)

		require.ErrorContains(t, err, "fetchRaw: ")
		assert.ErrorContains(t, err, "Failed to fetch")
	})
}

func TestScreenshot(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}

	t.Run("saves the picture the browser returns", func(t *testing.T) {
		p := chatPage()
		p.tools["take_screenshot"] = func(map[string]any) (*mcp.CallToolResult, error) {
			res := textResult("Took a screenshot of the current page's viewport.")
			res.Content = append(res.Content, &mcp.ImageContent{MIMEType: "image/png", Data: png})
			return res, nil
		}
		d := p.driver(t, devtools.DriverConfig{})
		path := filepath.Join(t.TempDir(), "shot.png")

		require.NoError(t, d.Screenshot(t.Context(), path))

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, png, got)
		info, _ := os.Stat(path)
		assert.Zero(t, info.Mode().Perm()&0o077, "a screenshot can show what was typed; it is private")
		assert.Equal(t, "png", p.called("take_screenshot")[0].args["format"])
		assert.NotContains(t, p.called("take_screenshot")[0].args, "filePath", "the picture comes back inline, so the browser needs no access to the disk")
	})
	t.Run("no picture", func(t *testing.T) {
		p := chatPage()
		p.tools["take_screenshot"] = ok("Took a screenshot")
		d := p.driver(t, devtools.DriverConfig{})

		err := d.Screenshot(t.Context(), filepath.Join(t.TempDir(), "x.png"))

		require.ErrorContains(t, err, "no picture")
	})
	t.Run("nowhere to write", func(t *testing.T) {
		p := chatPage()
		p.tools["take_screenshot"] = func(map[string]any) (*mcp.CallToolResult, error) {
			res := textResult("ok")
			res.Content = append(res.Content, &mcp.ImageContent{MIMEType: "image/png", Data: png})
			return res, nil
		}
		d := p.driver(t, devtools.DriverConfig{})

		err := d.Screenshot(t.Context(), filepath.Join(t.TempDir(), "missing", "x.png"))

		require.Error(t, err)
	})
	t.Run("the browser refuses", func(t *testing.T) {
		p := chatPage()
		p.tools["take_screenshot"] = func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: page closed"), nil }
		d := p.driver(t, devtools.DriverConfig{})

		require.ErrorContains(t, d.Screenshot(t.Context(), filepath.Join(t.TempDir(), "x.png")), "page closed")
	})
}

func TestDeleteChat(t *testing.T) {
	reply := replyState("complete")
	// afterReply makes a driver that has seen a reply, so it knows its conversation.
	afterReply := func(t *testing.T, p *page) *devtools.Driver {
		t.Helper()
		p.scripts["lastReply"] = fixed(reply)
		d := p.driver(t, devtools.DriverConfig{PollInterval: time.Millisecond})
		_, err := d.AwaitReply(t.Context())
		require.NoError(t, err)
		return d
	}
	sidebar := snapshotOf(`uid=4_0 RootWebArea "Agent"`, `  uid=4_1 button "Delete conversation What is 6 times 7?"`)
	dialog := snapshotOf(`uid=4_0 RootWebArea "Agent"`, `  uid=4_9 dialog "Delete this conversation?"`, `    uid=4_10 button "Cancel"`, `    uid=4_11 button "Delete"`)

	t.Run("deletes through the sidebar and the confirmation", func(t *testing.T) {
		p := chatPage()
		snapshots := []string{sidebar, dialog}
		p.tools["take_snapshot"] = func(map[string]any) (*mcp.CallToolResult, error) {
			s := snapshots[0]
			if len(snapshots) > 1 {
				snapshots = snapshots[1:]
			}
			return textResult(s), nil
		}
		p.scripts["rowButton"] = fixed(map[string]any{"name": "Delete conversation What is 6 times 7?", "before": 0})
		p.scripts["rowExists"] = sequenceOf(true, false)
		d := afterReply(t, p)

		require.NoError(t, d.DeleteChat(t.Context()))

		clicks := p.called("click")
		require.Len(t, clicks, 2)
		assert.Equal(t, "4_1", clicks[0].args["uid"], "the row's own delete button")
		assert.Equal(t, "4_11", clicks[1].args["uid"], "then Delete in the dialog, not Cancel")
		for _, c := range p.called("evaluate_script") {
			if f := c.args["function"].(string); scriptName(f) == "rowButton" || scriptName(f) == "rowExists" {
				assert.Contains(t, f, `"c-1"`, "asked about this conversation")
			}
		}
	})
	t.Run("rows with the same title: its own button is pressed, not the first with that name", func(t *testing.T) {
		// Every attempt of an evaluation opens with the same prompt, so every row
		// has the same title, and the buttons of all of them have the same name.
		name := `button "Delete conversation You are a general AI assistant. I will ask you"`
		crowded := snapshotOf(`uid=4_0 RootWebArea "Agent"`, `  uid=4_1 `+name, `  uid=4_2 `+name, `  uid=4_3 `+name)
		p := chatPage()
		snapshots := []string{crowded, dialog}
		p.tools["take_snapshot"] = func(map[string]any) (*mcp.CallToolResult, error) {
			s := snapshots[0]
			if len(snapshots) > 1 {
				snapshots = snapshots[1:]
			}
			return textResult(s), nil
		}
		// Two rows come before this conversation's, and have the same name.
		p.scripts["rowButton"] = fixed(map[string]any{"name": "Delete conversation You are a general AI assistant. I will ask you", "before": 2})
		p.scripts["rowExists"] = sequenceOf(true, false)
		d := afterReply(t, p)

		require.NoError(t, d.DeleteChat(t.Context()))

		clicks := p.called("click")
		require.Len(t, clicks, 2)
		assert.Equal(t, "4_3", clicks[0].args["uid"], "the third of three buttons with that name, because that is this conversation's row")
	})
	t.Run("fewer buttons with that name than the page says come before", func(t *testing.T) {
		p := chatPage()
		p.tools["take_snapshot"] = ok(sidebar)
		p.scripts["rowButton"] = fixed(map[string]any{"name": "Delete conversation What is 6 times 7?", "before": 3})
		d := afterReply(t, p)

		err := d.DeleteChat(t.Context())

		require.Error(t, err)
		assert.ErrorContains(t, err, "pressing the delete button")
		assert.Empty(t, p.called("click"), "nothing is pressed when the right button can't be found")
	})
	t.Run("a conversation that was never started", func(t *testing.T) {
		d := chatPage().driver(t, devtools.DriverConfig{})

		require.ErrorContains(t, d.DeleteChat(t.Context()), "no conversation to delete")
	})
	t.Run("a sidebar without the row", func(t *testing.T) {
		p := chatPage()
		p.scripts["rowButton"] = fixed(nil)
		d := afterReply(t, p)

		require.ErrorContains(t, d.DeleteChat(t.Context()), "the sidebar has no row for conversation c-1")
	})
	t.Run("a dialog that never opens", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := chatPage()
			p.tools["take_snapshot"] = ok(sidebar)
			p.scripts["rowButton"] = fixed(map[string]any{"name": "Delete conversation What is 6 times 7?", "before": 0})
			p.scripts["lastReply"] = fixed(reply)
			d := p.driver(t, devtools.DriverConfig{PollInterval: 250 * time.Millisecond, SettleTimeout: 2 * time.Second})
			_, err := d.AwaitReply(t.Context())
			require.NoError(t, err)

			err = d.DeleteChat(t.Context())

			require.Error(t, err)
			assert.ErrorContains(t, err, "delete confirmation")
			assert.ErrorIs(t, err, context.DeadlineExceeded)
		})
	})
	t.Run("a row that stays", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			p := chatPage()
			p.tools["take_snapshot"] = ok(dialog + `  uid=4_1 button "Delete conversation What is 6 times 7?"` + "\n")
			p.scripts["rowButton"] = fixed(map[string]any{"name": "Delete conversation What is 6 times 7?", "before": 0})
			p.scripts["rowExists"] = fixed(true)
			p.scripts["lastReply"] = fixed(reply)
			d := p.driver(t, devtools.DriverConfig{PollInterval: 250 * time.Millisecond, SettleTimeout: 2 * time.Second})
			_, err := d.AwaitReply(t.Context())
			require.NoError(t, err)

			err = d.DeleteChat(t.Context())

			require.Error(t, err)
			assert.ErrorContains(t, err, "conversation")
			assert.ErrorIs(t, err, context.DeadlineExceeded)
		})
	})
}

func TestDriverClose(t *testing.T) {
	p := chatPage()
	d := p.driver(t, devtools.DriverConfig{})

	require.NoError(t, d.Close(t.Context()))

	_, err := d.Session().Call(t.Context(), "take_snapshot", nil)
	require.ErrorIs(t, err, domain.ErrDriverDied, "the session is closed")
}

func TestScriptsAllExist(t *testing.T) {
	for _, name := range []string{"pageState", "lastReply", "fetchRaw", "rowButton", "rowExists", "collect", "observer", "userAgent"} {
		t.Run(name, func(t *testing.T) {
			assert.NotPanics(t, func() { _ = devtools.ScriptForTest(name) })
			src := devtools.ScriptForTest(name)
			if name != "observer" {
				assert.True(t, strings.HasPrefix(src, "// eval:"+name+"\n"), "the first line names the script")
			}
		})
	}
	assert.Equal(t, "pageState", devtools.ScriptMarkerForTest(devtools.ScriptForTest("pageState")))
	assert.Equal(t, "() => 1", devtools.ScriptMarkerForTest("() => 1"), "an unnamed function is known by its first line")
	assert.Panics(t, func() { _ = devtools.ScriptForTest("nope") })
}

// What counts as the text of a reply is one rule, in one file, and every script
// that counts it gets that file, so that no script counts by a copy of its own.
func TestScripts_ShareTheRuleForVisibleText(t *testing.T) {
	for _, name := range []string{"observer", "lastReply", "replies"} {
		t.Run(name, func(t *testing.T) {
			src := devtools.ScriptForTest(name)

			assert.Equal(t, 1, strings.Count(src, "const visibleText = (body) =>"), "the rule is in the script, once")
			assert.NotContains(t, src, "include:", "and the line that asked for it is gone")
		})
	}
	for _, name := range []string{"pageState", "lastReply", "fetchRaw", "rowButton", "rowExists", "collect", "replies", "observer", "userAgent"} {
		assert.NotContains(t, devtools.ScriptForTest(name), "include:", name)
	}
	assert.Panics(t, func() { _ = devtools.ScriptForTest("visibleText") }, "the rule alone is not a script")
}

func TestFactory(t *testing.T) {
	f := &devtools.Factory{Session: processOptions(t), Driver: devtools.DriverConfig{BaseURL: baseURL}}
	assert.Empty(t, f.Versions(), "nothing is known before a browser starts")

	d, err := f.NewDriver(t.Context())

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"chrome-devtools-mcp": "1.10.1"}, f.Versions(), "the server says which version it is")
	require.NoError(t, d.Close(t.Context()))

	t.Run("a browser that will not start", func(t *testing.T) {
		bad := &devtools.Factory{Session: processOptions(t, "exit-at-start")}

		_, err := bad.NewDriver(t.Context())

		require.ErrorContains(t, err, "starting Chrome DevTools MCP")
	})
}

func TestLogin_ReportsTheBrowserVersion(t *testing.T) {
	signIn := func(p *page) {
		p.scripts["pageState"] = sequenceOf(state("1"), state("1", signedIn), state("1", signedIn))
	}
	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/143.0.7499.0 Safari/537.36"

	t.Run("after signing in", func(t *testing.T) {
		p := loginPage()
		signIn(p)
		p.scripts["userAgent"] = fixed(ua)
		d := p.driver(t, devtools.DriverConfig{PollInterval: time.Millisecond})
		reported := map[string]string{}
		d.SetReportForTest(func(name, version string) { reported[name] = version })

		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))

		assert.Equal(t, map[string]string{"chrome": "143.0.7499.0"}, reported)
	})
	t.Run("a browser that will not say is no reason to stop", func(t *testing.T) {
		p := loginPage()
		signIn(p)
		p.scripts["userAgent"] = func(map[string]any) (any, error) { return nil, errors.New("boom") }
		d := p.driver(t, devtools.DriverConfig{PollInterval: time.Millisecond})
		called := false
		d.SetReportForTest(func(string, string) { called = true })

		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))

		assert.False(t, called)
	})
	t.Run("a user agent that holds no version", func(t *testing.T) {
		p := loginPage()
		signIn(p)
		p.scripts["userAgent"] = fixed("curl/8.0")
		d := p.driver(t, devtools.DriverConfig{PollInterval: time.Millisecond})
		called := false
		d.SetReportForTest(func(string, string) { called = true })

		require.NoError(t, d.Login(t.Context(), "a@b.c", "pw"))

		assert.False(t, called)
	})
}
