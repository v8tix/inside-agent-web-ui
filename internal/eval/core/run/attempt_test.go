package run_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	resmocks "github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource/mocks"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/run"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/score"
)

const timeout = 3 * time.Minute

var problem = domain.Problem{TaskID: "t1", Level: 1, Question: "What is 6 times 7?", Truth: "42"}

// healthyObs is a page on which every passive check passes (see the uicheck
// tests): Enter at 1000, first text at 1200, finished at 1600.
func healthyObs() domain.UIObservations {
	return domain.UIObservations{
		KeydownAt: 1000,
		Updates:   []domain.Update{{At: 1200, Chars: 20}, {At: 1300, Chars: 80}, {At: 1400, Chars: 150}, {At: 1500, Chars: 230}},
		Statuses: []domain.StatusChange{
			{At: 1100, Status: domain.ReplyPending}, {At: 1210, Status: domain.ReplyStreaming}, {At: 1600, Status: domain.ReplyComplete},
		},
		Scrolls:  []domain.ScrollSample{{At: 1200, Scrollable: true}, {At: 1500, FromBottomPx: 20, Scrollable: true}},
		Console:  []domain.ConsoleMessage{{Type: "log", Text: "ready"}},
		Requests: []domain.NetworkRequest{{Method: "GET", URL: "http://web/", Status: 200}},
		Message: domain.MessageInfo{
			Status: domain.ReplyComplete, TTFTMs: new(int64(150)), PromptTokens: new(120), CompletionTokens: new(30), ToolCalls: []string{"search"},
		},
		VisibleText: "Thinking it over. FINAL ANSWER: 42",
	}
}

func reply(status domain.ReplyStatus) func(context.Context) (domain.Reply, error) {
	return func(context.Context) (domain.Reply, error) {
		return domain.Reply{ConversationID: "c1", MessageID: "m1", Status: status}, nil
	}
}

// slow waits until its context ends, as a driver does when the reply is still going.
func slow(ctx context.Context) (domain.Reply, error) {
	<-ctx.Done()
	return domain.Reply{ConversationID: "c1", MessageID: "m1", Status: domain.ReplyStreaming}, fmt.Errorf("waiting for the reply: %w", domain.ErrReplyTimeout)
}

// plan is how the browser behaves in one scenario. Whatever the plan leaves
// out the driver is not expected to be asked, and the mock fails the test if it is.
type plan struct {
	startChat, submit error
	awaits            []func(context.Context) (domain.Reply, error) // one per AwaitReply call
	stop              error
	stopCalled        bool
	raw               string
	rawErr            error
	readsRaw          bool
	obs               domain.UIObservations
	collectErr        error
	collects          bool
	screenshotErr     error
	evidenceErr       error
	deleteErr         error
}

type rig struct {
	driver    *resmocks.UIDriver
	store     *resmocks.ResultStore
	attempter *run.Attempter
	calls     []string
	typed     string
	evidence  []string // names the store was asked for
	dir       string
}

func (p plan) rig(t *testing.T) *rig {
	t.Helper()
	r := &rig{driver: resmocks.NewUIDriver(t), store: resmocks.NewResultStore(t), dir: t.TempDir()}
	record := func(name string) func(mock.Arguments) {
		return func(mock.Arguments) { r.calls = append(r.calls, name) }
	}
	ctx := mock.Anything

	r.driver.On("StartChat", ctx).Run(record("StartChat")).Return(p.startChat).Maybe()
	r.driver.On("Submit", ctx, mock.Anything).Run(func(args mock.Arguments) {
		r.calls = append(r.calls, "Submit")
		r.typed = args.String(1)
	}).Return(p.submit).Maybe()
	for _, fn := range p.awaits {
		r.driver.On("AwaitReply", ctx).Run(record("AwaitReply")).Return(fn).Once()
	}
	if p.stopCalled {
		r.driver.On("Stop", ctx).Run(record("Stop")).Return(p.stop).Once()
	}
	if p.readsRaw {
		r.driver.On("ReadRaw", ctx, mock.Anything).Run(record("ReadRaw")).Return(p.raw, p.rawErr).Once()
	}
	if p.collects {
		r.driver.On("Collect", ctx).Run(record("Collect")).Return(p.obs, p.collectErr).Once()
	}
	r.driver.On("Screenshot", ctx, mock.Anything).Run(record("Screenshot")).Return(p.screenshotErr).Maybe()
	r.driver.On("DeleteChat", ctx).Run(record("DeleteChat")).Return(p.deleteErr).Maybe()
	r.store.On("EvidencePath", ctx, "run-1", mock.Anything).Run(func(args mock.Arguments) {
		r.calls = append(r.calls, "EvidencePath")
		r.evidence = append(r.evidence, args.String(2))
	}).Return(func(_ context.Context, _, name string) string { return filepath.Join(r.dir, name) }, p.evidenceErr).Maybe()

	r.attempter = &run.Attempter{Driver: r.driver, Store: r.store, Timeout: timeout, Screenshots: true}
	return r
}

