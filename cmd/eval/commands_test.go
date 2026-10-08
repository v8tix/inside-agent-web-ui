package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/probe"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/report"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/run"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/store"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/synthetic"
)

var fixedNow = time.Date(2026, 10, 2, 16, 17, 32, 0, time.UTC)

// fakeRunner is a service.Runner that records the request it was given.
type fakeRunner struct {
	req     service.RunRequest
	setup   runnerSetup
	outcome service.Outcome
	err     error
	called  bool
}

func (f *fakeRunner) Run(_ context.Context, req service.RunRequest) (service.Outcome, error) {
	f.called, f.req = true, req
	return f.outcome, f.err
}

// fakeProber is a service.Prober that records what it was asked and what the
// probes would have said to the agent.
type fakeProber struct {
	runID   string
	setup   proberSetup
	summary domain.Summary
	err     error
	called  bool
	// ctrlC, when set, ends the context of the run as the probes start, as the
	// user pressing Ctrl-C does.
	ctrlC func()
}

func (f *fakeProber) Probe(_ context.Context, runID string) (domain.Summary, error) {
	f.called, f.runID = true, runID
	if f.ctrlC != nil {
		f.ctrlC()
	}
	return f.summary, f.err
}

// realHarness is a CLI with its real handlers, a fake Web UI to find ready, a
// fake runner, and a configuration that comes from a map.
type realHarness struct {
	*cli
	out, errOut bytes.Buffer
	env         map[string]string
	runner      *fakeRunner
	prober      *fakeProber
	webUI       *httptest.Server
	webStatus   int
}

func newRealHarness(t *testing.T) *realHarness {
	t.Helper()
	h := &realHarness{runner: &fakeRunner{}, prober: &fakeProber{}, webStatus: http.StatusOK}
	h.webUI = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(h.webStatus)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(h.webUI.Close)
	h.env = map[string]string{
		"EVAL_BASE_URL":      h.webUI.URL,
		"EVAL_USER_EMAIL":    "eval@example.com",
		"EVAL_USER_PASSWORD": "a-password-for-tests",
		"RESULTS_DIR":        filepath.Join(t.TempDir(), "results"),
		"HF_DATASETS_DIR":    filepath.Join(t.TempDir(), "datasets"),
	}
	h.cli = newCLI(&h.out, &h.errOut)
	h.clock = func() time.Time { return fixedNow }
	h.loadConfig = func(string) (config.Config, error) {
		return config.Load(func(k string) (string, bool) { v, ok := h.env[k]; return v, ok })
	}
	h.buildRunner = func(s runnerSetup) service.Runner {
		h.runner.setup = s
		return h.runner
	}
	h.buildProber = func(s proberSetup) service.Prober {
		h.prober.setup = s
		return h.prober
	}
	return h
}

func (h *realHarness) do(args ...string) int { return h.run(context.Background(), args) }

// hasRun makes a run exist in the results directory, as one that was begun and
// interrupted does, so that it can be resumed.
func (h *realHarness) hasRun(t *testing.T, runID string) {
	t.Helper()
	require.NoError(t, store.New(h.env["RESULTS_DIR"]).Begin(t.Context(), domain.RunMeta{RunID: runID, Command: "run"}))
}

func finishedOutcome(mutate ...func(*service.Outcome)) service.Outcome {
	out := service.Outcome{
		Meta: domain.RunMeta{RunID: "r1", Command: "run", StartedAt: fixedNow, FinishedAt: fixedNow.Add(11*time.Minute + 3*time.Second)},
		Summary: report.Aggregate(report.Input{RunID: "r1", Rows: []domain.Result{
			{TaskID: "a", Level: 1, Attempt: 1, Status: domain.StatusCorrect},
			{TaskID: "b", Level: 1, Attempt: 1, Status: domain.StatusIncorrect},
			{TaskID: "c", Level: 2, Attempt: 1, Status: domain.StatusSkipped},
		}}),
	}
	for _, m := range mutate {
		m(&out)
	}
	return out
}

func TestNewRunID(t *testing.T) {
	id := newRunID("", fixedNow)

	assert.Regexp(t, `^20261002T161732Z-[0-9a-f]{6}$`, id)
	assert.Regexp(t, `^selftest-20261002T161732Z-[0-9a-f]{6}$`, newRunID("selftest-", fixedNow))
	assert.NotEqual(t, id, newRunID("", fixedNow), "two started in the same second differ")
	_, err := store.New(t.TempDir()).RunDir(id)
	assert.NoError(t, err, "and the store accepts it as a directory name")
}

func TestToolVersions(t *testing.T) {
	cfg, err := config.Load(func(string) (string, bool) { return "", false })
	require.NoError(t, err)

	got := toolVersions(cfg)

	assert.Equal(t, "1.10.1", got["chrome-devtools-mcp"], "the version the settings pin")
	assert.NotEmpty(t, got["go"])
	assert.False(t, strings.HasPrefix(got["go"], "go"))
}

func TestLoadPrompt(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "prompt.tmpl")
	require.NoError(t, os.WriteFile(custom, []byte("Answer briefly: {{.Question}}"), 0o600))

	t.Run("the built-in prompt", func(t *testing.T) {
		p, err := loadPrompt(config.Config{})

		require.NoError(t, err)
		assert.Contains(t, p.Compose("Q?"), "FINAL ANSWER")
	})
	t.Run("a prompt file", func(t *testing.T) {
		cfg := config.Config{}
		cfg.Eval.PromptFile = custom

		p, err := loadPrompt(cfg)

		require.NoError(t, err)
		assert.Equal(t, "Answer briefly: Q?", p.Compose("Q?"))
	})
	t.Run("a file that is not there", func(t *testing.T) {
		cfg := config.Config{}
		cfg.Eval.PromptFile = filepath.Join(dir, "none")

		_, err := loadPrompt(cfg)

		require.ErrorContains(t, err, "reading the prompt template")
	})
}

