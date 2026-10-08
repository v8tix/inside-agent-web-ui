package devtools_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
)

// The probes are judged here on what they conclude from a page, in a scene. Each
// runs against a page that is right and against pages that are wrong in the one
// way that probe looks for; the verdicts themselves are tested in the probe
// package.

// outcomeOf runs a probe in a bubble of virtual time, so that the minute it may
// wait for a reply costs nothing.
func outcomeOf(t *testing.T, s *scene, probe func(*devtools.Probes, context.Context) (domain.ProbeOutcome, error)) (out domain.ProbeOutcome, r *rig) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		r = newRig(t, s)
		var err error
		out, err = probe(r.probes, t.Context())
		require.NoError(t, err)
	})
	return out, r
}

// --- P-01 -------------------------------------------------------------------------------------

// pageClock is when Enter was pressed, on the clock of the page.
const pageClock = 1700000000000.0

// stopRecord is what the observer recorded of a reply that was stopped: Stop was
// clicked at stopAt (ms after pageClock) and the page showed it stopped at stoppedAt.
func stopRecord(stopAt, stoppedAt float64, updates ...[2]float64) map[string]any {
	var shown []map[string]any
	for _, u := range updates {
		shown = append(shown, map[string]any{"at": pageClock + u[0], "chars": int(u[1])})
	}
	return recorded(func(m map[string]any) {
		m["stopAt"] = pageClock + stopAt
		m["updates"] = shown
		m["statuses"] = []map[string]any{
			{"at": pageClock + stopAt - 300, "status": "pending"},
			{"at": pageClock + stopAt - 200, "status": "streaming"},
			{"at": pageClock + stoppedAt, "status": "stopped"},
		}
	})
}

// kept makes a scene whose server keeps the text a reply has when it is stopped.
func kept(text func(shown int) string) func(*scene) {
	return func(s *scene) { s.keepOnStop = text }
}

