package devtools_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
)

// sizes lists the window sizes a scene was given, in order.
func (s *scene) sizes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.viewports)
}

// --- P-03 -------------------------------------------------------------------------------------

func TestProbe_Lighthouse(t *testing.T) {
	// scores gives the accessibility and best-practices scores for each device.
	audit := func(scores map[string][2]int) func(map[string]any) (*mcp.CallToolResult, error) {
		return func(args map[string]any) (*mcp.CallToolResult, error) {
			device, _ := args["device"].(string)
			both, ok := scores[device]
			if !ok {
				return errorResult("Error: " + errAudit.Error()), nil
			}
			return textResult(lighthouseText(both[0], both[1])), nil
		}
	}
	tests := []struct {
		name    string
		audit   func(map[string]any) (*mcp.CallToolResult, error)
		want    domain.CheckResult
		detail  string
		wantErr string
		is      error
	}{
		{
			name: "all four scores are enough", want: domain.Pass,
			audit:  audit(map[string][2]int{"desktop": {95, 100}, "mobile": {92, 96}}),
			detail: "desktop accessibility 95, desktop best practices 100, mobile accessibility 92, mobile best practices 96",
		},
		{
			name: "a score on the phone is too low", want: domain.Fail,
			audit:  audit(map[string][2]int{"desktop": {95, 100}, "mobile": {80, 96}}),
			detail: "below 90: mobile accessibility 80",
		},
		{
			name: "the audit fails", wantErr: "auditing for desktop",
			audit: audit(map[string][2]int{}),
		},
		{
			name: "the audit fails on the phone", wantErr: "auditing for mobile",
			audit: audit(map[string][2]int{"desktop": {95, 100}}),
		},
		{
			name: "the audit prints no scores", is: devtools.ErrNoLighthouseScores,
			audit: func(map[string]any) (*mcp.CallToolResult, error) {
				return textResult("## Lighthouse Audit Results\nMode: navigation\n"), nil
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene()
			var got domain.ProbeOutcome
			var err error
			var r *rig
			synctest.Test(t, func(t *testing.T) {
				r = newRig(t, s)
				r.page.tools["lighthouse_audit"] = tc.audit
				got, err = r.probes.Lighthouse(t.Context())
			})

			switch {
			case tc.wantErr != "":
				require.ErrorContains(t, err, tc.wantErr)
			case tc.is != nil:
				require.ErrorIs(t, err, tc.is)
			default:
				require.NoError(t, err)
				assert.Equal(t, domain.P03, got.ID)
				assert.Equal(t, tc.want, got.Result, got.Detail)
				assert.Contains(t, got.Detail, tc.detail)
			}
		})
	}

	t.Run("it audits the page with a conversation on it, on a desktop and then on a phone", func(t *testing.T) {
		s := newScene()
		var r *rig
		synctest.Test(t, func(t *testing.T) {
			r = newRig(t, s)
			r.page.tools["lighthouse_audit"] = audit(map[string][2]int{"desktop": {95, 100}, "mobile": {92, 96}})
			_, err := r.probes.Lighthouse(t.Context())
			require.NoError(t, err)
		})

		audits := r.page.called("lighthouse_audit")
		require.Len(t, audits, 2)
		assert.Equal(t, "desktop", audits[0].args["device"])
		assert.Equal(t, "mobile", audits[1].args["device"])
		for _, a := range audits {
			assert.Equal(t, "navigation", a.args["mode"], "of the page as it loads, with the observer's pages in it")
		}
		assert.Len(t, s.titles(), 1, "a conversation was started to have something on the page")
		assert.Equal(t, []string{devtools.DefaultViewport}, s.sizes(), "the window is put back to the size it was after the audit changed it")
	})
	t.Run("a window that cannot be put back is not a finding", func(t *testing.T) {
		s := newScene()
		var got domain.ProbeOutcome
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, s)
			r.page.tools["lighthouse_audit"] = audit(map[string][2]int{"desktop": {95, 100}, "mobile": {92, 96}})
			r.page.tools["emulate"] = func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: gone"), nil }
			var err error
			got, err = r.probes.Lighthouse(t.Context())
			require.NoError(t, err)
		})

		assert.Equal(t, domain.Pass, got.Result)
	})
}