func TestCheckWebUI(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		h := newRealHarness(t)

		require.NoError(t, h.checkWebUI(t.Context(), h.webUI.URL))
	})
	t.Run("not ready", func(t *testing.T) {
		h := newRealHarness(t)
		h.webStatus = http.StatusServiceUnavailable

		err := h.checkWebUI(t.Context(), h.webUI.URL)

		var exit *exitError
		require.ErrorAs(t, err, &exit)
		assert.Equal(t, exitInfra, exit.code)
		assert.ErrorContains(t, err, "not ready (/readyz answered HTTP 503)")
	})
	t.Run("not there", func(t *testing.T) {
		h := newRealHarness(t)
		url := h.webUI.URL
		h.webUI.Close()

		err := h.checkWebUI(t.Context(), url)

		var exit *exitError
		require.ErrorAs(t, err, &exit)
		assert.Equal(t, exitInfra, exit.code)
		assert.ErrorContains(t, err, "is not answering")
		assert.ErrorContains(t, err, "docker compose --profile full up")
	})
	t.Run("an address that cannot be used", func(t *testing.T) {
		h := newRealHarness(t)

		err := h.checkWebUI(t.Context(), "http://[::1")

		var exit *exitError
		require.ErrorAs(t, err, &exit)
		assert.Equal(t, exitUsage, exit.code)
	})

	// EVAL_BASE_URL can carry a user and a password, and a token in its query,
	// which is why the loader never echoes it. A message names where the address
	// points and nothing else, and so does the cause it quotes: net/http and
	// net/url quote the address in their errors, the first with the password
	// masked and the rest as it is.
	t.Run("an address with credentials is named without them", func(t *testing.T) {
		withCredentials := func(address string) string {
			return strings.Replace(address, "://", "://canary-user:canary-pass@", 1)
		}
		t.Run("not ready", func(t *testing.T) {
			h := newRealHarness(t)
			h.webStatus = http.StatusServiceUnavailable

			err := h.checkWebUI(t.Context(), withCredentials(h.webUI.URL))

			require.ErrorContains(t, err, "the Web UI at "+h.webUI.URL+" is not ready (/readyz answered HTTP 503)")
			assert.NotContains(t, err.Error(), "canary")
		})
		t.Run("not there", func(t *testing.T) {
			h := newRealHarness(t)
			plain := h.webUI.URL
			h.webUI.Close()

			err := h.checkWebUI(t.Context(), withCredentials(plain)+"/ui?token=canary-query")

			require.ErrorContains(t, err, "the Web UI at "+plain+"/ui is not answering (dial tcp ")
			assert.NotContains(t, err.Error(), "canary")
		})
		t.Run("cannot be used", func(t *testing.T) {
			h := newRealHarness(t)

			err := h.checkWebUI(t.Context(), withCredentials("http://[::1"))

			var exit *exitError
			require.ErrorAs(t, err, &exit)
			assert.Equal(t, exitUsage, exit.code)
			assert.ErrorContains(t, err, `EVAL_BASE_URL "unreadable" is not a usable address: missing ']' in host`)
			assert.NotContains(t, err.Error(), "canary")
		})
	})
}

func TestRunCommand_Request(t *testing.T) {
	t.Run("from the settings", func(t *testing.T) {
		h := newRealHarness(t)
		h.env["EVAL_WORKERS"], h.env["EVAL_REPEATS"], h.env["EVAL_TASK_TIMEOUT"], h.env["EVAL_KEEP_CHATS"] = "3", "2", "7m", "true"
		h.env["GAIA_ATTACHMENTS"] = "include"
		h.runner.outcome = finishedOutcome()

		code := h.do("run")

		require.Equal(t, exitOK, code, h.errOut.String())
		req := h.runner.req
		assert.Regexp(t, `^20261002T161732Z-[0-9a-f]{6}$`, req.RunID)
		assert.Equal(t, "run", req.Command)
		assert.Equal(t, service.Filter{Attachments: domain.AttachmentsInclude}, req.Filter)
		assert.Equal(t, [3]any{2, 3, 7 * time.Minute}, [3]any{req.Repeats, req.Workers, req.Timeout})
		assert.True(t, req.KeepChats)
		assert.False(t, req.IncludeText)
		assert.False(t, req.Screenshots, "no text, no pictures of it")
		assert.Equal(t, "eval@example.com", req.Email)
		assert.Equal(t, "a-password-for-tests", req.Password)
	})
	t.Run("the flags win", func(t *testing.T) {
		h := newRealHarness(t)
		h.env["EVAL_WORKERS"], h.env["EVAL_REPEATS"] = "3", "2"
		h.runner.outcome = finishedOutcome()
		h.hasRun(t, "run-7")

		code := h.do("run", "--level", "2", "--limit", "5", "--task-id", "x,y", "--repeats", "4", "--workers", "1", "--timeout", "90s", "--include-text", "--human-typing", "--resume", "run-7")

		require.Equal(t, exitOK, code, h.errOut.String())
		req := h.runner.req
		assert.Equal(t, "run-7", req.RunID, "resuming names the run")
		assert.Equal(t, service.Filter{Level: 2, Limit: 5, TaskIDs: []string{"x", "y"}, Attachments: domain.AttachmentsSkip}, req.Filter)
		assert.Equal(t, [3]any{4, 1, 90 * time.Second}, [3]any{req.Repeats, req.Workers, req.Timeout})
		assert.True(t, req.IncludeText)
		assert.True(t, req.Screenshots, "pictures show the question and the reply, so they follow --include-text")
		assert.True(t, req.HumanTyping)
		assert.True(t, h.runner.setup.humanTyping)
		assert.NotNil(t, h.runner.setup.source)
		assert.NotNil(t, h.runner.setup.store)
		assert.Equal(t, h.env["RESULTS_DIR"], h.runner.setup.cfg.ResultsDir)
	})
	t.Run("a user is needed", func(t *testing.T) {
		h := newRealHarness(t)
		delete(h.env, "EVAL_USER_EMAIL")
		delete(h.env, "EVAL_USER_PASSWORD")

		code := h.do("run")

		assert.Equal(t, exitUsage, code)
		assert.Contains(t, h.errOut.String(), "no user to sign in as")
		assert.False(t, h.runner.called)
	})
	t.Run("a Web UI that is not up is found out before a browser starts", func(t *testing.T) {
		h := newRealHarness(t)
		h.webStatus = http.StatusServiceUnavailable

		code := h.do("run")

		assert.Equal(t, exitInfra, code)
		assert.False(t, h.runner.called)
	})
	t.Run("a prompt file that is broken", func(t *testing.T) {
		h := newRealHarness(t)
		h.env["EVAL_PROMPT_FILE"] = filepath.Join(t.TempDir(), "none")

		code := h.do("run")

		assert.Equal(t, exitUsage, code)
		assert.False(t, h.runner.called)
	})
	t.Run("results that git would pick up are refused", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git is not installed")
		}
		repo := t.TempDir()
		require.NoError(t, exec.Command("git", "-C", repo, "init", "-q").Run())
		h := newRealHarness(t)
		h.env["RESULTS_DIR"] = filepath.Join(repo, "results")

		code := h.do("run")

		assert.Equal(t, exitUsage, code)
		assert.Contains(t, h.errOut.String(), "not ignored")
		assert.False(t, h.runner.called)
	})
	// A typo in the id, after a long run or a paid model, would otherwise start
	// everything again under the id that was mistyped, and exit 0.
	t.Run("a run to resume that does not exist", func(t *testing.T) {
		h := newRealHarness(t)

		code := h.do("run", "--resume", "20261002T161732Z-abc12")

		assert.Equal(t, exitUsage, code)
		assert.Contains(t, h.errOut.String(), `there is no run "20261002T161732Z-abc12" to resume`)
		assert.Contains(t, h.errOut.String(), h.env["RESULTS_DIR"], "it says where it looked")
		assert.False(t, h.runner.called, "nothing was started under the wrong name")
		assert.NoDirExists(t, filepath.Join(h.env["RESULTS_DIR"], "20261002T161732Z-abc12"))
	})
	t.Run("a run whose record cannot be read is an infrastructure problem, not a typo", func(t *testing.T) {
		h := newRealHarness(t)
		h.hasRun(t, "run-9")
		require.NoError(t, os.WriteFile(filepath.Join(h.env["RESULTS_DIR"], "run-9", "run.json"), []byte("{not json"), 0o600))

		code := h.do("run", "--resume", "run-9")

		assert.Equal(t, exitInfra, code)
		assert.Contains(t, h.errOut.String(), "reading the run to resume")
		assert.NotContains(t, h.errOut.String(), "there is no run")
		assert.False(t, h.runner.called)
	})
	t.Run("a run that exists can be resumed", func(t *testing.T) {
		h := newRealHarness(t)
		h.hasRun(t, "run-9")
		h.runner.outcome = finishedOutcome()

		code := h.do("run", "--resume", "run-9")

		assert.Equal(t, exitOK, code, h.errOut.String())
		assert.True(t, h.runner.called)
	})
	t.Run("a run id the store would not accept", func(t *testing.T) {
		h := newRealHarness(t)

		code := h.do("run", "--resume", "../elsewhere")

		assert.Equal(t, exitUsage, code)
		assert.False(t, h.runner.called)
	})
}