func TestProbe_Stop(t *testing.T) {
	// 20 characters at 100 ms, 40 at 200 ms, Stop clicked at 300, 90 at 350.
	healthy := func(stoppedAt float64) any {
		return stopRecord(300, stoppedAt, [2]float64{100, 20}, [2]float64{200, 40}, [2]float64{350, 90})
	}
	tests := []struct {
		name       string
		scene      []func(*scene)
		observed   any
		want       domain.CheckResult
		detail     string
		measures   map[string]float64
		notPressed bool // there was no Stop to press, or no reply left to stop
	}{
		{
			name: "stops at once and keeps the text", observed: healthy(380), want: domain.Pass,
			detail:   "stopped in 80 ms, 40 characters kept",
			measures: map[string]float64{"stop_ms": 80, "chars_before_stop": 40, "kept_chars": 40},
		},
		{
			name: "text that arrived after the click is not counted against it", observed: healthy(380), want: domain.Pass,
			scene: []func(*scene){kept(func(int) string { return strings.Repeat("x", 40) })},
		},
		{
			name: "takes its time", observed: healthy(300 + 1500), want: domain.Fail,
			detail: "Stop took 1500 ms to take effect, over the 1000 ms limit",
		},
		{
			name: "the text written is lost", observed: healthy(380), want: domain.Fail,
			scene:  []func(*scene){kept(func(int) string { return "" })},
			detail: "the 40 characters written before Stop were not kept",
		},
		{
			name:     "less is kept than was showing, counted in characters and not in bytes",
			observed: stopRecord(300, 380, [2]float64{250, 230}), want: domain.Fail,
			// 200 characters of two bytes each: 400 bytes, more than 230, but fewer characters
			scene:  []func(*scene){kept(func(int) string { return strings.Repeat("é", 200) })},
			detail: "Stop kept 200 of the 230 characters that were showing",
		},
		{
			name:     "text that came at the moment of the click was showing",
			observed: stopRecord(300, 380, [2]float64{100, 20}, [2]float64{300, 40}), want: domain.Fail,
			scene:  []func(*scene){kept(func(int) string { return strings.Repeat("x", 30) })},
			detail: "Stop kept 30 of the 40 characters that were showing",
		},
		{
			name: "the page ignores Stop", observed: healthy(380), want: domain.Fail,
			scene:  []func(*scene){withFlaws(flaws{stopIgnored: true})},
			detail: `after Stop the reply was "complete", not stopped`,
		},
		{
			// The reply is long, and a minute from now it would still be going.
			name: "the page ignores Stop and the reply is still being written when the time is up", observed: healthy(380), want: domain.Fail,
			scene: []func(*scene){withFlaws(flaws{stopIgnored: true}), func(s *scene) {
				s.agent = func(string, int) sceneReply { return sceneReply{stages: endless} }
			}},
			detail: `after Stop the reply was "streaming", not stopped`,
		},
		{
			name: "the reply is over before Stop can be pressed", observed: healthy(380), want: domain.NA, notPressed: true,
			scene: []func(*scene){func(s *scene) {
				s.agent = func(string, int) sceneReply { return sceneReply{stages: []sceneStage{{"complete", 12}}} }
			}},
			detail: "the agent was too fast for this probe",
		},
		{
			name: "the reply failed before it showed any text", observed: healthy(380), want: domain.NA, notPressed: true,
			scene: []func(*scene){func(s *scene) {
				s.agent = func(string, int) sceneReply { return sceneReply{stages: failing, failure: failureText} }
			}},
			detail: "the agent was too fast for this probe",
		},
		{
			// The reply was being written at one look and over at the next, the one for
			// the button: a page that is right has none to show by then.
			name: "the reply ended as Stop was reached for", observed: healthy(380), want: domain.NA, notPressed: true,
			scene: []func(*scene){func(s *scene) {
				s.agent = func(string, int) sceneReply { return sceneReply{stages: quickly} }
				s.beforeSnapshot = func(s *scene) {
					if m := s.newest(); m != nil {
						m.look()
					}
				}
			}},
			detail: "the agent was too fast for this probe",
		},
		{
			name: "there is no Stop button to press", observed: healthy(380), want: domain.Fail, notPressed: true,
			scene:  []func(*scene){withFlaws(flaws{stopHidden: true})},
			detail: "there was no Stop button while the reply was being written",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(tc.scene...)
			s.observed = tc.observed

			got, r := outcomeOf(t, s, (*devtools.Probes).Stop)

			assert.Equal(t, domain.P01, got.ID)
			assert.Equal(t, tc.want, got.Result, got.Detail)
			assert.Contains(t, got.Detail, tc.detail)
			for name, want := range tc.measures {
				assert.InDelta(t, want, got.Measures[name], 1e-9, name)
			}
			pressed := 1
			if tc.notPressed {
				pressed = 0
			}
			assert.Len(t, r.page.called("click"), pressed, "Stop is pressed once when it can be, and nothing else is")
		})
	}

	t.Run("the click was not seen by the observer", func(t *testing.T) {
		s := newScene()
		s.observed = recorded() // no stopAt
		var err error
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, s)
			_, err = r.probes.Stop(t.Context())
		})

		require.ErrorContains(t, err, "the click on Stop was not seen by the observer")
	})
	t.Run("a page that cannot be read is an error, not a verdict", func(t *testing.T) {
		s := newScene()
		var err error
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, s)
			r.page.scripts["lastReply"] = func(map[string]any) (any, error) { return nil, assert.AnError }
			_, err = r.probes.Stop(t.Context())
		})

		require.Error(t, err)
	})
	t.Run("nor is a page that cannot be read once Stop was pressed", func(t *testing.T) {
		s := newScene()
		s.observed = healthy(380)
		var err error
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, s)
			read := r.page.scripts["lastReply"]
			r.page.scripts["lastReply"] = func(args map[string]any) (any, error) {
				if len(r.page.called("click")) > 0 {
					return nil, errors.New("Execution context was destroyed")
				}
				return read(args)
			}
			_, err = r.probes.Stop(t.Context())
		})

		require.ErrorContains(t, err, "after Stop: reading the reply failed 20 times in a row")
	})
	t.Run("nor is a page that shows neither the button nor the reply any more", func(t *testing.T) {
		// The page went elsewhere between two looks, as it does when its session ends:
		// that it has no Stop button says nothing of Stop.
		s := newScene(func(s *scene) {
			s.beforeSnapshot = func(s *scene) {
				if s.newest() != nil {
					s.load("/")
				}
			}
		})
		var err error
		synctest.Test(t, func(t *testing.T) {
			_, err = newRig(t, s).probes.Stop(t.Context())
		})

		require.ErrorContains(t, err, "there is no Stop button, and the reply is no longer on the page")
	})
	t.Run("a probe that fails before it has a conversation has nothing to tidy, and says nothing of it", func(t *testing.T) {
		s := newScene()
		var log bytes.Buffer
		var err error
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, s)
			r.probes.KeepChats = false
			r.probes.Logger = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelWarn}))
			r.page.tools["fill"] = func(map[string]any) (*mcp.CallToolResult, error) { return errorResult("Error: gone"), nil }
			_, err = r.probes.Stop(t.Context())
		})

		require.ErrorContains(t, err, "filling the composer")
		assert.Empty(t, log.String(), "no complaint about a conversation that never was")
	})
}