func (r *rig) run(t *testing.T, prob domain.Problem) (domain.Result, error) {
	t.Helper()
	return r.attempter.Run(t.Context(), run.Request{RunID: "run-1", Problem: prob, Attempt: 1})
}

// answered is the plan of a reply that finished and was read.
func answered(raw string) plan {
	obs := healthyObs()
	obs.VisibleText = raw // the page shows what the agent wrote
	return plan{awaits: []func(context.Context) (domain.Reply, error){reply(domain.ReplyComplete)}, raw: raw, readsRaw: true, obs: obs, collects: true}
}

func passingChecks() map[domain.CheckID]domain.CheckResult {
	m := map[domain.CheckID]domain.CheckResult{}
	for _, id := range domain.PassiveChecks {
		m[id] = domain.Pass
	}
	return m
}

func TestAttempt_Outcomes(t *testing.T) {
	t.Run("a correct answer", func(t *testing.T) {
		r := answered("Thinking it over.\nFINAL ANSWER: 42").rig(t)

		got, err := r.run(t, problem)

		require.NoError(t, err)
		want := domain.Result{
			RunID: "run-1", TaskID: "t1", Level: 1, Attempt: 1, Status: domain.StatusCorrect, Score: 1,
			ConversationID: "c1",
			LatencyMs:      new(int64(600)), TTFTUIMs: new(int64(200)), TTFTAgentMs: new(int64(150)), UIOverheadMs: new(int64(50)),
			Updates: 4, MaxGapMs: new(int64(100)),
			Tokens: domain.Tokens{Prompt: 120, Completion: 30}, ToolCalls: []string{"search"},
			UIChecks: passingChecks(),
		}
		assert.Equal(t, want, got)
		assert.Equal(t, []string{"StartChat", "Submit", "AwaitReply", "ReadRaw", "Collect", "DeleteChat"}, r.calls)
	})
	t.Run("the prompt typed is the built-in one around the question", func(t *testing.T) {
		r := answered("FINAL ANSWER: 42").rig(t)

		_, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, score.DefaultPrompt().Compose(problem.Question), r.typed)
		assert.Contains(t, r.typed, "FINAL ANSWER")
		assert.Contains(t, r.typed, problem.Question)
	})
	t.Run("a prompt of the user's own", func(t *testing.T) {
		r := answered("FINAL ANSWER: 42").rig(t)
		custom, err := score.ParsePrompt("Please answer: {{.Question}} (end with FINAL ANSWER:)")
		require.NoError(t, err)
		r.attempter.Prompt = custom

		_, err = r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, "Please answer: What is 6 times 7? (end with FINAL ANSWER:)", r.typed)
	})
	t.Run("a wrong answer", func(t *testing.T) {
		r := answered("FINAL ANSWER: 43").rig(t)

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, domain.StatusIncorrect, got.Status)
		assert.Zero(t, got.Score)
		assert.Empty(t, got.Evidence, "a wrong answer is the agent's affair; no screenshot")
		assert.NotContains(t, r.calls, "Screenshot")
	})
	t.Run("the official scorer decides, quirks and all", func(t *testing.T) {
		p := problem
		p.Truth = "1000"
		r := answered("FINAL ANSWER: $1,000").rig(t)

		got, err := r.run(t, p)

		require.NoError(t, err)
		assert.Equal(t, domain.StatusCorrect, got.Status)
	})
	t.Run("no marker", func(t *testing.T) {
		r := answered("I believe it is 42.").rig(t)

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, domain.StatusNoFinalAnswer, got.Status)
		assert.Zero(t, got.Score)
		assert.Equal(t, domain.NA, got.UIChecks[domain.U06], "there was no answer to look for on the page")
	})
	t.Run("the agent failed", func(t *testing.T) {
		p := plan{awaits: []func(context.Context) (domain.Reply, error){reply(domain.ReplyFailed)}, obs: healthyObs(), collects: true}
		p.obs.Message.Status = domain.ReplyFailed
		p.obs.Statuses[2].Status = domain.ReplyFailed
		r := p.rig(t)

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, domain.StatusAgentError, got.Status)
		assert.Equal(t, run.ClassAgentFailed, got.ErrorClass)
		assert.NotContains(t, r.calls, "ReadRaw", "there is no answer to read")
		assert.Equal(t, []string{"evidence/t1-1-agent-error.png"}, got.Evidence)
		assert.Equal(t, []string{"StartChat", "Submit", "AwaitReply", "Collect", "EvidencePath", "Screenshot", "DeleteChat"}, r.calls)
		require.NotNil(t, got.LatencyMs, "an error bubble is a reply that ended")
	})
}