func TestRunCommand_Outcomes(t *testing.T) {
	tests := []struct {
		name    string
		outcome service.Outcome
		err     error
		args    []string
		want    int
		wantErr string
	}{
		{"a finished run", finishedOutcome(), nil, nil, exitOK, ""},
		{"interrupted", finishedOutcome(func(o *service.Outcome) { o.Interrupted = true }), nil, nil, exitInterrupted, "continue with: agent-eval run --resume 20261002T161732Z-"},
		{"a required UI check failed", finishedOutcome(func(o *service.Outcome) {
			o.Summary.UI.MustFailed = []domain.CheckID{domain.U03, domain.P01}
		}), nil, nil, exitUIFailed, "required UI checks failed: U-03, P-01"},
		{"accuracy below the minimum", finishedOutcome(), nil, []string{"--min-accuracy", "0.9"}, exitAccuracy, "accuracy 50.0% of the 2 problems asked is below the minimum of 90.0%"},
		{"accuracy at the minimum", finishedOutcome(), nil, []string{"--min-accuracy", "0.5"}, exitOK, ""},
		{"a UI failure is reported before accuracy", finishedOutcome(func(o *service.Outcome) {
			o.Summary.UI.MustFailed = []domain.CheckID{domain.U01}
		}), nil, []string{"--min-accuracy", "0.99"}, exitUIFailed, "U-01"},
		{"an interrupted run is not judged", finishedOutcome(func(o *service.Outcome) { o.Interrupted = true }), nil, []string{"--min-accuracy", "0.99"}, exitInterrupted, "interrupted"},
		// A run that exercised nothing, or in which the page could not be driven,
		// has not shown that the Web UI works, and must not exit as if it had.
		{"every problem was skipped", finishedOutcome(func(o *service.Outcome) {
			o.Summary = report.Aggregate(report.Input{RunID: "r1", Rows: []domain.Result{
				{TaskID: "a", Level: 1, Attempt: 1, Status: domain.StatusSkipped, ErrorClass: "attachment"},
			}})
		}), nil, nil, exitUsage, "nothing was asked"},
		{"the dataset held no problem at all", finishedOutcome(func(o *service.Outcome) { o.Summary = report.Aggregate(report.Input{RunID: "r1"}) }), nil, nil, exitUsage, "nothing was asked"},
		{"the page could not be started", finishedOutcome(func(o *service.Outcome) {
			o.Summary.UI.UIErrors = map[string]int{"start_chat": 2}
		}), nil, nil, exitInfra, "the page could not be driven in 2 attempts (start_chat 2)"},
		{"the browser died", finishedOutcome(func(o *service.Outcome) {
			o.Summary.UI.UIErrors = map[string]int{"driver_died": 1}
		}), nil, nil, exitInfra, "driver_died 1"},
		{"the page broke an attempt in its own way", finishedOutcome(func(o *service.Outcome) {
			o.Summary.UI.UIErrors = map[string]int{"submit": 1}
		}), nil, nil, exitUIFailed, "the page broke 1 attempt (submit 1)"},
		{"a failed check is reported before a broken attempt", finishedOutcome(func(o *service.Outcome) {
			o.Summary.UI.MustFailed = []domain.CheckID{domain.U03}
			o.Summary.UI.UIErrors = map[string]int{"start_chat": 1}
		}), nil, nil, exitUIFailed, "required UI checks failed: U-03"},
		{"the way to continue is given for a run the page broke", finishedOutcome(func(o *service.Outcome) {
			o.Summary.UI.UIErrors = map[string]int{"start_chat": 1}
		}), nil, nil, exitInfra, "agent-eval run --resume"},
		{"the dataset is missing", service.Outcome{}, &run.SelectionError{Err: errors.New("no GAIA files: run `agent-eval datasets fetch` first")}, nil, exitUsage, "datasets fetch"},
		{"the credentials are refused", service.Outcome{}, fmt.Errorf("signing in: %w", domain.ErrLoginFailed), nil, exitUsage, "check EVAL_USER_EMAIL"},
		{"the page speaks another contract", service.Outcome{}, fmt.Errorf("opening: %w", domain.ErrContractVersion), nil, exitUsage, "UI contract"},
		{"anything else", service.Outcome{}, errors.New("chrome-devtools-mcp not found"), nil, exitInfra, "chrome-devtools-mcp not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newRealHarness(t)
			h.runner.outcome, h.runner.err = tc.outcome, tc.err

			code := h.do(append([]string{"run"}, tc.args...)...)

			assert.Equal(t, tc.want, code, h.errOut.String())
			if tc.wantErr != "" {
				assert.Contains(t, h.errOut.String(), tc.wantErr)
			}
		})
	}
	t.Run("a run that began and then failed says how to continue", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.err = errors.New("starting a browser: no space left")
		h.hasRun(t, "run-9") // the run's directory exists, as it does once a run has begun

		code := h.do("run", "--resume", "run-9")

		assert.Equal(t, exitInfra, code)
		assert.Contains(t, h.errOut.String(), "continue with: agent-eval run --resume run-9")
	})
}