// --- P-04 -------------------------------------------------------------------------------------

func TestProbe_Mobile(t *testing.T) {
	tests := []struct {
		name   string
		flaws  flaws
		want   domain.CheckResult
		detail string
	}{
		{name: "it fits, and the conversations can be opened", want: domain.Pass, detail: "no sideways scrolling at 375 px, conversations reachable"},
		{name: "the page spills sideways", flaws: flaws{overflowPx: 24}, want: domain.Fail, detail: "the page is 24 px wider than the window"},
		{name: "nothing opens the conversations", flaws: flaws{noMenuButton: true}, want: domain.Fail, detail: "the conversations cannot be opened"},
		{name: "the button does not open them", flaws: flaws{menuStaysClosed: true}, want: domain.Fail, detail: "the conversations cannot be opened"},
		{name: "the message field is out of the window", flaws: flaws{composerOffScreen: true}, want: domain.Fail, detail: "the message field is not on screen"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(withFlaws(tc.flaws))

			got, _ := outcomeOf(t, s, (*devtools.Probes).Mobile)

			assert.Equal(t, domain.P04, got.ID)
			assert.Equal(t, tc.want, got.Result, got.Detail)
			assert.Contains(t, got.Detail, tc.detail)
			assert.Equal(t, []string{"375x667x2,mobile,touch", devtools.DefaultViewport}, s.sizes(), "the window is the size of a phone's, and then put back")
		})
	}

	t.Run("a page that will not open still leaves the window as it was", func(t *testing.T) {
		s := newScene(func(s *scene) {
			s.opened = func(*scene, string) error { return errors.New("net::ERR_CONNECTION_REFUSED") }
		})
		var err error
		synctest.Test(t, func(t *testing.T) {
			_, err = newRig(t, s).probes.Mobile(t.Context())
		})

		require.ErrorContains(t, err, "opening the start page")
		assert.Equal(t, []string{"375x667x2,mobile,touch", devtools.DefaultViewport}, s.sizes())
	})
	t.Run("a button that cannot be pressed is a conversation list that cannot be opened", func(t *testing.T) {
		s := newScene()
		var got domain.ProbeOutcome
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, s)
			r.page.tools["click"] = func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: not clickable"), nil }
			var err error
			got, err = r.probes.Mobile(t.Context())
			require.NoError(t, err)
		})

		assert.Equal(t, domain.Fail, got.Result)
		assert.Contains(t, got.Detail, "the conversations cannot be opened")
	})
	t.Run("a window that will not take the size of a phone is an error", func(t *testing.T) {
		s := newScene()
		var err error
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, s)
			r.page.tools["emulate"] = func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: no"), nil }
			_, err = r.probes.Mobile(t.Context())
		})

		require.ErrorContains(t, err, "setting the size of the browser window")
	})
}

// --- P-06 -------------------------------------------------------------------------------------