func TestProbe_AProbeTidiesUp(t *testing.T) {
	// Every probe that starts a conversation deletes it, unless told to keep it,
	// whatever it found and wherever it stopped, with an error or without.
	over := func(s *scene) {
		s.agent = func(string, int) sceneReply { return sceneReply{stages: []sceneStage{{"complete", 12}}} }
	}
	// unanswered is an agent whose reply never ends, so the probe gives up waiting.
	unanswered := func(s *scene) {
		s.agent = func(string, int) sceneReply { return sceneReply{stages: endless} }
	}
	// opensOnce is a start page that opens the first time, and not the second.
	opensOnce := func(s *scene) {
		opened := 0
		s.opened = func(*scene, string) error {
			if opened++; opened == 2 {
				return errors.New("net::ERR_CONNECTION_REFUSED")
			}
			return nil
		}
	}
	// refusing is a page one of whose tools says no to the calls that match.
	refusing := func(tool string, match func(args map[string]any) bool) func(*rig) {
		return func(r *rig) {
			answer := r.page.tools[tool]
			r.page.tools[tool] = func(args map[string]any) (*mcp.CallToolResult, error) {
				if match(args) {
					return errorResult("Error: refused"), nil
				}
				return answer(args)
			}
		}
	}
	always := func(map[string]any) bool { return true }
	onSave := func(args map[string]any) bool { return args["uid"] == "save" }
	tests := []struct {
		name    string
		scene   []func(*scene)
		rig     func(*rig)
		probe   func(*devtools.Probes, context.Context) (domain.ProbeOutcome, error)
		want    domain.CheckResult
		wantErr string
	}{
		{name: "Stop", probe: (*devtools.Probes).Stop, want: domain.Pass},
		{name: "Stop, when the reply was over", scene: []func(*scene){over}, probe: (*devtools.Probes).Stop, want: domain.NA},
		{name: "Stop, when there is no button", scene: []func(*scene){withFlaws(flaws{stopHidden: true})}, probe: (*devtools.Probes).Stop, want: domain.Fail},
		{name: "Regenerate", probe: (*devtools.Probes).Regenerate, want: domain.Pass},
		{name: "reloading", probe: (*devtools.Probes).ReloadMidStream, want: domain.Pass},
		{name: "reloading, when the reply was over", scene: []func(*scene){over}, probe: (*devtools.Probes).ReloadMidStream, want: domain.NA},
		{name: "a failed reply", probe: (*devtools.Probes).AgentDown, want: domain.Pass},
		{name: "the layout shift", probe: (*devtools.Probes).LayoutShift, want: domain.Pass},
		{name: "the keyboard", probe: (*devtools.Probes).KeyboardOnly, want: domain.Pass},
		{name: "the keyboard, when Stop cannot be reached", scene: []func(*scene){withFlaws(flaws{stopNotFocusable: true})}, probe: (*devtools.Probes).KeyboardOnly, want: domain.Fail},
		{name: "the keyboard, when the second page will not open", scene: []func(*scene){opensOnce}, probe: (*devtools.Probes).KeyboardOnly, wantErr: "opening the start page"},
		{name: "the audit", probe: (*devtools.Probes).Lighthouse, want: domain.Pass},
		{name: "the audit, when its question gets no answer", scene: []func(*scene){unanswered}, probe: (*devtools.Probes).Lighthouse, wantErr: "waiting for the reply"},
		{name: "the audit, when it cannot be made", rig: refusing("lighthouse_audit", always), probe: (*devtools.Probes).Lighthouse, wantErr: "auditing for desktop"},
		{name: "renaming and deleting", probe: (*devtools.Probes).RenameDelete, want: domain.Pass},
		{name: "renaming, when its question gets no answer", scene: []func(*scene){unanswered}, probe: (*devtools.Probes).RenameDelete, wantErr: "waiting for the reply"},
		{name: "renaming, when Save cannot be pressed", rig: refusing("click", onSave), probe: (*devtools.Probes).RenameDelete, wantErr: "pressing Save"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(tc.scene...)
			s.observed = stopRecord(300, 380, [2]float64{200, 40})
			synctest.Test(t, func(t *testing.T) {
				r := newRig(t, s)
				r.probes.KeepChats = false
				r.page.tools["performance_start_trace"] = ok("started")
				r.page.tools["performance_stop_trace"] = ok(traceText("0.00"))
				r.page.tools["lighthouse_audit"] = ok(lighthouseText(95, 100))
				if tc.rig != nil {
					tc.rig(r)
				}

				out, err := tc.probe(r.probes, t.Context())

				if tc.wantErr != "" {
					require.ErrorContains(t, err, tc.wantErr)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, tc.want, out.Result, out.Detail)
			})
			assert.Empty(t, s.titles(), "the conversations the probe started are gone")
		})
	}

	t.Run("and kept when asked", func(t *testing.T) {
		s := newScene()
		s.observed = stopRecord(300, 380, [2]float64{200, 40})
		outcomeOf(t, s, (*devtools.Probes).Stop)

		assert.Len(t, s.titles(), 1)
	})
	t.Run("a conversation that cannot be deleted is tried again on a fresh page, and then said to be left", func(t *testing.T) {
		s := newScene()
		s.observed = stopRecord(300, 380, [2]float64{200, 40})
		var log bytes.Buffer
		var r *rig
		synctest.Test(t, func(t *testing.T) {
			r = newRig(t, s)
			r.probes.KeepChats = false
			r.probes.Logger = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelWarn}))
			refusing("click", func(args map[string]any) bool { return strings.HasPrefix(args["uid"].(string), "delete:") })(r)

			out, err := r.probes.Stop(t.Context())

			require.NoError(t, err)
			assert.Equal(t, domain.Pass, out.Result, "what the probe found does not depend on the tidying")
		})

		assert.Len(t, s.titles(), 1, "the conversation is still there")
		reloads := 0
		for _, c := range r.page.called("navigate_page") {
			if c.args["type"] == "reload" {
				reloads++
			}
		}
		assert.Equal(t, 1, reloads, "the page was loaded again once, for the second try")
		assert.Equal(t, 1, strings.Count(log.String(), "could not delete a probe's conversation"), log.String())
	})
}

