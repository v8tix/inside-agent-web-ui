// The evaluator's self-test, end to end: the real Web UI, a real PostgreSQL, the
// scripted agent running its fixture, headless Chrome driven through Chrome
// DevTools MCP, and the evaluator's own runner, scorer and checks. If the
// evaluator is right, a run over the invented problems finds exactly the
// results written in selftest_expected.json, every time.
//
// Group A: needs Docker, Node (npx) and Chrome.
package evalselftest_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/run"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/store"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/synthetic"
	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
	"github.com/v8tix/inside-agent-web-ui/it/internal/evalenv"
)

// selftestEnv is the Web UI in front of the scripted agent that plays the
// self-test's script.
func selftestEnv(t *testing.T) *evalenv.Env {
	t.Helper()
	fixture, err := testagent.ParseFixture(synthetic.Replies())
	require.NoError(t, err)
	return evalenv.Start(t, testagent.WithFixture(fixture), testagent.WithWordDelay(5*time.Millisecond))
}

// selftestRunner is the evaluator's runner, wired as `agent-eval selftest` wires
// it: a real Chrome through Chrome DevTools MCP, the invented problems, the store.
func selftestRunner(t *testing.T, env *evalenv.Env, st *store.Store, log *slog.Logger) *run.Runner {
	factory := &devtools.Factory{
		Session: evalenv.BrowserOptions(t),
		Driver:  devtools.DriverConfig{BaseURL: env.URL},
	}
	return &run.Runner{
		Source: synthetic.Problems(), Drivers: factory, Store: st,
		Thresholds: domain.DefaultThresholds(),
		Info:       run.RunInfo{UIContract: devtools.SupportedContract},
		Versions:   factory.Versions,
		Logger:     log,
	}
}

// selftestRequest asks for the whole self-test under the run id.
func selftestRequest(env *evalenv.Env, id string) service.RunRequest {
	return service.RunRequest{
		RunID: id, Command: "selftest",
		Filter:  service.Filter{Attachments: domain.AttachmentsSkip},
		Repeats: 1, Workers: 2, Timeout: run.SelftestTimeout, Screenshots: true,
		Email: env.Email, Password: env.Password,
	}
}

// selftest runs the invented problems once and returns the outcome and the
// directory the run's files are in.
func selftest(t *testing.T, env *evalenv.Env, results string, id string) (service.Outcome, string) {
	t.Helper()
	st := store.New(results)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	out, err := selftestRunner(t, env, st, nil).Run(ctx, selftestRequest(env, id))
	require.NoError(t, err)
	dir, err := st.RunDir(id)
	require.NoError(t, err)
	return out, dir
}

// fingerprint is what a run must repeat exactly: each attempt's status, and the
// verdict on each passive check.
type fingerprint struct {
	statuses map[string]domain.AttemptStatus
	verdicts map[domain.CheckID]domain.CheckResult
}

func fingerprintOf(t *testing.T, out service.Outcome, st *store.Store, id string) fingerprint {
	t.Helper()
	rows, err := st.Load(t.Context(), id)
	require.NoError(t, err)
	f := fingerprint{statuses: map[string]domain.AttemptStatus{}, verdicts: out.Summary.UI.Verdicts}
	for _, r := range rows {
		f.statuses[r.Key().String()] = r.Status
	}
	return f
}