func TestRunCommand_Digest(t *testing.T) {
	h := newRealHarness(t)
	h.hasRun(t, "r1")
	out := finishedOutcome()
	out.Summary.UI.Undecided = nil // every required check was decided, and none failed
	out.Summary.UI.OverheadMs = domain.Quantiles{N: 2, P50: 44, P95: 51}
	out.Summary.UI.OverheadBasis = "p50"
	out.Summary.Thresholds = domain.DefaultThresholds()
	h.runner.outcome = out

	require.Equal(t, exitOK, h.do("run", "--resume", "r1"), h.errOut.String())

	digest := h.out.String()
	assert.Contains(t, digest, "Run r1 (run) finished in 11m3s")
	assert.Contains(t, digest, "asked 2 of 3 problems (coverage 66.7%), 1 correct")
	assert.Contains(t, digest, "accuracy of those asked:   50.0% (95% CI")
	assert.Contains(t, digest, "level 1: 1 of 2 asked correct")
	assert.Contains(t, digest, "statuses: correct 1 · incorrect 1 · skipped 1")
	assert.Contains(t, digest, "all required checks passed")
	assert.Contains(t, digest, "UI overhead: p50 44 ms, p95 51 ms (limit 100 ms, judged on the median: too few attempts for a 95th percentile)")
	assert.Contains(t, digest, "Full report: "+filepath.Join(h.env["RESULTS_DIR"], "r1")+"/summary.md")
	assert.Contains(t, h.errOut.String(), "agent-eval run r1: results go to")
	assert.NotContains(t, digest+h.errOut.String(), "a-password-for-tests", "the password is never printed")

	t.Run("a failed check is named", func(t *testing.T) {
		h := newRealHarness(t)
		h.hasRun(t, "r1")
		h.runner.outcome = finishedOutcome(func(o *service.Outcome) {
			o.Summary.UI.MustFailed = []domain.CheckID{domain.U07}
			o.Interrupted = true
			o.Meta.FinishedAt = time.Time{}
		})

		h.do("run", "--resume", "r1")

		assert.Contains(t, h.out.String(), "Run r1 (run) interrupted\n")
		assert.Contains(t, h.out.String(), "FAILED: U-07 Scroll follow")
	})
	t.Run("repeats are described", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome(func(o *service.Outcome) {
			o.Summary.Agent.Repeatability = &domain.Repeatability{Repeats: 3, Tasks: 10, MeanAccuracy: 0.6, SDAccuracy: 0.05, Flipped: 2}
		})

		h.do("run")

		assert.Contains(t, h.out.String(), "repeatability: mean accuracy 60.0% ± 5.0 pp over 3 repeats; 2 of 10 problems flipped")
	})
	t.Run("checks nobody could decide are not reported as passed", func(t *testing.T) {
		h := newRealHarness(t)
		h.hasRun(t, "r1")
		h.runner.outcome = finishedOutcome(func(o *service.Outcome) {
			o.Summary.UI.Undecided = []domain.CheckID{domain.U07, domain.P01}
		})

		code := h.do("run", "--resume", "r1")

		assert.Equal(t, exitOK, code, "an undecided check is not a failed one")
		assert.Contains(t, h.out.String(), "no required check failed; not decided: U-07 Scroll follow, P-01 Stop")
		assert.NotContains(t, h.out.String(), "all required checks passed")
	})
	t.Run("attempts the page broke are listed", func(t *testing.T) {
		h := newRealHarness(t)
		h.hasRun(t, "r1")
		h.runner.outcome = finishedOutcome(func(o *service.Outcome) {
			o.Summary.UI.UIErrors = map[string]int{"start_chat": 2, "submit": 1}
		})

		h.do("run", "--resume", "r1")

		assert.Contains(t, h.out.String(), "the page broke 3 attempts: start_chat 2 · submit 1")
		assert.NotContains(t, h.out.String(), "all required checks passed")
	})
	t.Run("a probe run asks no problems, and that is what it is for", func(t *testing.T) {
		out := finishedOutcome(func(o *service.Outcome) {
			o.Meta.Command = "probe"
			o.Summary = report.Aggregate(report.Input{RunID: "r1", Probes: []domain.ProbeOutcome{{ID: domain.P01, Level: domain.Must, Result: domain.Pass}}})
		})

		assert.NoError(t, judge(out, verdictRules{}, "r1"))
	})
	t.Run("nothing was asked", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome(func(o *service.Outcome) { o.Summary = report.Aggregate(report.Input{RunID: "r1"}) })

		h.do("run")

		assert.Contains(t, h.out.String(), "accuracy of those asked:   n/a")
	})
}

// --- selftest -------------------------------------------------------------------------

// matchingSelftestSummary builds the summary a correct evaluator finds.
func matchingSelftestSummary(t *testing.T, mutate ...func(*domain.Summary)) domain.Summary {
	t.Helper()
	want, err := synthetic.Expected()
	require.NoError(t, err)
	a := domain.AgentScorecard{
		Eligible: want.Eligible, Attempted: want.Attempted, Correct: want.Correct,
		Statuses:         map[domain.AttemptStatus]int{},
		FormatCompliance: domain.Proportion{Successes: want.FormatCompliance.Successes, Total: want.FormatCompliance.Total},
		Tokens:           domain.TokenStats{ToolCallsTotal: want.ToolCalls},
	}
	maps.Copy(a.Statuses, want.Statuses)
	for _, l := range want.Levels {
		a.Levels = append(a.Levels, domain.LevelScore{Level: l.Level, Eligible: l.Eligible, Attempted: l.Attempted, Correct: l.Correct})
	}
	s := domain.Summary{RunID: "selftest", Agent: a}
	for _, m := range mutate {
		m(&s)
	}
	return s
}