// How long a probe waits for the page, each wait timed from where it begins.
func TestProbe_Waits(t *testing.T) {
	// atClick notes when an element whose uid starts with prefix is clicked. With
	// stuck, the click then takes longer than any probe waits, and fails.
	atClick := func(prefix string, stuck bool) func(*rig, *atomic.Pointer[time.Time]) {
		return func(r *rig, begun *atomic.Pointer[time.Time]) {
			click := r.page.tools["click"]
			r.page.tools["click"] = func(args map[string]any) (*mcp.CallToolResult, error) {
				uid, _ := args["uid"].(string)
				switch {
				case !strings.HasPrefix(uid, prefix):
					return click(args)
				case stuck:
					begun.Store(new(time.Now()))
					time.Sleep(time.Minute)
					return errorResult("Error: the page did not respond"), nil
				}
				begun.Store(new(time.Now()))
				return click(args)
			}
		}
	}
	tests := []struct {
		name    string
		scene   []func(*scene)
		begins  func(*rig, *atomic.Pointer[time.Time]) // nil: when the probe starts
		tidies  bool
		want    time.Duration
		wantErr string
	}{
		{
			name: "thirty seconds for a reply to show its first text", want: 30 * time.Second, wantErr: "waiting for the reply to start",
			scene: []func(*scene){func(s *scene) {
				s.agent = func(string, int) sceneReply { return sceneReply{stages: []sceneStage{{"pending", 0}}} }
			}},
		},
		{
			name: "ten seconds for Stop to take effect", want: 10 * time.Second, begins: atClick("stop", false),
			scene: []func(*scene){withFlaws(flaws{stopIgnored: true}), func(s *scene) {
				s.agent = func(string, int) sceneReply { return sceneReply{stages: endless} }
			}},
		},
		{
			name: "twenty seconds to delete its conversation", want: 20 * time.Second, begins: atClick("delete:", true), tidies: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(tc.scene...)
			s.observed = stopRecord(300, 380, [2]float64{200, 40})
			var waited time.Duration
			var err error
			synctest.Test(t, func(t *testing.T) {
				r := newRig(t, s)
				r.probes.KeepChats = !tc.tidies
				var begun atomic.Pointer[time.Time]
				begun.Store(new(time.Now()))
				if tc.begins != nil {
					tc.begins(r, &begun)
				}

				_, err = r.probes.Stop(t.Context())

				waited = time.Since(*begun.Load())
			})

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, waited)
		})
	}
}