func TestSelftestSuite(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := selftestEnv(t)
	expected, err := synthetic.Expected()
	require.NoError(t, err)
	results := t.TempDir()
	st := store.New(results)
	started := time.Now()

	var prints []fingerprint
	var first service.Outcome
	var firstDir string
	for i, id := range []string{"selftest-1", "selftest-2", "selftest-3"} {
		out, dir := selftest(t, env, results, id)
		if i == 0 {
			first, firstDir = out, dir
		}
		t.Logf("%s: %d attempts, %d correct, must-failed=%v, overhead p50=%.0f p95=%.0f max=%.0f ms", id, out.Summary.Agent.Attempted, out.Summary.Agent.Correct,
			out.Summary.UI.MustFailed, out.Summary.UI.OverheadMs.P50, out.Summary.UI.OverheadMs.P95, out.Summary.UI.OverheadMs.Max)
		if rows, loadErr := st.Load(t.Context(), id); loadErr == nil {
			var overheads []string
			for _, r := range rows {
				if r.UIOverheadMs != nil {
					overheads = append(overheads, fmt.Sprintf("%s=%d", strings.TrimPrefix(r.TaskID, "syn-"), *r.UIOverheadMs))
				}
			}
			t.Logf("%s: overhead per attempt (ms, in the order they finished): %s", id, strings.Join(overheads, " "))
		}

		if len(out.Summary.UI.MustFailed) > 0 { // say which attempts, so a flake can be told from a fault
			if rows, loadErr := st.Load(t.Context(), id); loadErr == nil {
				for _, r := range rows {
					for check, result := range r.UIChecks {
						if result == domain.Fail {
							t.Logf("%s: %s attempt %d failed %s %s (console errors %d, failed requests %d, error class %q): %s",
								id, r.TaskID, r.Attempt, check, check.Name(), r.ConsoleErrors, r.FailedRequests, r.ErrorClass, strings.Join(r.Notes, " | "))
							for _, line := range append(env.Logs.Lines("/stop"), env.Logs.Lines("level=WARN")...) {
								t.Logf("  server: %s", line)
							}
							for _, line := range env.Logs.Lines("level=ERROR") {
								t.Logf("  server: %s", line)
							}
						}
					}
				}
			}
		}
		assert.Empty(t, expected.Compare(out.Summary), "%s found what the golden says", id)
		assert.Empty(t, out.Summary.UI.MustFailed, "%s: every required UI check passes on the real Web UI", id)
		assert.False(t, out.Interrupted)
		prints = append(prints, fingerprintOf(t, out, st, id))
	}

	t.Run("three runs in a row give the same results", func(t *testing.T) {
		require.Len(t, prints, 3)
		assert.Equal(t, prints[0], prints[1])
		assert.Equal(t, prints[0], prints[2])
	})
	t.Run("and it all takes well under five minutes", func(t *testing.T) {
		assert.Less(t, time.Since(started), 5*time.Minute)
	})

	t.Run("each kind of attempt is what the script says it is", func(t *testing.T) {
		rows, err := st.Load(t.Context(), "selftest-1")
		require.NoError(t, err)
		byTask := map[string]domain.Result{}
		for _, r := range rows {
			byTask[r.TaskID] = r
		}

		for task, want := range map[string]domain.AttemptStatus{
			"syn-l1-01": domain.StatusCorrect, "syn-l1-02": domain.StatusCorrect, "syn-l1-03": domain.StatusCorrect,
			"syn-l1-04": domain.StatusIncorrect, "syn-l1-05": domain.StatusSkipped,
			"syn-l2-01": domain.StatusCorrect, "syn-l2-02": domain.StatusCorrect, "syn-l2-03": domain.StatusNoFinalAnswer,
			"syn-l2-04": domain.StatusAgentError, "syn-l2-05": domain.StatusCorrect,
			"syn-l3-01": domain.StatusTimeout, "syn-l3-02": domain.StatusCorrect, "syn-l3-03": domain.StatusCorrect,
		} {
			assert.Equal(t, want, byTask[task].Status, task)
		}
		assert.Equal(t, "attachment", byTask["syn-l1-05"].ErrorClass)
		assert.Equal(t, []string{"lookup"}, byTask["syn-l2-05"].ToolCalls, "the tool call the agent reported")
		assert.Positive(t, byTask["syn-l1-01"].Tokens.Prompt+byTask["syn-l1-01"].Tokens.Completion, "and its tokens")

		long := byTask["syn-l3-02"]
		assert.Equal(t, domain.Pass, long.UIChecks[domain.U04], "a long reply arrives in pieces")
		assert.Equal(t, domain.Pass, long.UIChecks[domain.U07], "and is followed down the page")
		assert.Equal(t, domain.Pass, long.UIChecks[domain.U06], "and the answer is on the page")

		slow := byTask["syn-l3-01"]
		assert.Equal(t, domain.NA, slow.UIChecks[domain.U03], "a slow agent is not a stuck page: Stop worked")
		assert.Nil(t, slow.LatencyMs, "a timeout has no latency")
		assert.NotEmpty(t, slow.Evidence, "and its screenshot was kept")
		assert.NotEmpty(t, byTask["syn-l2-04"].Evidence, "so was the error bubble's")
		for _, r := range rows {
			if r.Status == domain.StatusCorrect || r.Status == domain.StatusIncorrect {
				assert.Zero(t, r.ConsoleErrors, r.TaskID)
				assert.Zero(t, r.FailedRequests, r.TaskID)
			}
		}
	})

	t.Run("the files of a run", func(t *testing.T) {
		for _, name := range []string{"results.jsonl", "run.json", "summary.json", "summary.md"} {
			assert.FileExists(t, filepath.Join(firstDir, name))
		}
		evidence, err := os.ReadDir(filepath.Join(firstDir, "evidence"))
		require.NoError(t, err)
		var names []string
		for _, e := range evidence {
			names = append(names, e.Name())
		}
		assert.Contains(t, strings.Join(names, " "), "syn-l3-01-1-timeout.png")
		assert.Contains(t, strings.Join(names, " "), "syn-l2-04-1-agent-error.png")

		meta, err := st.LoadMeta(t.Context(), "selftest-1")
		require.NoError(t, err)
		assert.Equal(t, "selftest", meta.Command)
		assert.NotZero(t, meta.FinishedAt)
		assert.Equal(t, "1.10.1", meta.Versions["chrome-devtools-mcp"], "the server said so")
		assert.Regexp(t, `^\d+(\.\d+)*$`, meta.Versions["chrome"], "and so did the browser")
		assert.Equal(t, "1", meta.UIContract)
	})

	t.Run("nothing the agent was asked or answered is in the results", func(t *testing.T) {
		for _, name := range []string{"results.jsonl", "run.json", "summary.json", "summary.md"} {
			data, err := os.ReadFile(filepath.Join(firstDir, name))
			require.NoError(t, err)
			for _, secret := range []string{"capital of France", "capital of Germany", "Munich", "forty-two", "in the south, I think", env.Password} {
				assert.NotContains(t, string(data), secret, name)
			}
		}
	})

	t.Run("the summary says the same", func(t *testing.T) {
		a := first.Summary.Agent
		assert.Equal(t, [3]int{13, 12, 8}, [3]int{a.Eligible, a.Attempted, a.Correct})
		assert.InDelta(t, 12.0/13, a.Coverage, 1e-9)
		assert.Equal(t, 9, a.FormatCompliance.Successes)
		assert.Equal(t, 10, a.FormatCompliance.Total)
		for id, verdict := range first.Summary.UI.Verdicts {
			assert.NotEqual(t, domain.Fail, verdict, "%s %s", id, id.Name())
		}
	})
}