func TestSelftestCommand(t *testing.T) {
	t.Run("asks the synthetic problems of a scripted agent", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome(func(o *service.Outcome) {
			o.Meta.Command = "selftest"
			o.Summary = matchingSelftestSummary(t)
		})

		code := h.do("selftest")

		require.Equal(t, exitOK, code, h.errOut.String())
		req := h.runner.req
		assert.Regexp(t, `^selftest-20261002T161732Z-[0-9a-f]{6}$`, req.RunID)
		assert.Equal(t, "selftest", req.Command)
		assert.Equal(t, service.Filter{Attachments: domain.AttachmentsSkip}, req.Filter, "whatever GAIA_ATTACHMENTS says")
		assert.Equal(t, [3]any{1, selftestWorkers, run.SelftestTimeout}, [3]any{req.Repeats, req.Workers, req.Timeout})
		assert.False(t, req.IncludeText)
		assert.True(t, req.Screenshots, "the problems are invented, so the pictures are kept")
		count := 0
		for _, err := range h.runner.setup.source.Problems(t.Context()) {
			require.NoError(t, err)
			count++
		}
		assert.Equal(t, 13, count)
	})
	t.Run("different results fail it, with the differences", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome(func(o *service.Outcome) {
			o.Summary = matchingSelftestSummary(t, func(s *domain.Summary) { s.Agent.Correct = 7; s.Agent.Statuses[domain.StatusCorrect] = 7 })
		})

		code := h.do("selftest")

		assert.Equal(t, exitAccuracy, code)
		assert.Contains(t, h.errOut.String(), "found different results from the expected ones")
		assert.Contains(t, h.errOut.String(), "correct: got 7, want 8")
		assert.Contains(t, h.errOut.String(), "status correct: got 7, want 8")
		assert.NotContains(t, h.errOut.String(), "not running the self-test script", "some scripted outcomes did turn up")
	})
	t.Run("an agent that is not running the script is the likely cause when none of its outcomes turn up", func(t *testing.T) {
		for name, statuses := range map[string]map[domain.AttemptStatus]int{
			"an echo with a marker in the question":  {domain.StatusIncorrect: 12, domain.StatusSkipped: 1},
			"replies without any marker at all":      {domain.StatusNoFinalAnswer: 12, domain.StatusSkipped: 1},
			"a mixture of wrong answers and no mark": {domain.StatusIncorrect: 6, domain.StatusNoFinalAnswer: 6, domain.StatusSkipped: 1},
		} {
			h := newRealHarness(t)
			h.runner.outcome = finishedOutcome(func(o *service.Outcome) {
				o.Summary = matchingSelftestSummary(t, func(s *domain.Summary) { s.Agent.Statuses = statuses; s.Agent.Correct = 0 })
			})

			code := h.do("selftest")

			assert.Equal(t, exitAccuracy, code, name)
			assert.Contains(t, h.errOut.String(), "the agent is probably\nnot running the self-test script", name)
			assert.Contains(t, h.errOut.String(), "--profile selftest", name)
			assert.Contains(t, h.errOut.String(), "nothing else is listening on the Web UI's port", name)
		}
	})
	t.Run("a required UI check failing is the louder failure", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome(func(o *service.Outcome) {
			o.Summary = matchingSelftestSummary(t, func(s *domain.Summary) { s.UI.MustFailed = []domain.CheckID{domain.U05} })
		})

		code := h.do("selftest")

		assert.Equal(t, exitUIFailed, code)
	})
	t.Run("needs a user", func(t *testing.T) {
		h := newRealHarness(t)
		delete(h.env, "EVAL_USER_EMAIL")
		delete(h.env, "EVAL_USER_PASSWORD")

		assert.Equal(t, exitUsage, h.do("selftest"))
	})
	t.Run("results that git would pick up are refused", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git is not installed")
		}
		repo := t.TempDir()
		require.NoError(t, exec.Command("git", "-C", repo, "init", "-q").Run())
		h := newRealHarness(t)
		h.env["RESULTS_DIR"] = filepath.Join(repo, "results")

		code := h.do("selftest")

		assert.Equal(t, exitUsage, code)
		assert.Contains(t, h.errOut.String(), "not ignored")
		assert.False(t, h.runner.called)
	})
}

// --- datasets and report --------------------------------------------------------------

type gaiaRow struct {
	TaskID      string `parquet:"task_id"`
	Question    string `parquet:"Question"`
	Level       string `parquet:"Level"`
	FinalAnswer string `parquet:"Final answer"`
	FileName    string `parquet:"file_name"`
}

func writeGAIA(t *testing.T, dir, config, split string, rows []gaiaRow) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, config, split), 0o755))
	var buf bytes.Buffer
	w := parquet.NewGenericWriter[gaiaRow](&buf)
	_, err := w.Write(rows)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, os.WriteFile(filepath.Join(dir, config, split, "0000.parquet"), buf.Bytes(), 0o644))
}

func TestDatasetsList(t *testing.T) {
	t.Run("nothing downloaded", func(t *testing.T) {
		h := newRealHarness(t)

		code := h.do("datasets", "list")

		assert.Equal(t, exitOK, code)
		assert.Contains(t, h.out.String(), "There are no GAIA files in")
		assert.Contains(t, h.out.String(), "agent-eval datasets fetch")
	})
	t.Run("what is there", func(t *testing.T) {
		h := newRealHarness(t)
		writeGAIA(t, h.env["HF_DATASETS_DIR"], "2023_level1", "validation", []gaiaRow{
			{TaskID: "a", Question: "Q a", Level: "1", FinalAnswer: "1"},
			{TaskID: "b", Question: "Q b", Level: "1", FinalAnswer: "2", FileName: "x.png"},
			{TaskID: "c", Question: "Q c", Level: "2", FinalAnswer: "3"},
		})

		code := h.do("datasets", "list")

		assert.Equal(t, exitOK, code, h.errOut.String())
		out := h.out.String()
		assert.Regexp(t, `2023_level1\s+validation\s+1\s+3\s+2\s+1\s+0\s+1\s+0`, out)
		assert.Contains(t, out, "Problems with an attachment are skipped")
	})
	t.Run("attachments included", func(t *testing.T) {
		h := newRealHarness(t)
		h.env["GAIA_ATTACHMENTS"] = "include"
		writeGAIA(t, h.env["HF_DATASETS_DIR"], "2023_level1", "validation", []gaiaRow{{TaskID: "a", Question: "Q", Level: "1", FinalAnswer: "1"}})

		h.do("datasets", "list")

		assert.NotContains(t, h.out.String(), "are skipped")
	})
}