func TestProbe_ARunThatIsStoppedIsNotAFinding(t *testing.T) {
	// When the run is cancelled while a probe waits for the page, the wait ends
	// and the probe says so: the page did not get its time, so nothing is said
	// of it.
	cancelledAfter := func(t *testing.T, s *scene, script string, when func(r *rig) bool, probe func(*devtools.Probes, context.Context) (domain.ProbeOutcome, error)) error {
		t.Helper()
		var err error
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, s)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			looked := r.page.scripts[script]
			r.page.scripts[script] = func(args map[string]any) (any, error) {
				if when(r) {
					cancel()
				}
				return looked(args)
			}
			_, err = probe(r.probes, ctx)
		})
		return err
	}
	s := func() *scene {
		sc := newScene()
		sc.observed = stopRecord(300, 380, [2]float64{200, 40})
		return sc
	}

	t.Run("while it waits for Stop to work", func(t *testing.T) {
		err := cancelledAfter(t, s(), "lastReply", func(r *rig) bool { return len(r.page.called("click")) > 0 }, (*devtools.Probes).Stop)

		require.ErrorIs(t, err, context.Canceled)
		assert.ErrorContains(t, err, "after Stop")
	})
	t.Run("while it waits for a reload to end", func(t *testing.T) {
		err := cancelledAfter(t, s(), "replies", func(*rig) bool { return true }, (*devtools.Probes).ReloadMidStream)

		require.ErrorIs(t, err, context.Canceled)
	})
}

// --- P-02 -------------------------------------------------------------------------------------