func TestProbe_Theme(t *testing.T) {
	tests := []struct {
		name       string
		flaws      flaws
		want       domain.CheckResult
		detail     string
		leftAs     string // the theme the page was left in
		wantClicks int
	}{
		{name: "it switches and the choice is kept", want: domain.Pass, detail: "the theme switches and the choice survives a reload", leftAs: "auto", wantClicks: 3},
		{name: "the switch does nothing", flaws: flaws{themeStuck: true}, want: domain.Fail, detail: "did not change theme", leftAs: "auto", wantClicks: 3},
		{name: "the choice is not kept", flaws: flaws{themeForgotten: true}, want: domain.Fail, detail: "went back to the old one", leftAs: "auto", wantClicks: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(withFlaws(tc.flaws))

			got, r := outcomeOf(t, s, (*devtools.Probes).Theme)

			assert.Equal(t, domain.P06, got.ID)
			assert.Equal(t, tc.want, got.Result, got.Detail)
			assert.Contains(t, got.Detail, tc.detail)
			assert.Equal(t, tc.leftAs, s.theme, "the page is left in the theme it was found in: the switch goes round auto, light, dark")
			assert.Len(t, r.page.called("click"), tc.wantClicks, "once to see it work, and twice to go round")
		})
	}

	t.Run("it gives up putting the theme back when the switch is gone", func(t *testing.T) {
		s := newScene()
		var r *rig
		synctest.Test(t, func(t *testing.T) {
			r = newRig(t, s)
			pressed := r.page.tools["click"]
			r.page.tools["click"] = func(args map[string]any) (*mcp.CallToolResult, error) {
				if len(r.page.called("click")) > 1 {
					return errorResult("Error: gone"), nil
				}
				return pressed(args)
			}
			got, err := r.probes.Theme(t.Context())
			require.NoError(t, err)
			assert.Equal(t, domain.Pass, got.Result, "what it found does not depend on the clean-up")
		})

		assert.Len(t, r.page.called("click"), 2, "the first press that fails ends it")
	})
}

// --- P-08 -------------------------------------------------------------------------------------

func TestProbe_KeyboardOnly(t *testing.T) {
	tests := []struct {
		name   string
		flaws  flaws
		want   domain.CheckResult
		cannot string // what the detail says could not be done
		can    []string
	}{
		{name: "send, stop and open a conversation", want: domain.Pass},
		{
			name: "the focus is not in the message field", flaws: flaws{composerNotFocused: true}, want: domain.Fail,
			cannot: "send a message, stop a reply, open a conversation",
		},
		{
			name: "Stop cannot be reached with Tab", flaws: flaws{stopNotFocusable: true}, want: domain.Fail,
			cannot: "stop a reply", can: []string{"send a message", "open a conversation"},
		},
		{
			name: "Enter on Stop does not stop the reply", flaws: flaws{stopIgnored: true}, want: domain.Fail,
			cannot: "stop a reply", can: []string{"send a message", "open a conversation"},
		},
		{
			name: "Enter on a conversation does not open it", flaws: flaws{linkInert: true}, want: domain.Fail,
			cannot: "open a conversation", can: []string{"send a message", "stop a reply"},
		},
		{
			name: "the conversations are not links", flaws: flaws{linkWithoutHref: true}, want: domain.Fail,
			cannot: "open a conversation", can: []string{"send a message", "stop a reply"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(withFlaws(tc.flaws))

			got, r := outcomeOf(t, s, (*devtools.Probes).KeyboardOnly)

			assert.Equal(t, domain.P08, got.ID)
			assert.Equal(t, tc.want, got.Result, got.Detail)
			if tc.want == domain.Pass {
				assert.Contains(t, got.Detail, "send, stop and navigate with the keyboard alone")
				return
			}
			if tc.flaws.composerNotFocused {
				assert.Empty(t, r.page.called("type_text"), "nothing is typed where the message would not go")
			}
			assert.Contains(t, got.Detail, "it was not possible to "+tc.cannot)
			for _, did := range tc.can {
				assert.NotContains(t, got.Detail, did)
			}
		})
	}

	t.Run("it uses the keyboard and nothing else to do it", func(t *testing.T) {
		s := newScene()
		_, r := outcomeOf(t, s, (*devtools.Probes).KeyboardOnly)

		assert.Empty(t, r.page.called("click"), "no mouse")
		assert.Empty(t, r.page.called("fill"), "and the words are typed, not put in the field")
		typed := r.page.called("type_text")
		require.Len(t, typed, 2)
		assert.Equal(t, shortPrompt, typed[0].args["text"])
		assert.Equal(t, "Enter", typed[0].args["submitKey"])
		assert.Equal(t, longPrompt, typed[1].args["text"])
		assert.Equal(t, "Enter", typed[1].args["submitKey"])
	})
	t.Run("it walks to Stop forwards and back to a conversation backwards", func(t *testing.T) {
		s := newScene()
		_, r := outcomeOf(t, s, (*devtools.Probes).KeyboardOnly)

		var keys []string
		for _, c := range r.page.called("press_key") {
			keys = append(keys, c.args["key"].(string))
		}
		firstBack := slices.Index(keys, "Shift+Tab")
		require.GreaterOrEqual(t, firstBack, 0)
		assert.Contains(t, keys[:firstBack], "Tab")
		assert.NotContains(t, keys[:firstBack], "Shift+Tab")
		assert.Equal(t, "Enter", keys[firstBack-1], "Stop is pressed before the walk back")
		assert.Equal(t, "Enter", keys[len(keys)-1], "and the conversation is opened with Enter")
	})
	t.Run("the walk to a control that cannot be reached is bounded", func(t *testing.T) {
		// A page the focus cannot get to Stop on must not keep the probe pressing Tab
		// for ever: forty presses, and it gives up.
		s := newScene(withFlaws(flaws{stopNotFocusable: true}))
		_, r := outcomeOf(t, s, (*devtools.Probes).KeyboardOnly)

		tabs := 0
		for _, c := range r.page.called("press_key") {
			if c.args["key"] == "Tab" {
				tabs++
			}
		}
		assert.Equal(t, 40, tabs)
	})
}