func TestDatasetsFetch(t *testing.T) {
	t.Run("a token is needed, and nothing is sent without one", func(t *testing.T) {
		h := newRealHarness(t)

		code := h.do("datasets", "fetch")

		assert.Equal(t, exitUsage, code)
		assert.Contains(t, h.errOut.String(), "HF_TOKEN is not set")
	})
	t.Run("results that git would pick up are refused", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git is not installed")
		}
		repo := t.TempDir()
		require.NoError(t, exec.Command("git", "-C", repo, "init", "-q").Run())
		h := newRealHarness(t)
		h.env["HF_TOKEN"] = "hf_test_token_value"
		h.env["HF_DATASETS_DIR"] = filepath.Join(repo, "datasets")

		code := h.do("datasets", "fetch")

		assert.Equal(t, exitUsage, code)
		assert.Contains(t, h.errOut.String(), "not ignored")
	})
	t.Run("downloads what the listing names", func(t *testing.T) {
		parquetData := func() []byte {
			var buf bytes.Buffer
			w := parquet.NewGenericWriter[gaiaRow](&buf)
			_, err := w.Write([]gaiaRow{{TaskID: "a", Question: "Q", Level: "1", FinalAnswer: "1"}})
			require.NoError(t, err)
			require.NoError(t, w.Close())
			return buf.Bytes()
		}()
		var hub *httptest.Server
		hub = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer hf_test_token_value" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch r.URL.Path {
			case "/parquet":
				_ = json.MarshalWrite(w, map[string]any{"parquet_files": []map[string]any{{
					"config": "2023_level1", "split": "validation", "url": hub.URL + "/f", "filename": "0000.parquet", "size": len(parquetData),
				}}})
			case "/f":
				_, _ = w.Write(parquetData)
			}
		}))
		defer hub.Close()
		h := newRealHarness(t)
		h.env["HF_TOKEN"] = "hf_test_token_value"
		h.env["HF_DATASETS_SERVER_URL"], h.env["HF_HUB_URL"] = hub.URL, hub.URL

		code := h.do("datasets", "fetch")

		require.Equal(t, exitOK, code, h.errOut.String())
		assert.Regexp(t, `2023_level1\s+validation\s+0000\.parquet\s+\d+ bytes\s+downloaded`, h.out.String())
		assert.FileExists(t, filepath.Join(h.env["HF_DATASETS_DIR"], "2023_level1", "validation", "0000.parquet"))
		assert.NotContains(t, h.out.String()+h.errOut.String(), "hf_test_token_value", "the token is never printed")

		again := newRealHarness(t)
		again.env = h.env
		again.loadConfig = h.loadConfig
		assert.Equal(t, exitOK, again.do("datasets", "fetch"))
		assert.Contains(t, again.out.String(), "already there")
	})
	t.Run("the hub refuses", func(t *testing.T) {
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
		defer hub.Close()
		h := newRealHarness(t)
		h.env["HF_TOKEN"] = "hf_test_token_value"
		h.env["HF_DATASETS_SERVER_URL"], h.env["HF_HUB_URL"] = hub.URL, hub.URL

		code := h.do("datasets", "fetch")

		assert.Equal(t, exitInfra, code)
		assert.Contains(t, h.errOut.String(), "was refused")
		assert.NotContains(t, h.errOut.String(), "hf_test_token_value")
	})
}

func TestReport(t *testing.T) {
	newRun := func(t *testing.T, h *realHarness) {
		t.Helper()
		st := store.New(h.env["RESULTS_DIR"])
		meta := domain.RunMeta{RunID: "r1", Command: "run", StartedAt: fixedNow, FinishedAt: fixedNow.Add(time.Minute), Thresholds: domain.DefaultThresholds()}
		require.NoError(t, st.Begin(t.Context(), meta))
		require.NoError(t, st.Append(t.Context(), "r1", domain.Result{TaskID: "a", Level: 1, Attempt: 1, Status: domain.StatusCorrect}))
		require.NoError(t, st.Append(t.Context(), "r1", domain.Result{TaskID: "b", Level: 1, Attempt: 1, Status: domain.StatusIncorrect}))
	}

	t.Run("markdown by default, and the files are written again", func(t *testing.T) {
		h := newRealHarness(t)
		newRun(t, h)

		code := h.do("report", "r1")

		require.Equal(t, exitOK, code, h.errOut.String())
		assert.Contains(t, h.out.String(), "# agent-eval run r1")
		assert.Contains(t, h.out.String(), "| **All** | 2 | 2 | 1 | 50.0%")
		assert.FileExists(t, filepath.Join(h.env["RESULTS_DIR"], "r1", "summary.md"))
		assert.FileExists(t, filepath.Join(h.env["RESULTS_DIR"], "r1", "summary.json"))
	})
	t.Run("json", func(t *testing.T) {
		h := newRealHarness(t)
		newRun(t, h)

		code := h.do("report", "r1", "--format", "json")

		require.Equal(t, exitOK, code, h.errOut.String())
		var back domain.Summary
		require.NoError(t, json.Unmarshal(h.out.Bytes(), &back))
		assert.Equal(t, 2, back.Agent.Attempted)
		assert.Equal(t, "r1", back.RunID)
	})
	t.Run("a run that does not exist", func(t *testing.T) {
		h := newRealHarness(t)

		code := h.do("report", "ghost")

		assert.Equal(t, exitUsage, code)
		assert.Contains(t, h.errOut.String(), "no such run")
		assert.Contains(t, h.errOut.String(), "runs are kept under")
	})
	t.Run("a run id that is not a name", func(t *testing.T) {
		h := newRealHarness(t)

		assert.Equal(t, exitUsage, h.do("report", "../x"))
	})
	t.Run("a damaged results file", func(t *testing.T) {
		h := newRealHarness(t)
		newRun(t, h)
		path := filepath.Join(h.env["RESULTS_DIR"], "r1", "results.jsonl")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append([]byte("{broken}\n"), data...), 0o600))

		code := h.do("report", "r1")

		assert.Equal(t, exitInfra, code)
		assert.Regexp(t, regexp.MustCompile(`results\.jsonl line 1 is damaged`), h.errOut.String())
	})
}