func TestProbe_Regenerate(t *testing.T) {
	tests := []struct {
		name    string
		scene   []func(*scene)
		want    domain.CheckResult
		detail  string
		wantErr string
	}{
		{name: "only the last reply is replaced", want: domain.Pass, detail: "only the last reply was replaced, and only it offers Regenerate"},
		{
			name: "the new reply is added instead of replacing the last", want: domain.Fail,
			scene:  []func(*scene){withFlaws(flaws{regenerateAdds: true})},
			detail: "the conversation had 2 replies and has 3 after regenerating",
		},
		{
			name: "an earlier reply is rewritten", want: domain.Fail,
			scene: []func(*scene){func(s *scene) {
				s.afterRegenerate = func(s *scene) { s.assistants()[0].chars++ }
			}},
			detail: "an earlier reply changed",
		},
		{
			name: "an earlier reply is replaced by one that says the same", want: domain.Fail,
			scene: []func(*scene){func(s *scene) {
				s.afterRegenerate = func(s *scene) { s.assistants()[0].id = "m-other" }
			}},
			detail: "an earlier reply changed",
		},
		{
			name: "every reply offers Regenerate", want: domain.Fail,
			scene:  []func(*scene){withFlaws(flaws{regenerateEverywhere: true})},
			detail: "2 Regenerate buttons are offered",
		},
		{
			name: "pressing Regenerate changes nothing", want: domain.Fail,
			scene:  []func(*scene){withFlaws(flaws{regenerateIgnored: true})},
			detail: "the last reply was not replaced by a new one",
		},
		{
			name: "the new reply is never finished", want: domain.NA,
			scene: []func(*scene){func(s *scene) {
				s.afterRegenerate = func(s *scene) { s.newest().stages = []sceneStage{{"streaming", 5}} }
			}},
			detail: "the agent was too slow for this probe",
		},
		{
			name: "no reply offers Regenerate", wantErr: `named "Regenerate response"`,
			scene: []func(*scene){withFlaws(flaws{noRegenerate: true})},
		},
		{
			name: "the page drops the first reply when the second is asked for", wantErr: "expected a conversation of two replies, found 1",
			scene: []func(*scene){withFlaws(flaws{forgetsEarlier: true})},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(tc.scene...)
			var got domain.ProbeOutcome
			var err error
			synctest.Test(t, func(t *testing.T) {
				got, err = newRig(t, s).probes.Regenerate(t.Context())
			})

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, domain.P02, got.ID)
			assert.Equal(t, tc.want, got.Result, got.Detail)
			assert.Contains(t, got.Detail, tc.detail)
		})
	}

	t.Run("a page that cannot be read after Regenerate is an error, not a verdict", func(t *testing.T) {
		s := newScene()
		var err error
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, s)
			read := r.page.scripts["lastReply"]
			r.page.scripts["lastReply"] = func(args map[string]any) (any, error) {
				if len(r.page.called("click")) > 0 {
					return nil, errors.New("Execution context was destroyed")
				}
				return read(args)
			}
			_, err = r.probes.Regenerate(t.Context())
		})

		require.ErrorContains(t, err, "reading the reply failed 20 times in a row")
	})
	t.Run("it asks about the second message, with the words of the first and the second", func(t *testing.T) {
		s := newScene()
		var r *rig
		synctest.Test(t, func(t *testing.T) {
			r = newRig(t, s)
			_, err := r.probes.Regenerate(t.Context())
			require.NoError(t, err)
		})

		var said []string
		for _, c := range r.page.called("fill") {
			said = append(said, c.args["value"].(string))
		}
		assert.Equal(t, []string{shortPrompt + " (first)", shortPrompt + " (second)"}, said)
		assert.Len(t, r.page.called("click"), 1, "Regenerate is pressed once")
	})
}

// --- P-05 -------------------------------------------------------------------------------------

func TestProbe_RenameDelete(t *testing.T) {
	tests := []struct {
		name   string
		flaws  flaws
		want   domain.CheckResult
		detail string
	}{
		{name: "both work and last", want: domain.Pass, detail: "renaming and deleting work and persist across a reload"},
		{name: "the sidebar keeps the old title", flaws: flaws{renameNotShown: true}, want: domain.Fail, detail: "the new title did not show after renaming"},
		{name: "the new title is not kept", flaws: flaws{renameLost: true}, want: domain.Fail, detail: "the new title was gone after a reload"},
		{name: "the row stays after deleting", flaws: flaws{deleteNotShown: true}, want: domain.Fail, detail: "the conversation was still listed after deleting it"},
		{name: "the deleted conversation returns", flaws: flaws{deleteLost: true}, want: domain.Fail, detail: "the deleted conversation came back after a reload"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(withFlaws(tc.flaws))

			got, _ := outcomeOf(t, s, (*devtools.Probes).RenameDelete)

			assert.Equal(t, domain.P05, got.ID)
			assert.Equal(t, tc.want, got.Result, got.Detail)
			assert.Contains(t, got.Detail, tc.detail)
		})
	}

	t.Run("it works on its own conversation when others have the same title", func(t *testing.T) {
		// Every question of an evaluation opens with the same words, so the
		// sidebar fills with conversations that have one title.
		s := newScene()
		s.seed(titleFrom(shortPrompt), 2)
		var r *rig
		synctest.Test(t, func(t *testing.T) {
			r = newRig(t, s)
			got, err := r.probes.RenameDelete(t.Context())
			require.NoError(t, err)
			assert.Equal(t, domain.Pass, got.Result, got.Detail)
		})

		assert.Equal(t, map[string]string{"seed-1": "say hello", "seed-2": "say hello"}, s.titles(),
			"the other two are as they were, and only the probe's own is gone")
		fills := r.page.called("fill")
		require.Len(t, fills, 2, "the message, and the new title")
		assert.Equal(t, "Renamed by agent-eval", fills[1].args["value"])
	})
}

