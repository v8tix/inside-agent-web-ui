package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/config"
)

// harness is a CLI whose configuration comes from a map and whose handlers
// record what they were given.
type harness struct {
	*cli
	out, err bytes.Buffer
	env      map[string]string
	got      struct {
		command string
		run     runOptions
		common  commonOptions
		probe   probeOptions
		report  reportOptions
		cfg     config.Config
	}
	result error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{env: map[string]string{}}
	h.cli = newCLI(&h.out, &h.err)
	h.loadConfig = func(string) (config.Config, error) {
		return config.Load(func(k string) (string, bool) { v, ok := h.env[k]; return v, ok })
	}
	h.runCmd = func(_ context.Context, _ *cli, cfg config.Config, o runOptions) error {
		h.got.command, h.got.run, h.got.cfg = "run", o, cfg
		return h.result
	}
	common := func(name string) handler[commonOptions] {
		return func(_ context.Context, _ *cli, cfg config.Config, o commonOptions) error {
			h.got.command, h.got.common, h.got.cfg = name, o, cfg
			return h.result
		}
	}
	h.selftestCmd, h.fetchCmd, h.listCmd = common("selftest"), common("datasets fetch"), common("datasets list")
	h.probeCmd = func(_ context.Context, _ *cli, cfg config.Config, o probeOptions) error {
		h.got.command, h.got.probe, h.got.cfg = "probe", o, cfg
		return h.result
	}
	h.reportCmd = func(_ context.Context, _ *cli, cfg config.Config, o reportOptions) error {
		h.got.command, h.got.report, h.got.cfg = "report", o, cfg
		return h.result
	}
	return h
}

func (h *harness) do(args ...string) int { return h.run(context.Background(), args) }