func TestRealRunner(t *testing.T) {
	cfg, err := config.Load(func(k string) (string, bool) {
		return map[string]string{"EVAL_BASE_URL": "http://localhost:8300", "EVAL_TYPING_DELAY": "30ms"}[k], k == "EVAL_BASE_URL" || k == "EVAL_TYPING_DELAY"
	})
	require.NoError(t, err)

	r := realRunner(runnerSetup{cfg: cfg, source: synthetic.Problems(), store: store.New(t.TempDir()), humanTyping: true})

	runner, ok := r.(*run.Runner)
	require.True(t, ok)
	assert.Equal(t, cfg.Thresholds, runner.Thresholds)
	assert.Equal(t, "1", runner.Info.UIContract)
	assert.Equal(t, "1.10.1", runner.Info.Versions["chrome-devtools-mcp"])
	assert.NotNil(t, runner.Versions, "the versions the browser reports are added at the end")
	assert.NotNil(t, runner.Drivers)
}

func TestGitSHA(t *testing.T) {
	// Built by `go test` there is usually no VCS information; when there is, it
	// is a short hash, perhaps marked as modified.
	assert.Regexp(t, `^([0-9a-f]{1,12}(\+dirty)?)?$`, gitSHA())
}

// --- the probes --------------------------------------------------------------------------

func probedSummary(base domain.Summary, outcomes ...domain.ProbeOutcome) domain.Summary {
	if base.RunID == "" {
		base.RunID = "r1" // the reporter always names the run
	}
	base.UI.Probes = outcomes
	for _, o := range outcomes {
		if o.Result == domain.Fail && o.Level == domain.Must {
			base.UI.MustFailed = append(base.UI.MustFailed, o.ID)
		}
	}
	return base
}

func TestProbesAfterTheProblems(t *testing.T) {
	stopped := domain.ProbeOutcome{ID: domain.P01, Level: domain.Must, Result: domain.Pass, Detail: "stopped in 15 ms, 462 characters kept"}

	t.Run("a run asks them of the agent with the prompt from the settings", func(t *testing.T) {
		h := newRealHarness(t)
		h.env["EVAL_PROBE_PROMPT"] = "Write an essay about owls."
		h.runner.outcome = finishedOutcome()
		h.prober.summary = probedSummary(h.runner.outcome.Summary, stopped)

		code := h.do("run")

		require.Equal(t, exitOK, code, h.errOut.String())
		assert.True(t, h.prober.called)
		assert.Equal(t, "r1", h.prober.runID, "for the run that just finished")
		assert.Equal(t, "Write an essay about owls.", h.prober.setup.prompts.Long)
		assert.Equal(t, "Reply with the single word OK.", h.prober.setup.prompts.Short)
		assert.Empty(t, h.prober.setup.prompts.Flaky, "an agent of unknown kind cannot be told to fail once")
		assert.Contains(t, h.out.String(), "Probes\n")
		assert.Contains(t, h.out.String(), "P-01 Stop")
		assert.Contains(t, h.out.String(), "stopped in 15 ms, 462 characters kept")
	})
	t.Run("--no-probes leaves them out", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome()

		code := h.do("run", "--no-probes")

		require.Equal(t, exitOK, code, h.errOut.String())
		assert.False(t, h.prober.called)
		assert.NotContains(t, h.out.String(), "Probes\n")
	})
	t.Run("a probe that fails a required check fails the run", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome()
		h.prober.summary = probedSummary(h.runner.outcome.Summary, domain.ProbeOutcome{ID: domain.P03, Level: domain.Must, Result: domain.Fail, Detail: "below 90: mobile accessibility 71"})

		code := h.do("run")

		assert.Equal(t, exitUIFailed, code)
		assert.Contains(t, h.errOut.String(), "required UI checks failed: P-03")
		assert.Contains(t, h.out.String(), "FAILED: P-03 Lighthouse")
		assert.Contains(t, h.out.String(), "below 90: mobile accessibility 71")
	})
	t.Run("an interrupted run does not go on to the probes", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome(func(o *service.Outcome) { o.Interrupted = true })

		code := h.do("run")

		assert.Equal(t, exitInterrupted, code)
		assert.False(t, h.prober.called)
	})
	t.Run("interrupted during the probes, what they found is kept and shown", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome()
		h.prober.summary = probedSummary(h.runner.outcome.Summary, stopped)
		ctx, ctrlC := context.WithCancel(t.Context())
		h.prober.ctrlC, h.prober.err = ctrlC, context.Canceled

		code := h.run(ctx, []string{"run"})

		assert.Equal(t, exitInterrupted, code)
		assert.Contains(t, h.out.String(), "P-01 Stop")
		assert.Contains(t, h.out.String(), "interrupted")
	})
	t.Run("probes that cannot keep their outcomes stop the run", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome()
		h.prober.err = errors.New("keeping the probe outcomes: disk full")

		code := h.do("run")

		assert.Equal(t, exitInfra, code)
		assert.Contains(t, h.errOut.String(), "disk full")
	})
	t.Run("Ctrl-C and then a failure to keep what the probes found is a failure", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome()
		ctx, ctrlC := context.WithCancel(t.Context())
		h.prober.ctrlC, h.prober.err = ctrlC, errors.New("keeping the probe outcomes: disk full")

		code := h.run(ctx, []string{"run"})

		assert.Equal(t, exitInfra, code, "what finished is not saved, so the run is not merely interrupted")
		assert.Contains(t, h.errOut.String(), "disk full")
	})
	t.Run("a store that gives up on its own is not Ctrl-C", func(t *testing.T) {
		// Only the run's own context ending cuts a run short. A store or a
		// reporter that returns a deadline error of its own has failed, and the
		// outcomes it was asked to keep are not saved.
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome()
		h.prober.err = fmt.Errorf("keeping the probe outcomes: %w", context.DeadlineExceeded)

		code := h.do("run")

		assert.Equal(t, exitInfra, code)
		assert.Contains(t, h.errOut.String(), "keeping the probe outcomes")
		assert.NotContains(t, h.errOut.String(), "interrupted")
	})
	t.Run("the self-test uses the scripted agent's replies and runs them all", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome(func(o *service.Outcome) { o.Summary = matchingSelftestSummary(t) })
		h.prober.summary = probedSummary(matchingSelftestSummary(t), stopped)

		code := h.do("selftest")

		require.Equal(t, exitOK, code, h.errOut.String())
		assert.Equal(t, scriptedPrompts(), h.prober.setup.prompts)
		assert.Contains(t, h.prober.setup.prompts.Long, "[probe-long]")
		assert.Contains(t, h.prober.setup.prompts.Flaky, "[probe-flaky]")
	})
	t.Run("a long probe detail is kept to a line", func(t *testing.T) {
		h := newRealHarness(t)
		h.runner.outcome = finishedOutcome()
		h.prober.summary = probedSummary(h.runner.outcome.Summary, domain.ProbeOutcome{ID: domain.P04, Level: domain.Should, Result: domain.Fail, Detail: strings.Repeat("wide ", 100)})

		h.do("run")

		assert.Contains(t, h.out.String(), "…")
		assert.NotContains(t, h.out.String(), strings.Repeat("wide ", 30))
	})
}