// --- P-07 -------------------------------------------------------------------------------------

func TestProbe_ReloadMidStream(t *testing.T) {
	tests := []struct {
		name   string
		scene  []func(*scene)
		want   domain.CheckResult
		detail string
	}{
		{name: "the same reply comes back and ends", want: domain.Pass, detail: "one reply, 230 characters, ended"},
		{
			name: "the reply is over before the page can be reloaded", want: domain.NA,
			scene: []func(*scene){func(s *scene) {
				s.agent = func(string, int) sceneReply { return sceneReply{stages: []sceneStage{{"complete", 12}}} }
			}},
			detail: "the reply finished before the page could be reloaded",
		},
		{
			name: "the reply failed before it showed any text", want: domain.NA,
			scene: []func(*scene){func(s *scene) {
				s.agent = func(string, int) sceneReply { return sceneReply{stages: failing, failure: failureText} }
			}},
			detail: "the reply finished before the page could be reloaded",
		},
		{
			name: "the reply is there twice", want: domain.Fail,
			scene: []func(*scene){func(s *scene) {
				s.afterReload = func(s *scene) {
					c := s.open()
					copied := *c.messages[len(c.messages)-1]
					copied.id += "-again"
					c.messages = append(c.messages, &copied)
				}
			}},
			detail: "1 questions and 2 replies",
		},
		{
			name: "the reply never ends", want: domain.Fail,
			scene: []func(*scene){func(s *scene) {
				s.afterReload = func(s *scene) { s.newest().stages = []sceneStage{{"streaming", 90}} }
			}},
			detail: "never reached a final status",
		},
		{
			name: "the reply comes back shorter than it was", want: domain.Fail,
			scene: []func(*scene){func(s *scene) {
				s.afterReload = func(s *scene) { s.newest().stages = []sceneStage{{"complete", 10}} }
			}},
			detail: "the reply has 10 characters, it had 40 before",
		},
		{
			name: "the reply comes back empty", want: domain.Fail,
			scene: []func(*scene){func(s *scene) {
				s.afterReload = func(s *scene) { s.newest().stages = []sceneStage{{"complete", 0}} }
			}},
			detail: "the reply has no text",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(tc.scene...)

			got, _ := outcomeOf(t, s, (*devtools.Probes).ReloadMidStream)

			assert.Equal(t, domain.P07, got.ID)
			assert.Equal(t, tc.want, got.Result, got.Detail)
			assert.Contains(t, got.Detail, tc.detail)
		})
	}

	t.Run("a reply that does not end is given a minute, whether the time runs out in a pause or in a look", func(t *testing.T) {
		// A look that takes longer than the pause between looks: the end of the
		// minute falls inside one.
		s := newScene(func(s *scene) {
			s.afterReload = func(s *scene) { s.newest().stages = []sceneStage{{"streaming", 90}} }
		})
		var got domain.ProbeOutcome
		var elapsed time.Duration
		synctest.Test(t, func(t *testing.T) {
			r := newRig(t, s)
			looked := r.page.scripts["replies"]
			r.page.scripts["replies"] = func(args map[string]any) (any, error) {
				time.Sleep(400 * time.Millisecond)
				return looked(args)
			}
			start := time.Now()
			var err error
			got, err = r.probes.ReloadMidStream(t.Context())
			require.NoError(t, err, "a reply that never ends is what the probe looks for")
			elapsed = time.Since(start)
		})

		assert.Equal(t, domain.Fail, got.Result, got.Detail)
		assert.Contains(t, got.Detail, "never reached a final status")
		assert.InDelta(t, time.Minute.Seconds(), elapsed.Seconds(), 5)
	})
	t.Run("the page is loaded again once, while the reply is being written", func(t *testing.T) {
		s := newScene()
		_, r := outcomeOf(t, s, (*devtools.Probes).ReloadMidStream)

		var reloads int
		for _, c := range r.page.called("navigate_page") {
			if c.args["type"] == "reload" {
				reloads++
				assert.Contains(t, c.args["initScript"], "window.__agentEval", "with the observer in the new page")
			}
		}
		assert.Equal(t, 1, reloads)
	})
}