func TestAttempt_Timeout(t *testing.T) {
	stopped := healthyObs()
	stopped.Message.Status = domain.ReplyStopped
	stopped.Statuses[2].Status = domain.ReplyStopped

	t.Run("Stop ends the reply: the agent was slow, the UI was fine", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := plan{
				awaits:     []func(context.Context) (domain.Reply, error){slow, reply(domain.ReplyStopped)},
				stopCalled: true, obs: stopped, collects: true,
			}.rig(t)
			start := time.Now()

			got, err := r.run(t, problem)

			require.NoError(t, err)
			assert.Equal(t, timeout, time.Since(start), "it waited the whole time and no longer")
			assert.Equal(t, domain.StatusTimeout, got.Status)
			assert.Equal(t, run.ClassTimeout, got.ErrorClass)
			assert.Equal(t, domain.NA, got.UIChecks[domain.U03], "a slow agent is not a UI fault")
			assert.Nil(t, got.LatencyMs, "a timeout has no latency")
			assert.Equal(t, []string{"evidence/t1-1-timeout.png"}, got.Evidence)
			assert.Equal(t, []string{"StartChat", "Submit", "AwaitReply", "Stop", "AwaitReply", "Collect", "EvidencePath", "Screenshot", "DeleteChat"}, r.calls,
				"it does not read or score a reply that did not finish")
		})
	})
	t.Run("Stop does nothing: that is the UI's fault", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stuck := healthyObs()
			stuck.Statuses = stuck.Statuses[:2]
			stuck.Message.Status = domain.ReplyStreaming
			r := plan{
				awaits:     []func(context.Context) (domain.Reply, error){slow, slow},
				stopCalled: true, obs: stuck, collects: true,
			}.rig(t)

			got, err := r.run(t, problem)

			require.NoError(t, err)
			assert.Equal(t, domain.StatusTimeout, got.Status, "the agent outcome is still a timeout")
			assert.Equal(t, domain.Fail, got.UIChecks[domain.U03])
			require.Len(t, got.Notes, 1)
			assert.Contains(t, got.Notes[0], `U-03: the message was still "streaming" after Stop`)
		})
	})
	t.Run("the Stop button itself breaks", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			stuck := healthyObs()
			stuck.Statuses = stuck.Statuses[:2]
			stuck.Message.Status = domain.ReplyStreaming
			r := plan{awaits: []func(context.Context) (domain.Reply, error){slow}, stopCalled: true, stop: errors.New("no Stop button"), obs: stuck, collects: true}.rig(t)

			got, err := r.run(t, problem)

			require.NoError(t, err)
			assert.Equal(t, domain.StatusTimeout, got.Status)
			assert.Equal(t, domain.Fail, got.UIChecks[domain.U03])
		})
	})
	t.Run("Stop is given only so long", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := plan{awaits: []func(context.Context) (domain.Reply, error){slow, slow}, stopCalled: true, obs: healthyObs(), collects: true}.rig(t)
			r.attempter.StopWait = 2 * time.Second
			start := time.Now()

			_, err := r.run(t, problem)

			require.NoError(t, err)
			assert.Equal(t, timeout+2*time.Second, time.Since(start))
		})
	})
	t.Run("Stop is given ten seconds unless told otherwise", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := plan{awaits: []func(context.Context) (domain.Reply, error){slow}, obs: healthyObs(), collects: true}.rig(t)
			r.driver.On("Stop", mock.Anything).Return(hang).Once() // a click that never comes back
			start := time.Now()

			got, err := r.run(t, problem)

			require.NoError(t, err)
			assert.Equal(t, timeout+10*time.Second, time.Since(start))
			assert.Equal(t, domain.StatusTimeout, got.Status, "a Stop that did not work leaves the attempt a timeout")
		})
	})
}