func TestProbeCommand(t *testing.T) {
	passing := domain.ProbeOutcome{ID: domain.P01, Level: domain.Must, Result: domain.Pass, Detail: "stopped in 15 ms"}

	t.Run("a run of its own, begun and finished", func(t *testing.T) {
		h := newRealHarness(t)
		h.prober.summary = probedSummary(domain.Summary{RunID: "probe-x"}, passing)

		code := h.do("probe")

		require.Equal(t, exitOK, code, h.errOut.String())
		assert.True(t, h.prober.called)
		assert.Regexp(t, `^probe-20261002T161732Z-[0-9a-f]{6}$`, h.prober.runID)
		h.env["EVAL_PROBE_PROMPT"] = "Write an essay about owls."
		st := store.New(h.env["RESULTS_DIR"])
		meta, err := st.LoadMeta(t.Context(), h.prober.runID)
		require.NoError(t, err)
		assert.Equal(t, "probe", meta.Command)
		assert.Equal(t, fixedNow, meta.FinishedAt.Add(0), "finished when the clock said")
		assert.Equal(t, domain.DefaultThresholds(), meta.Thresholds)
		assert.Equal(t, "1", meta.UIContract)
		assert.Contains(t, h.out.String(), "Run probe-")
		assert.Contains(t, h.out.String(), "P-01 Stop")
		assert.NotContains(t, h.out.String(), "\nAgent\n", "there were no problems")
	})
	t.Run("against the live agent unless told it is the scripted one", func(t *testing.T) {
		h := newRealHarness(t)
		h.prober.summary = probedSummary(domain.Summary{}, passing)

		require.Equal(t, exitOK, h.do("probe"))
		assert.Empty(t, h.prober.setup.prompts.Flaky)

		h = newRealHarness(t)
		h.prober.summary = probedSummary(domain.Summary{}, passing)
		require.Equal(t, exitOK, h.do("probe", "--scripted"))
		assert.Equal(t, scriptedPrompts(), h.prober.setup.prompts)
	})
	t.Run("a required check failing is exit 2", func(t *testing.T) {
		h := newRealHarness(t)
		h.prober.summary = probedSummary(domain.Summary{}, domain.ProbeOutcome{ID: domain.P02, Level: domain.Must, Result: domain.Fail, Detail: "the new reply was added"})

		code := h.do("probe")

		assert.Equal(t, exitUIFailed, code)
		assert.Contains(t, h.errOut.String(), "P-02")
	})
	t.Run("interrupted", func(t *testing.T) {
		h := newRealHarness(t)
		h.prober.summary = probedSummary(domain.Summary{}, passing)
		ctx, ctrlC := context.WithCancel(t.Context())
		h.prober.ctrlC, h.prober.err = ctrlC, context.Canceled

		code := h.run(ctx, []string{"probe"})

		assert.Equal(t, exitInterrupted, code)
		st := store.New(h.env["RESULTS_DIR"])
		meta, err := st.LoadMeta(t.Context(), h.prober.runID)
		require.NoError(t, err)
		assert.True(t, meta.FinishedAt.IsZero(), "not marked finished")
	})
	t.Run("a Web UI that is not up is found before a browser starts", func(t *testing.T) {
		h := newRealHarness(t)
		h.webStatus = http.StatusServiceUnavailable

		assert.Equal(t, exitInfra, h.do("probe"))
		assert.False(t, h.prober.called)
	})
	t.Run("a user is needed", func(t *testing.T) {
		h := newRealHarness(t)
		delete(h.env, "EVAL_USER_EMAIL")
		delete(h.env, "EVAL_USER_PASSWORD")

		assert.Equal(t, exitUsage, h.do("probe"))
	})
	t.Run("probes that fail to run at all", func(t *testing.T) {
		h := newRealHarness(t)
		h.prober.err = errors.New("starting a browser: no space left")

		code := h.do("probe")

		assert.Equal(t, exitInfra, code)
		assert.Contains(t, h.errOut.String(), "no space left")
	})
	t.Run("results that git would pick up are refused", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git is not installed")
		}
		repo := t.TempDir()
		require.NoError(t, exec.Command("git", "-C", repo, "init", "-q").Run())
		h := newRealHarness(t)
		h.env["RESULTS_DIR"] = filepath.Join(repo, "results")

		assert.Equal(t, exitUsage, h.do("probe"))
		assert.False(t, h.prober.called)
	})
}

func TestRealProber(t *testing.T) {
	cfg, err := config.Load(func(k string) (string, bool) {
		v := map[string]string{"EVAL_BASE_URL": "http://localhost:8300", "EVAL_USER_EMAIL": "a@b.c", "EVAL_USER_PASSWORD": "pw"}[k]
		return v, v != ""
	})
	require.NoError(t, err)

	p := realProber(proberSetup{cfg: cfg, store: store.New(t.TempDir()), prompts: scriptedPrompts()})

	suite, ok := p.(*probe.Suite)
	require.True(t, ok)
	assert.NotNil(t, suite.Probes)
	assert.NotNil(t, suite.Reporter)
}