// --- P-09 -------------------------------------------------------------------------------------

func TestProbe_LayoutShift(t *testing.T) {
	trace := func(text string) func(map[string]any) (*mcp.CallToolResult, error) {
		return func(map[string]any) (*mcp.CallToolResult, error) { return textResult(text), nil }
	}
	tests := []struct {
		name    string
		stop    func(map[string]any) (*mcp.CallToolResult, error)
		start   func(map[string]any) (*mcp.CallToolResult, error)
		scene   []func(*scene)
		want    domain.CheckResult
		detail  string
		wantErr string
		is      error
		stopped int // how many times the trace was stopped
	}{
		{name: "nothing moves", stop: trace(traceText("0")), want: domain.Pass, detail: "cumulative layout shift 0.000", stopped: 1},
		{name: "a little moves", stop: trace(traceText("0.08")), want: domain.Pass, detail: "cumulative layout shift 0.080", stopped: 1},
		{name: "too much moves", stop: trace(traceText("0.31")), want: domain.Fail, detail: "cumulative layout shift 0.310 is over the 0.100 limit", stopped: 1},
		{name: "the summary has no figure", stop: trace("The performance trace has been stopped.\n"), is: devtools.ErrNoCLS, stopped: 1},
		{
			name: "the trace cannot be stopped", wantErr: "stopping the trace", stopped: 1,
			stop: func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: no trace"), nil },
		},
		{
			name: "the trace cannot be started", wantErr: "starting the trace", stopped: 0, stop: trace(traceText("0")),
			start: func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: busy"), nil },
		},
		{
			// the reply that never ends, so that the probe gives up waiting
			name: "the reply never ends", wantErr: "waiting for the reply", stopped: 1, stop: trace(traceText("0")),
			scene: []func(*scene){func(s *scene) {
				s.agent = func(string, int) sceneReply { return sceneReply{stages: endless} }
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(tc.scene...)
			var got domain.ProbeOutcome
			var err error
			var r *rig
			synctest.Test(t, func(t *testing.T) {
				r = newRig(t, s)
				r.page.tools["performance_start_trace"] = cmpStart(tc.start)
				r.page.tools["performance_stop_trace"] = tc.stop
				got, err = r.probes.LayoutShift(t.Context())
			})

			switch {
			case tc.wantErr != "":
				require.ErrorContains(t, err, tc.wantErr)
			case tc.is != nil:
				require.ErrorIs(t, err, tc.is)
			default:
				require.NoError(t, err)
				assert.Equal(t, domain.P09, got.ID)
				assert.Equal(t, tc.want, got.Result, got.Detail)
				assert.Contains(t, got.Detail, tc.detail)
			}
			assert.Len(t, r.page.called("performance_stop_trace"), tc.stopped, "a trace that was started is stopped, once, whatever happened")
			if tc.start == nil {
				started := r.page.called("performance_start_trace")
				require.Len(t, started, 1)
				assert.Equal(t, false, started[0].args["reload"], "the page is not loaded again for the trace")
				assert.Equal(t, false, started[0].args["autoStop"], "it runs until the reply is in")
			}
		})
	}

	t.Run("the trace is on while the long reply is written", func(t *testing.T) {
		s := newScene()
		var r *rig
		synctest.Test(t, func(t *testing.T) {
			r = newRig(t, s)
			r.page.tools["performance_start_trace"] = ok("started")
			r.page.tools["performance_stop_trace"] = trace(traceText("0"))
			_, err := r.probes.LayoutShift(t.Context())
			require.NoError(t, err)
		})

		seq := r.page.sequence()
		start := slices.Index(seq, "performance_start_trace")
		stop := slices.Index(seq, "performance_stop_trace")
		fill := slices.Index(seq, "fill")
		require.GreaterOrEqual(t, start, 0)
		assert.Less(t, start, fill, "started before the message is sent")
		assert.Less(t, fill, stop, "and stopped after")
		assert.Equal(t, longPrompt, r.page.called("fill")[0].args["value"])
	})
}

// cmpStart is the handler of the tool that starts a trace: the test's, or one that works.
func cmpStart(f func(map[string]any) (*mcp.CallToolResult, error)) func(map[string]any) (*mcp.CallToolResult, error) {
	if f != nil {
		return f
	}
	return func(map[string]any) (*mcp.CallToolResult, error) {
		return textResult("The performance trace is running."), nil
	}
}

// --- the browser opens on first use ---------------------------------------------------------------

func TestProbes_OpenTheBrowserOnFirstUse(t *testing.T) {
	// The tests above give the probes a tab that is open. These start the
	// browser, as a run does, with a stand-in for the server that has no page
	// to sign in to.
	t.Run("a browser that will not start is an error of the probe", func(t *testing.T) {
		p := &devtools.Probes{
			Factory: &devtools.Factory{Session: processOptions(t, "exit-at-start"), Driver: devtools.DriverConfig{BaseURL: baseURL}},
			Email:   "a@b.c", Password: "pw",
		}

		_, err := p.Theme(t.Context())

		require.ErrorContains(t, err, "starting Chrome DevTools MCP")
	})
	t.Run("a page that cannot be signed in to is an error, and the next probe tries again", func(t *testing.T) {
		p := &devtools.Probes{
			Factory: &devtools.Factory{Session: processOptions(t), Driver: devtools.DriverConfig{BaseURL: baseURL}},
			Email:   "a@b.c", Password: "pw",
		}

		_, first := p.Theme(t.Context())
		_, second := p.Mobile(t.Context())

		require.ErrorContains(t, first, "opening the Web UI")
		require.ErrorContains(t, second, "opening the Web UI", "a browser that failed to sign in was not kept")
		require.NoError(t, p.Close(t.Context()), "and there is none to close")
	})
}

// --- the browser opens once --------------------------------------------------------------------

func TestProbes_Close(t *testing.T) {
	t.Run("closes the session, once", func(t *testing.T) {
		s := newScene()
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, s)

			require.NoError(t, r.probes.Close(context.WithoutCancel(t.Context())))
			require.NoError(t, r.probes.Close(context.WithoutCancel(t.Context())), "and again, with nothing to close")
		})
	})
	t.Run("a probe never run has nothing to close", func(t *testing.T) {
		require.NoError(t, (&devtools.Probes{}).Close(t.Context()))
	})
}