func TestAttempt_ThePageBreaks(t *testing.T) {
	tests := []struct {
		name      string
		plan      plan
		class     string
		wantCalls []string
		evidence  bool
	}{
		{"it cannot open a conversation", plan{startChat: errors.New("no new-chat button")}, run.ClassStartChat,
			[]string{"StartChat"}, false},
		{"it cannot take the prompt", plan{submit: errors.New("composer not found")}, run.ClassSubmit,
			[]string{"StartChat", "Submit", "EvidencePath", "Screenshot", "DeleteChat"}, true},
		{"waiting for the reply goes wrong", plan{awaits: []func(context.Context) (domain.Reply, error){func(context.Context) (domain.Reply, error) {
			return domain.Reply{}, errors.New("the page navigated away")
		}}, obs: healthyObs(), collects: true}, run.ClassAwaitReply,
			[]string{"StartChat", "Submit", "AwaitReply", "Collect", "EvidencePath", "Screenshot", "DeleteChat"}, true},
		{"the reply cannot be read", plan{awaits: []func(context.Context) (domain.Reply, error){reply(domain.ReplyComplete)}, readsRaw: true, rawErr: errors.New("404"), obs: healthyObs(), collects: true}, run.ClassReadRaw,
			[]string{"StartChat", "Submit", "AwaitReply", "ReadRaw", "Collect", "EvidencePath", "Screenshot", "DeleteChat"}, true},
		{"the reply was stopped by nobody", plan{awaits: []func(context.Context) (domain.Reply, error){reply(domain.ReplyStopped)}, obs: healthyObs(), collects: true}, run.ClassUnexpectedEnd,
			[]string{"StartChat", "Submit", "AwaitReply", "Collect", "EvidencePath", "Screenshot", "DeleteChat"}, true},
		{"the wait ends with the reply not final", plan{awaits: []func(context.Context) (domain.Reply, error){reply(domain.ReplyStreaming)}, obs: healthyObs(), collects: true}, run.ClassUnexpectedEnd,
			[]string{"StartChat", "Submit", "AwaitReply", "Collect", "EvidencePath", "Screenshot", "DeleteChat"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.plan.rig(t)

			got, err := r.run(t, problem)

			require.NoError(t, err, "a broken page is a row, not an error")
			assert.Equal(t, domain.StatusUIError, got.Status)
			assert.Equal(t, tc.class, got.ErrorClass)
			assert.Zero(t, got.Score)
			assert.Equal(t, tc.wantCalls, r.calls)
			if tc.evidence {
				assert.Equal(t, []string{"evidence/t1-1-ui-error.png"}, got.Evidence)
			} else {
				assert.Empty(t, got.Evidence)
			}
		})
	}
	t.Run("no driver error text reaches the row", func(t *testing.T) {
		r := plan{submit: errors.New("could not fill: What is 6 times 7? SECRET-CANARY")}.rig(t)

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.NotContains(t, fmt.Sprintf("%+v", got), "CANARY")
	})
}