func TestHelpAndUsage(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  bool // usage text on stdout
		wantErr  string
	}{
		{"no arguments", nil, exitUsage, false, "Usage:"},
		{"help", []string{"help"}, exitOK, true, ""},
		{"-h", []string{"-h"}, exitOK, true, ""},
		{"--help", []string{"--help"}, exitOK, true, ""},
		{"help on a command", []string{"run", "--help"}, exitOK, true, ""},
		{"help on a nested command", []string{"datasets", "fetch", "-h"}, exitOK, true, ""},
		{"an unknown command", []string{"frobnicate"}, exitUsage, false, `unknown command "frobnicate"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			code := h.do(tc.args...)

			assert.Equal(t, tc.wantCode, code)
			assert.Equal(t, tc.wantOut, bytes.Contains(h.out.Bytes(), []byte("Commands:")))
			assert.Contains(t, h.err.String(), tc.wantErr)
			assert.Empty(t, h.got.command, "no command ran")
		})
	}
}

func TestRunFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want func(t *testing.T, o runOptions)
	}{
		{"defaults", []string{"run"}, func(t *testing.T, o runOptions) {
			assert.Equal(t, runOptions{EnvFile: ".env"}, o)
		}},
		{"everything", []string{
			"run", "--level", "2", "--limit", "10", "--task-id", "a", "--task-id", "b,c", "--repeats", "3",
			"--workers", "2", "--timeout", "90s", "--human-typing", "--resume", "run-1", "--include-text", "--no-probes",
			"--min-accuracy", "0.25", "--env", "other.env",
		}, func(t *testing.T, o runOptions) {
			assert.Equal(t, 2, o.Level)
			assert.Equal(t, 10, o.Limit)
			assert.Equal(t, []string{"a", "b", "c"}, o.TaskIDs)
			assert.Equal(t, 3, o.Repeats)
			assert.Equal(t, 2, o.Workers)
			assert.Equal(t, 90*time.Second, o.Timeout)
			assert.True(t, o.HumanTyping)
			assert.Equal(t, "run-1", o.Resume)
			assert.True(t, o.IncludeText)
			assert.True(t, o.NoProbes)
			assert.InDelta(t, 0.25, o.MinAccuracy, 1e-9)
			assert.Equal(t, "other.env", o.EnvFile)
		}},
		{"level all", []string{"run", "--level", "all"}, func(t *testing.T, o runOptions) { assert.Zero(t, o.Level) }},
		{"a repeated task id is kept once", []string{"run", "--task-id", "a", "--task-id", "a"}, func(t *testing.T, o runOptions) {
			assert.Equal(t, []string{"a"}, o.TaskIDs)
		}},
		{"single dash works too", []string{"run", "-limit", "4"}, func(t *testing.T, o runOptions) { assert.Equal(t, 4, o.Limit) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			code := h.do(tc.args...)

			require.Equal(t, exitOK, code, h.err.String())
			assert.Equal(t, "run", h.got.command)
			tc.want(t, h.got.run)
		})
	}
}

func TestRunFlags_Problems(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"a level out of range", []string{"run", "--level", "4"}, "--level must be 1, 2, 3 or all"},
		{"a level in words", []string{"run", "--level", "one"}, "--level must be"},
		{"a negative limit", []string{"run", "--limit", "-1"}, "--limit must not be negative"},
		{"no repeats", []string{"run", "--repeats", "-2"}, "--repeats and --workers"},
		{"a negative timeout", []string{"run", "--timeout", "-5s"}, "--timeout must be positive"},
		{"a bad timeout", []string{"run", "--timeout", "soon"}, "invalid value"},
		{"accuracy above one", []string{"run", "--min-accuracy", "1.5"}, "--min-accuracy must be between 0 and 1"},
		{"accuracy below zero", []string{"run", "--min-accuracy", "-0.1"}, "--min-accuracy must be between 0 and 1"},
		{"an unknown flag", []string{"run", "--turbo"}, "turbo"},
		{"a stray argument", []string{"run", "everything"}, `run takes no arguments (got "everything")`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			code := h.do(tc.args...)

			assert.Equal(t, exitUsage, code)
			assert.Contains(t, h.err.String(), tc.want)
			assert.Empty(t, h.got.command, "the command did not run")
		})
	}
}

func TestReportFlags(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantRun    string
		wantFormat string
		wantErr    string
	}{
		{"the id first", []string{"report", "run-7"}, exitOK, "run-7", "md", ""},
		{"flags after the id", []string{"report", "run-7", "--format", "json"}, exitOK, "run-7", "json", ""},
		{"flags before the id", []string{"report", "--format", "json", "run-7"}, exitOK, "run-7", "json", ""},
		{"no id", []string{"report"}, exitUsage, "", "", "needs the id of a run"},
		{"two ids", []string{"report", "a", "b"}, exitUsage, "", "", "needs the id of a run"},
		{"a bad format", []string{"report", "run-7", "--format", "html"}, exitUsage, "", "", `--format must be md or json (got "html")`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			code := h.do(tc.args...)

			assert.Equal(t, tc.wantCode, code)
			assert.Contains(t, h.err.String(), tc.wantErr)
			if tc.wantCode == exitOK {
				assert.Equal(t, tc.wantRun, h.got.report.RunID)
				assert.Equal(t, tc.wantFormat, h.got.report.Format)
			}
		})
	}
}

func TestOtherCommands(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr string
	}{
		{"selftest", []string{"selftest"}, "selftest", ""},
		{"probe", []string{"probe", "--env", "x.env"}, "probe", ""},
		{"datasets fetch", []string{"datasets", "fetch"}, "datasets fetch", ""},
		{"datasets list", []string{"datasets", "list"}, "datasets list", ""},
		{"datasets alone", []string{"datasets"}, "", "datasets needs a subcommand"},
		{"datasets with a flag first", []string{"datasets", "--env", "x"}, "", "datasets needs a subcommand"},
		{"an unknown datasets subcommand", []string{"datasets", "delete"}, "", `unknown datasets subcommand "delete"`},
		{"selftest with an argument", []string{"selftest", "now"}, "", `selftest takes no arguments (got "now")`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			code := h.do(tc.args...)

			assert.Equal(t, tc.want, h.got.command)
			if tc.wantErr != "" {
				assert.Equal(t, exitUsage, code)
				assert.Contains(t, h.err.String(), tc.wantErr)
				return
			}
			assert.Equal(t, exitOK, code, h.err.String())
		})
	}

	t.Run("the env flag reaches the handler", func(t *testing.T) {
		h := newHarness(t)
		h.do("selftest", "--env", "x.env")
		assert.Equal(t, "x.env", h.got.common.EnvFile)
	})
	t.Run("the probe flags", func(t *testing.T) {
		h := newHarness(t)
		assert.Equal(t, exitOK, h.do("probe", "--scripted", "--env", "x.env"), h.err.String())
		assert.Equal(t, probeOptions{EnvFile: "x.env", Scripted: true}, h.got.probe)

		h = newHarness(t)
		assert.Equal(t, exitOK, h.do("probe"), h.err.String())
		assert.False(t, h.got.probe.Scripted)

		h = newHarness(t)
		assert.Equal(t, exitUsage, h.do("probe", "now"))
		assert.Contains(t, h.err.String(), `probe takes no arguments (got "now")`)

		h = newHarness(t)
		assert.Equal(t, exitUsage, h.do("probe", "--fast"))
		assert.Contains(t, h.err.String(), "unknown flag")
	})
}

func TestConfiguration(t *testing.T) {
	t.Run("a bad configuration stops every command with exit 1, naming each problem", func(t *testing.T) {
		for _, args := range [][]string{{"run"}, {"selftest"}, {"probe"}, {"report", "x"}, {"datasets", "fetch"}, {"datasets", "list"}} {
			h := newHarness(t)
			h.env["EVAL_WORKERS"] = "zero"
			h.env["GAIA_SPLIT"] = "train"

			code := h.do(args...)

			assert.Equal(t, exitUsage, code, args)
			assert.Contains(t, h.err.String(), "configuration:", args)
			assert.Contains(t, h.err.String(), "EVAL_WORKERS", args)
			assert.Contains(t, h.err.String(), "GAIA_SPLIT", args)
			assert.Empty(t, h.got.command, "nothing ran", args)
		}
	})

	t.Run("an unreadable settings file is a configuration problem", func(t *testing.T) {
		h := newHarness(t)
		h.loadConfig = func(string) (config.Config, error) {
			return config.Config{}, errors.New("reading .env: permission denied")
		}

		code := h.do("selftest")

		assert.Equal(t, exitUsage, code)
		assert.Contains(t, h.err.String(), "permission denied")
	})

	// A file that --env names has to be there, as it has for the scripts and for
	// agent-web-ui: with no file behind the name the settings would be the
	// environment's alone, and nobody would be told. They end with status 2 for
	// it, and so does agent-eval.
	t.Run("a settings file that is named and is not there stops every command", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nothing.env")
		for _, args := range [][]string{{"run"}, {"selftest"}, {"probe"}, {"report", "x"}, {"datasets", "fetch"}, {"datasets", "list"}} {
			h := newHarness(t)
			h.loadConfig = loadConfigFile // the settings as the program reads them

			code := h.do(append(args, "--env", missing)...)

			assert.Equal(t, 2, code, "%v", args)
			assert.Equal(t, fmt.Sprintf("agent-eval: no settings file %q\n\nTry: agent-eval help\n", missing), h.err.String(), "%v", args)
			assert.Empty(t, h.got.command, "nothing ran: %v", args)
		}
	})

	t.Run("and so does a name that is empty", func(t *testing.T) {
		h := newHarness(t)
		h.loadConfig = loadConfigFile

		code := h.do("selftest", "--env", "")

		assert.Equal(t, 2, code)
		assert.Contains(t, h.err.String(), `agent-eval: no settings file ""`)
		assert.Empty(t, h.got.command, "nothing ran")
	})

	t.Run("a settings file that is named and is there is read", func(t *testing.T) {
		named := filepath.Join(t.TempDir(), "other.env")
		require.NoError(t, os.WriteFile(named, []byte("EVAL_BASE_URL=http://named.example:8300\n"), 0o600))
		settingsOfTheShellAside(t)
		h := newHarness(t)
		h.loadConfig = loadConfigFile

		code := h.do("selftest", "--env", named)

		require.Equal(t, exitOK, code, h.err.String())
		assert.Equal(t, "http://named.example:8300", h.got.cfg.Eval.BaseURL)
	})

	// The file that is not named, .env, may be missing: the settings are then the
	// environment's, as they are in a container.
	t.Run("but .env may be missing", func(t *testing.T) {
		t.Chdir(t.TempDir())
		settingsOfTheShellAside(t)
		for _, args := range [][]string{{"selftest"}, {"selftest", "--env", ".env"}} {
			h := newHarness(t)
			h.loadConfig = loadConfigFile

			code := h.do(args...)

			assert.Equal(t, exitOK, code, "%v: %s", args, h.err.String())
			assert.Equal(t, "selftest", h.got.command, "%v", args)
		}
	})

	t.Run("warnings are shown and the command goes on", func(t *testing.T) {
		h := newHarness(t)
		h.env["GAIA_SPLIT"] = "test"

		code := h.do("probe")

		assert.Equal(t, exitOK, code)
		assert.Contains(t, h.err.String(), "warning: GAIA_SPLIT=test has no published answers")
		assert.Equal(t, "probe", h.got.command)
	})

	t.Run("the handler gets the configuration", func(t *testing.T) {
		h := newHarness(t)
		h.env["EVAL_BASE_URL"] = "http://example.test:9"

		h.do("selftest")

		assert.Equal(t, "http://example.test:9", h.got.cfg.Eval.BaseURL)
	})
}

// settingsOfTheShellAside empties, for the length of a test, every variable
// that the loader of the settings asks for. An empty value counts as unset, so
// a test that reads the settings as the program does gets the file it names
// and the defaults, whatever the shell it runs in exports.
func settingsOfTheShellAside(t *testing.T) {
	t.Helper()
	var asked []string
	_, _ = config.Load(func(key string) (string, bool) {
		asked = append(asked, key)
		return "", false
	})
	require.NotEmpty(t, asked, "the loader asked for its settings")
	for _, key := range asked {
		t.Setenv(key, "")
	}
}

type wrapped struct{ err error }

func (w *wrapped) Error() string { return "wrapped: " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

func TestExitCodes(t *testing.T) {
	tests := []struct {
		name    string
		result  error
		want    int
		wantErr string
	}{
		{"fine", nil, exitOK, ""},
		{"a required UI check failed", fail(exitUIFailed, "U-01 failed"), exitUIFailed, "U-01 failed"},
		{"accuracy below the minimum", fail(exitAccuracy, "accuracy 0.20 is below 0.50"), exitAccuracy, "below 0.50"},
		{"the handler's own usage complaint", usageError{msg: "nothing to do"}, exitUsage, "nothing to do"},
		{"anything else is an infrastructure failure", errors.New("chrome crashed"), exitInfra, "chrome crashed"},
		{"a wrapped exit code is found", &wrapped{fail(exitUIFailed, "inner")}, exitUIFailed, "inner"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.result = tc.result

			code := h.do("selftest")

			assert.Equal(t, tc.want, code)
			assert.Contains(t, h.err.String(), tc.wantErr)
		})
	}
}

func TestWarnings(t *testing.T) {
	const timeoutWarning = "EVAL_TASK_TIMEOUT (20m0s) is longer than AGENT_REQUEST_TIMEOUT (10m0s)"
	const splitWarning = "GAIA_SPLIT=test has no published answers"
	env := map[string]string{"EVAL_TASK_TIMEOUT": "20m", "AGENT_REQUEST_TIMEOUT": "10m", "GAIA_SPLIT": "test"}

	tests := []struct {
		name          string
		args          []string
		wantTimeout   bool
		wantTestSplit bool
	}{
		{"run says both", []string{"run"}, true, true},
		{"selftest has its own timeouts", []string{"selftest"}, false, true},
		{"probe has its own timeouts", []string{"probe"}, false, true},
		{"report asks nothing of the agent", []string{"report", "run-1"}, false, true},
		{"datasets list", []string{"datasets", "list"}, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.env = env

			require.Equal(t, exitOK, h.do(tc.args...), h.err.String())

			assert.Equal(t, tc.wantTimeout, bytes.Contains(h.err.Bytes(), []byte(timeoutWarning)), h.err.String())
			assert.Equal(t, tc.wantTestSplit, bytes.Contains(h.err.Bytes(), []byte(splitWarning)), h.err.String())
		})
	}
}