// --- P-10 -------------------------------------------------------------------------------------

func TestProbe_AgentDown(t *testing.T) {
	always := func(r sceneReply) func(*scene) {
		return func(s *scene) { s.agent = func(string, int) sceneReply { return r } }
	}
	tests := []struct {
		name   string
		scene  []func(*scene)
		want   domain.CheckResult
		detail string
	}{
		{name: "it fails, says so, and Retry gets the answer", want: domain.Pass, detail: "a failed reply says so and Retry gets the answer"},
		{
			name: "the agent does not fail", want: domain.Fail,
			scene:  []func(*scene){always(sceneReply{stages: quickly})},
			detail: "did not show as failed",
		},
		{
			name: "the page shows nothing final", want: domain.Fail,
			scene:  []func(*scene){always(sceneReply{stages: endless})},
			detail: "did not show as failed",
		},
		{
			name: "Retry gets no answer", want: domain.Fail,
			scene: []func(*scene){func(s *scene) {
				s.agent = func(_ string, attempt int) sceneReply {
					if attempt == 1 {
						return sceneReply{stages: failing, failure: failureText}
					}
					return sceneReply{stages: endless}
				}
			}},
			detail: "Retry did not get an answer",
		},
		{
			name: "the failed reply says nothing to the reader", want: domain.Fail,
			scene:  []func(*scene){always(sceneReply{stages: failing})},
			detail: "gave the reader no message",
		},
		{
			name: "the failed reply offers no Retry", want: domain.Fail,
			scene:  []func(*scene){withFlaws(flaws{noRetry: true})},
			detail: "offered no Retry",
		},
		{
			name: "Retry fails again", want: domain.Fail,
			scene:  []func(*scene){always(sceneReply{stages: failing, failure: failureText})},
			detail: "Retry did not get an answer",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(tc.scene...)

			got, _ := outcomeOf(t, s, (*devtools.Probes).AgentDown)

			assert.Equal(t, domain.P10, got.ID)
			assert.Equal(t, tc.want, got.Result, got.Detail)
			assert.Contains(t, got.Detail, tc.detail)
		})
	}

	t.Run("without a scripted agent there is nothing to try, and the browser is left alone", func(t *testing.T) {
		s := newScene()
		var got domain.ProbeOutcome
		var r *rig
		synctest.Test(t, func(t *testing.T) {
			r = newRig(t, s)
			r.probes.Prompts.Flaky = ""
			var err error
			got, err = r.probes.AgentDown(t.Context())
			require.NoError(t, err)
		})

		assert.Equal(t, domain.NA, got.Result)
		assert.Contains(t, got.Detail, "needs the scripted agent")
		assert.Empty(t, r.page.sequence())
	})
	t.Run("each run asks in words of its own, so that the agent has not seen them", func(t *testing.T) {
		// The scripted agent fails a question the first time it is asked and
		// remembers: a second run with the same words would find it answering.
		var said []string
		for range 2 {
			s := newScene()
			_, r := outcomeOf(t, s, (*devtools.Probes).AgentDown)
			said = append(said, r.page.called("fill")[0].args["value"].(string))
		}

		for _, words := range said {
			assert.True(t, strings.HasPrefix(words, flakyPrompt+" #"), words)
			assert.Greater(t, len(words), len(flakyPrompt+" #"), "with something after the mark")
		}
		assert.NotEqual(t, said[0], said[1])
	})
}