func TestAttempt_TheRunCannotGoOn(t *testing.T) {
	died := fmt.Errorf("mcp: %w", domain.ErrDriverDied)
	await := func(err error) []func(context.Context) (domain.Reply, error) {
		return []func(context.Context) (domain.Reply, error){func(context.Context) (domain.Reply, error) { return domain.Reply{}, err }}
	}
	tests := []struct {
		name string
		plan plan
	}{
		{"StartChat", plan{startChat: died}},
		{"Submit", plan{submit: died}},
		{"AwaitReply", plan{awaits: await(died)}},
		{"ReadRaw", plan{awaits: []func(context.Context) (domain.Reply, error){reply(domain.ReplyComplete)}, readsRaw: true, rawErr: died}},
		{"Collect", plan{awaits: []func(context.Context) (domain.Reply, error){reply(domain.ReplyComplete)}, readsRaw: true, raw: "FINAL ANSWER: 42", collects: true, collectErr: died}},
	}
	for _, tc := range tests {
		t.Run("the driver dies in "+tc.name, func(t *testing.T) {
			r := tc.plan.rig(t)

			got, err := r.run(t, problem)

			require.ErrorIs(t, err, domain.ErrDriverDied)
			assert.Equal(t, domain.Result{}, got, "no row: the batch restarts the driver and tries again")
			assert.NotContains(t, r.calls, "DeleteChat", "there is no session to clean up")
		})
	}
	t.Run("the driver dies while stopping", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := plan{awaits: []func(context.Context) (domain.Reply, error){slow}, stopCalled: true, stop: died}.rig(t)

			_, err := r.run(t, problem)

			require.ErrorIs(t, err, domain.ErrDriverDied)
		})
	})
	t.Run("the driver dies waiting after Stop", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := plan{awaits: []func(context.Context) (domain.Reply, error){slow, func(context.Context) (domain.Reply, error) { return domain.Reply{}, died }}, stopCalled: true}.rig(t)

			_, err := r.run(t, problem)

			require.ErrorIs(t, err, domain.ErrDriverDied)
		})
	})
	t.Run("Ctrl-C while waiting", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := plan{awaits: []func(context.Context) (domain.Reply, error){slow}}.rig(t)
			ctx, cancel := context.WithCancel(t.Context())
			time.AfterFunc(30*time.Second, cancel)

			got, err := r.attempter.Run(ctx, run.Request{RunID: "run-1", Problem: problem, Attempt: 1})

			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, domain.Result{}, got, "an interrupted attempt is not recorded; resuming asks it again")
			assert.NotContains(t, r.calls, "Stop", "the run is over, not timing out")
			assert.NotContains(t, r.calls, "DeleteChat")
		})
	})
}

func TestAttempt_WhatIsLeftOver(t *testing.T) {
	t.Run("reading the observations fails but the answer is known", func(t *testing.T) {
		p := answered("FINAL ANSWER: 42")
		p.collectErr = errors.New("evaluate_script failed")
		r := p.rig(t)

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, domain.StatusCorrect, got.Status, "the agent's answer counts all the same")
		assert.Equal(t, run.ClassCollect, got.ErrorClass)
		assert.Nil(t, got.UIChecks, "and no UI check is claimed")
		assert.Nil(t, got.LatencyMs)
	})
	t.Run("the conversation is deleted, and a failure to is only logged", func(t *testing.T) {
		p := answered("FINAL ANSWER: 42")
		p.deleteErr = errors.New("menu did not open")
		r := p.rig(t)

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, domain.StatusCorrect, got.Status)
		assert.Contains(t, r.calls, "DeleteChat")
	})
	t.Run("KEEP_CHATS leaves it", func(t *testing.T) {
		r := answered("FINAL ANSWER: 42").rig(t)
		r.attempter.KeepChats = true

		_, err := r.run(t, problem)

		require.NoError(t, err)
		assert.NotContains(t, r.calls, "DeleteChat")
	})
	t.Run("a failed UI check keeps a screenshot and says why", func(t *testing.T) {
		p := answered("FINAL ANSWER: 42")
		p.obs.Console = []domain.ConsoleMessage{{Type: "error", Text: "boom"}}
		r := p.rig(t)

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, domain.StatusCorrect, got.Status, "the agent was right")
		assert.Equal(t, domain.Fail, got.UIChecks[domain.U01])
		assert.Equal(t, 1, got.ConsoleErrors)
		assert.Equal(t, []string{"U-01: 1 console error; the first is an error of 4 characters (its text is kept only with --include-text)"}, got.Notes,
			"the page's own words are not in the notes of a run that keeps no text")
		assert.Equal(t, []string{"evidence/t1-1-ui-check.png"}, got.Evidence)
		assert.Equal(t, "t1-1-ui-check.png", r.evidence[0])
	})
	t.Run("a run that keeps text quotes the console message", func(t *testing.T) {
		p := answered("FINAL ANSWER: 42")
		p.obs.Console = []domain.ConsoleMessage{{Type: "error", Text: "boom"}}
		r := p.rig(t)
		r.attempter.IncludeText = true

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, []string{"U-01: 1 console error; the first: boom"}, got.Notes)
	})
	t.Run("the thresholds given are the ones used", func(t *testing.T) {
		r := answered("FINAL ANSWER: 42").rig(t)
		r.attempter.Thresholds = domain.DefaultThresholds()
		r.attempter.Thresholds.OverheadP95Ms = 10 // the page added 50

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, domain.Fail, got.UIChecks[domain.U05])
	})
	t.Run("a task id cannot reach out of the evidence directory", func(t *testing.T) {
		p := answered("FINAL ANSWER: 43")
		p.obs.Console = []domain.ConsoleMessage{{Type: "error"}}
		r := p.rig(t)
		prob := problem
		prob.TaskID = "../x y/z"

		_, err := r.run(t, prob)

		require.NoError(t, err)
		require.Len(t, r.evidence, 1)
		assert.NotContains(t, r.evidence[0], "/")
		assert.NotContains(t, r.evidence[0], " ")
		assert.Equal(t, "_x_y_z-1-ui-check.png", r.evidence[0])
	})
	t.Run("no place for the screenshot, or no picture", func(t *testing.T) {
		for name, mutate := range map[string]func(*plan){
			"the store refuses": func(p *plan) { p.evidenceErr = errors.New("disk full") },
			"the driver fails":  func(p *plan) { p.screenshotErr = errors.New("page closed") },
		} {
			t.Run(name, func(t *testing.T) {
				p := answered("FINAL ANSWER: 42")
				p.obs.Console = []domain.ConsoleMessage{{Type: "error"}}
				mutate(&p)
				r := p.rig(t)

				got, err := r.run(t, problem)

				require.NoError(t, err)
				assert.Empty(t, got.Evidence)
				assert.Equal(t, domain.StatusCorrect, got.Status)
			})
		}
	})
	t.Run("no store, no screenshot, no crash", func(t *testing.T) {
		p := answered("FINAL ANSWER: 42")
		p.obs.Console = []domain.ConsoleMessage{{Type: "error"}}
		r := p.rig(t)
		r.attempter.Store = nil

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Empty(t, got.Evidence)
	})
}

func TestAttempt_Text(t *testing.T) {
	t.Run("kept when asked", func(t *testing.T) {
		r := answered("FINAL ANSWER: 41").rig(t)
		r.attempter.IncludeText = true

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, "What is 6 times 7?", got.Question)
		assert.Equal(t, "42", got.Truth)
		assert.Equal(t, "41", got.Answer)
	})
	t.Run("the answer is None when there was none", func(t *testing.T) {
		r := answered("no idea").rig(t)
		r.attempter.IncludeText = true

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, score.NoAnswer, got.Answer)
	})
	t.Run("left out otherwise", func(t *testing.T) {
		r := answered("FINAL ANSWER: 41").rig(t)

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Empty(t, got.Question)
		assert.Empty(t, got.Truth)
		assert.Empty(t, got.Answer)
		assert.NotContains(t, fmt.Sprintf("%+v", got), "6 times 7")
	})
}

func TestAttempt_RefusesWhatItCannotAsk(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*run.Attempter, *run.Request)
		want   string
	}{
		{"no timeout", func(a *run.Attempter, _ *run.Request) { a.Timeout = 0 }, "timeout must be positive"},
		{"attempt zero", func(_ *run.Attempter, r *run.Request) { r.Attempt = 0 }, "attempts start at 1"},
		{"a problem with no answer to score", func(_ *run.Attempter, r *run.Request) { r.Problem.Unscored = true }, "should have been skipped"},
		{"a problem with no question", func(_ *run.Attempter, r *run.Request) { r.Problem.Question = "" }, "has no question"},
		{"a problem with no id", func(_ *run.Attempter, r *run.Request) { r.Problem.TaskID = "" }, "no task id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := plan{}.rig(t) // a driver nothing is expected of
			req := run.Request{RunID: "run-1", Problem: problem, Attempt: 1}
			tc.mutate(r.attempter, &req)

			got, err := r.attempter.Run(t.Context(), req)

			require.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), tc.want), err.Error())
			assert.Equal(t, domain.Result{}, got)
			assert.Empty(t, r.calls, "nothing was done in the browser")
		})
	}
}

func TestAttempt_CancelledWhileOpening(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	r := plan{}.rig(t)
	r.driver.ExpectedCalls = nil // replace the default StartChat
	r.driver.On("StartChat", mock.Anything).Run(func(mock.Arguments) { cancel() }).Return(errors.New("interrupted")).Once()

	got, err := r.attempter.Run(ctx, run.Request{RunID: "run-1", Problem: problem, Attempt: 1})

	require.ErrorIs(t, err, context.Canceled, "an error caused by Ctrl-C is not a broken page")
	assert.Equal(t, domain.Result{}, got)
}

func TestAttempt_Logs(t *testing.T) {
	var logs bytes.Buffer
	p := answered("FINAL ANSWER: 42")
	p.obs.Console = []domain.ConsoleMessage{{Type: "error", Text: "boom"}}
	r := p.rig(t)
	r.attempter.Logger = slog.New(slog.NewTextHandler(&logs, nil))

	_, err := r.run(t, problem)

	require.NoError(t, err)
	assert.Contains(t, logs.String(), "attempt finished")
	assert.Contains(t, logs.String(), "task=t1")
	assert.Contains(t, logs.String(), "a UI check failed")
	assert.Contains(t, logs.String(), "U-01")
	assert.NotContains(t, logs.String(), "6 times 7", "the question is not logged")
	assert.NotContains(t, logs.String(), "FINAL ANSWER", "nor is the answer")
}

func TestAttempt_ScreenshotsShowTheQuestionSoTheyAreOptIn(t *testing.T) {
	failing := func() plan {
		p := answered("FINAL ANSWER: 42")
		p.obs.Console = []domain.ConsoleMessage{{Type: "error", Text: "boom"}}
		return p
	}

	t.Run("not kept unless asked for", func(t *testing.T) {
		r := failing().rig(t)
		r.attempter.Screenshots = false

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, domain.Fail, got.UIChecks[domain.U01], "the check still failed")
		assert.Equal(t, []string{"U-01: 1 console error; the first is an error of 4 characters (its text is kept only with --include-text)"}, got.Notes, "and says why")
		assert.Empty(t, got.Evidence)
		assert.NotContains(t, r.calls, "Screenshot")
		assert.NotContains(t, r.calls, "EvidencePath", "the page was not even looked at")
	})
	t.Run("kept when asked for", func(t *testing.T) {
		r := failing().rig(t)

		got, err := r.run(t, problem)

		require.NoError(t, err)
		assert.Equal(t, []string{"evidence/t1-1-ui-check.png"}, got.Evidence)
	})
}
