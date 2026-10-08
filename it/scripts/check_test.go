package scripts_test

import (
	"bytes"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gateRig is a copy of scripts/check.sh and its helper in a directory of their
// own, with settings of its own and a stand-in for every program the gate starts:
// go, gofmt and the four tools. A stand-in writes down how it was called and
// answers as the test says, so a test sees which steps ran, in which order, and
// what the gate makes of what they print. The gate works in the directory it sits
// in, so the copy never reaches the real tools, the real code or the real tests,
// and the temporary files it makes stay in the rig.
type gateRig struct {
	t       *testing.T
	dir     string
	answers []string
	env     []string // more of the environment the gate runs in, as NAME=value
}

// gateTools are the programs of the gate that the rig stands in for, and
// gateSystemTools the ones of the machine that the gate and the stand-ins call by
// name. The path of the rig holds these and nothing else, so that a linter the
// machine has cannot stand in for one the rig lacks.
var (
	gateTools       = []string{"go", "gofmt", "golangci-lint", "staticcheck", "modernize", "govulncheck", "mktemp"}
	gateSystemTools = []string{"bash", "env", "awk", "sed", "grep", "sort", "head", "cat", "cp", "rm", "dirname", "basename"}
)

// gateStandIn is every program of the rig. It adds its name and arguments to the
// file calls and writes the rig's coverage profile where -coverprofile says. Then
// it looks for the first line of the file answers whose text is in its command
// line, prints the output that line names and ends with its status. With no such
// line it prints nothing and succeeds. Under the name mktemp it is the system's
// mktemp, told to make its file in the rig: on macOS mktemp asks the system for a
// directory before it looks at TMPDIR.
const gateStandIn = `#!/usr/bin/env bash
rig="$(cd "$(dirname "$0")/.." && pwd)"
name="$(basename "$0")"
if [ "$name" = mktemp ]; then
  PATH=/usr/bin:/bin exec mktemp "$rig/tmp/tmp.XXXXXXXX"
fi
call="$name $*"
echo "$call" >>"$rig/calls"
for arg in "$@"; do
  case "$arg" in -coverprofile=*) cp "$rig/profile" "${arg#-coverprofile=}" ;; esac
done
while IFS=$'\t' read -r text status output; do
  case "$call" in *"$text"*) cat "$output"; exit "$status" ;; esac
done <"$rig/answers"
exit 0
`

// gateSettings are the settings of the rig: the floors .env.example has, and two
// packages that are not held to the floor of a package.
const gateSettings = `TOOLS_DIR=tools
COVER_CORE_MIN=85
COVER_ALL_MIN=70
COVER_PKG_MIN=50
COVER_EXEMPT="internal/app cmd/web"
`

// The three kinds of file the floors tell apart.
const (
	gateUseCase     = "internal/core/chat/stream.go"
	gateEvalUseCase = "internal/eval/core/run/batch.go"
	gateAdapter     = "internal/infrastructure/config/config.go"
)

// newGateRig makes a rig whose programs all succeed and whose unit tests cover
// enough for every floor.
func newGateRig(t *testing.T) *gateRig {
	t.Helper()
	r := &gateRig{t: t, dir: t.TempDir()}
	for _, file := range []string{"scripts/check.sh", "scripts/lib/process.sh"} {
		src, err := os.ReadFile(filepath.Join("..", "..", file))
		require.NoError(t, err)
		r.write(file, string(src), 0o700)
	}
	// One file under every name: it reads its name from how it was called.
	r.write("stand-in", gateStandIn, 0o700)
	require.NoError(t, os.Mkdir(filepath.Join(r.dir, "tools"), 0o700))
	for _, tool := range gateTools {
		require.NoError(t, os.Symlink(filepath.Join(r.dir, "stand-in"), filepath.Join(r.dir, "tools", tool)))
	}
	require.NoError(t, os.Mkdir(filepath.Join(r.dir, "system"), 0o700))
	for _, tool := range gateSystemTools {
		path, err := exec.LookPath(tool)
		require.NoError(t, err)
		require.NoError(t, os.Symlink(path, filepath.Join(r.dir, "system", tool)))
	}
	r.write(".env.example", gateSettings, 0o600)
	r.write("answers", "", 0o600)
	require.NoError(t, os.Mkdir(filepath.Join(r.dir, "tmp"), 0o700))
	r.cover(gateCover{gateUseCase, 90, 100}, gateCover{gateEvalUseCase, 90, 100}, gateCover{gateAdapter, 70, 100})
	return r
}

// write puts a file in the rig, with the directories above it.
func (r *gateRig) write(name, content string, mode os.FileMode) {
	r.t.Helper()
	path := filepath.Join(r.dir, name)
	require.NoError(r.t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(r.t, os.WriteFile(path, []byte(content), mode))
}

// answer makes the stand-in whose command line holds text print output and end
// with status. A call gets the first answer that fits it.
func (r *gateRig) answer(text, output string, status int) {
	r.t.Helper()
	file := "output-" + strconv.Itoa(len(r.answers))
	r.write(file, output, 0o600)
	r.answers = append(r.answers, text+"\t"+strconv.Itoa(status)+"\t"+filepath.Join(r.dir, file))
	r.write("answers", strings.Join(r.answers, "\n")+"\n", 0o600)
}

// gateCover is how much of a file the unit tests ran: covered of its statements.
type gateCover struct {
	file                string
	covered, statements int
}

// cover sets the coverage profile the unit tests of the rig write: for each file a
// block that ran and a block that did not.
func (r *gateRig) cover(files ...gateCover) {
	r.t.Helper()
	var profile strings.Builder
	profile.WriteString("mode: atomic\n")
	for _, f := range files {
		name := "github.com/v8tix/inside-agent-web-ui/" + f.file
		fmt.Fprintf(&profile, "%s:1.1,2.2 %d 1\n", name, f.covered)
		fmt.Fprintf(&profile, "%s:3.1,4.2 %d 0\n", name, f.statements-f.covered)
	}
	r.write("profile", profile.String(), 0o600)
}

// run runs the gate of the rig, and returns all it printed and its exit status.
func (r *gateRig) run(args ...string) (output string, exit int) {
	r.t.Helper()
	cmd := exec.CommandContext(r.t.Context(), "bash", append([]string{filepath.Join(r.dir, "scripts", "check.sh")}, args...)...)
	cmd.Dir = r.dir
	// The path is the rig's, and so are the names under which a developer's shell
	// could hand the gate other settings files or a file to read at its start.
	cmd.Env = append(append(withoutSecrets(os.Environ()),
		"PATH="+filepath.Join(r.dir, "system"), "ENV_FILE=.env", "ENV_EXAMPLE=.env.example", "BASH_ENV="), r.env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return out.String(), exitErr.ExitCode()
	}
	require.NoError(r.t, err)
	return out.String(), 0
}

// gateProfileArg is where the unit tests are told to write the coverage profile: a
// temporary file, with another name on every run.
var gateProfileArg = regexp.MustCompile(`-coverprofile=\S+`)

// calls are the command lines the gate ran, in order.
func (r *gateRig) calls() []string {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, "calls"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil // nothing ran
	}
	require.NoError(r.t, err)
	return strings.Split(strings.TrimSpace(gateProfileArg.ReplaceAllString(string(data), "-coverprofile=FILE")), "\n")
}

// leftBehind are the temporary files of the gate that are still there.
func (r *gateRig) leftBehind() []string {
	r.t.Helper()
	entries, err := os.ReadDir(filepath.Join(r.dir, "tmp"))
	require.NoError(r.t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// gateHeadings are the steps the gate announced, in order.
func gateHeadings(output string) []string {
	var headings []string
	for line := range strings.SplitSeq(output, "\n") {
		if heading, ok := strings.CutPrefix(line, "== "); ok {
			headings = append(headings, heading)
		}
	}
	return headings
}

// The commands of the gate, in the order it runs them: the ones up to the coverage,
// and the tests under it/ that start no container, which a run without --it adds.
var (
	gateCalls = []string{
		"gofmt -l assets cmd internal it",
		"go vet ./...",
		"golangci-lint run ./...",
		"staticcheck ./...",
		"modernize ./...",
		"govulncheck ./...",
		"go test -count=1 -race -covermode=atomic -coverprofile=FILE ./internal/... ./cmd/...",
	}
	gateDockerFreeCalls = []string{
		"go test -count=1 -v ./it/scripts/",
		"go test -count=1 -v -run ^TestRoutes_ ./it/security/",
	}
)

func TestCheck_RunsItsStepsInOrder(t *testing.T) {
	r := newGateRig(t)

	out, exit := r.run()

	require.Zero(t, exit, out)
	assert.Equal(t, slices.Concat(gateCalls, gateDockerFreeCalls), r.calls())
	assert.Equal(t, []string{"gofmt", "go vet", "golangci-lint", "staticcheck", "modernize", "govulncheck", "unit tests", "coverage", "tests that need no Docker"}, gateHeadings(out))
	assert.True(t, strings.HasSuffix(out, "\nAll checks passed.\n"), out)
	assert.Empty(t, r.leftBehind(), "the coverage profile is gone when the gate ends")
}

func TestCheck_QuickLeavesOutTheRaceDetectorAndTheScan(t *testing.T) {
	r := newGateRig(t)

	out, exit := r.run("--quick")

	require.Zero(t, exit, out)
	assert.Equal(t, slices.Concat([]string{
		"gofmt -l assets cmd internal it",
		"go vet ./...",
		"golangci-lint run ./...",
		"staticcheck ./...",
		"modernize ./...",
		"go test -count=1 -covermode=atomic -coverprofile=FILE ./internal/... ./cmd/...",
	}, gateDockerFreeCalls), r.calls(), "the other steps are the same")
	assert.NotContains(t, gateHeadings(out), "govulncheck")
	assert.True(t, strings.HasSuffix(out, "\nThe quick checks passed (the race detector and the vulnerability scan were left out).\n"), out)
	assert.NotContains(t, out, "All checks passed", "a quick run does not say that everything was checked")
}

// The tools come from TOOLS_DIR, which scripts/build-tools.sh fills. One that is
// not there is said before the first step, not found out after the others ran.
func TestCheck_AMissingToolStopsIt(t *testing.T) {
	for _, tool := range []string{"golangci-lint", "staticcheck", "modernize", "govulncheck"} {
		t.Run(tool, func(t *testing.T) {
			r := newGateRig(t)
			require.NoError(t, os.Remove(filepath.Join(r.dir, "tools", tool)))

			out, exit := r.run()

			assert.Equal(t, 1, exit, out)
			assert.Contains(t, out, "check.sh: "+tool+" is not installed: run scripts/build-tools.sh")
			assert.Empty(t, r.calls(), "no step ran")
		})
	}
}

func TestCheck_AStepThatFailsStopsIt(t *testing.T) {
	tests := []struct {
		name   string
		step   string // a text in the command line of the step that goes wrong
		output string // what the program prints
		status int    // and how it ends
		says   string // what the gate adds of its own, if anything
		last   string // the last command that ran
	}{
		{"a file that is not formatted", "gofmt", "internal/app/app.go\n", 0,
			"check.sh: not formatted (run gofmt -w):\ninternal/app/app.go\n", "gofmt -l assets cmd internal it"},
		{"go vet", "go vet", "internal/app/app.go:9:2: unreachable code\n", 1, "", "go vet ./..."},
		{"golangci-lint", "golangci-lint", "internal/app/app.go:9:2: Error return value is not checked (errcheck)\n", 1, "", "golangci-lint run ./..."},
		{"staticcheck", "staticcheck", "internal/app/app.go:9:2: this value is never used (SA4006)\n", 1, "", "staticcheck ./..."},
		{"a suggestion of modernize", "modernize", "internal/app/app.go:9:2: for loop can be modernized using range over int\n", 3,
			"check.sh: modernize has suggestions:\ninternal/app/app.go:9:2: for loop can be modernized using range over int\n", "modernize ./..."},
		{"a vulnerability", "govulncheck", "Vulnerability #1: GO-2026-0001\n", 3, "", "govulncheck ./..."},
		{"a unit test", "go test", "--- FAIL: TestServe (0.00s)\nFAIL\n", 1, "",
			"go test -count=1 -race -covermode=atomic -coverprofile=FILE ./internal/... ./cmd/..."},
		{"a test of the scripts", "./it/scripts/", "--- FAIL: TestGitignore (0.01s)\nFAIL\n", 1,
			"check.sh: the tests that need no Docker failed: all of their output is above\n", "go test -count=1 -v ./it/scripts/"},
		{"the test of the table of routes", "./it/security/", "--- FAIL: TestRoutes_TheTableIsComplete (0.00s)\nFAIL\n", 1,
			"check.sh: the tests that need no Docker failed: all of their output is above\n", "go test -count=1 -v -run ^TestRoutes_ ./it/security/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newGateRig(t)
			r.answer(tc.step, tc.output, tc.status)

			out, exit := r.run()

			assert.NotZero(t, exit, out)
			assert.Contains(t, out, tc.output, "what the program found is shown")
			assert.Contains(t, out, tc.says)
			calls := r.calls()
			require.NotEmpty(t, calls)
			assert.Equal(t, tc.last, calls[len(calls)-1], "nothing runs after the step that failed")
			assert.NotContains(t, out, "checks passed")
			assert.Empty(t, r.leftBehind())
		})
	}
}

// The floors are counted from the profile the unit tests write: the use cases, the
// whole of internal/ and cmd/, and each package on its own. The generated mocks
// count nowhere, and a package named in COVER_EXEMPT is not held to the third.
func TestCheck_CoverageFloors(t *testing.T) {
	const (
		useCases   = "use cases (internal/core, internal/eval/core):  %s%% (target 85%%)\n"
		everything = "everything under internal/ and cmd/, no mocks: %s%% (target 70%%)\n"
		packages   = "every package is at 50% or above, except the ones named in COVER_EXEMPT\n"
	)
	tests := []struct {
		name  string
		cover []gateCover
		exit  int
		says  []string
	}{
		{"every floor is met",
			[]gateCover{{gateUseCase, 90, 100}, {gateEvalUseCase, 90, 100}, {gateAdapter, 70, 100}, {"cmd/eval/main.go", 60, 100}}, 0,
			[]string{fmt.Sprintf(useCases, "90.0"), fmt.Sprintf(everything, "77.5"), packages}},
		{"the use cases exactly at their floor",
			[]gateCover{{gateUseCase, 85, 100}, {gateAdapter, 100, 100}}, 0,
			[]string{fmt.Sprintf(useCases, "85.0"), packages}},
		{"the use cases under their floor",
			[]gateCover{{gateUseCase, 84, 100}, {gateAdapter, 100, 100}}, 1,
			[]string{fmt.Sprintf(useCases, "84.0"), "check.sh: the use cases are below 85%\n"}},
		{"the evaluator's use cases are counted with the Web UI's",
			[]gateCover{{gateUseCase, 100, 100}, {gateEvalUseCase, 60, 100}, {gateAdapter, 100, 100}}, 1,
			[]string{fmt.Sprintf(useCases, "80.0"), "check.sh: the use cases are below 85%\n"}},
		{"everything exactly at its floor, and a package at its own",
			[]gateCover{{gateUseCase, 90, 100}, {gateAdapter, 50, 100}}, 0,
			[]string{fmt.Sprintf(everything, "70.0"), packages}},
		{"everything under its floor",
			[]gateCover{{gateUseCase, 90, 100}, {gateAdapter, 50, 100}, {"cmd/eval/main.go", 50, 100}}, 1,
			[]string{fmt.Sprintf(everything, "63.3"), "check.sh: internal/ and cmd/ are below 70%\n"}},
		{"a package under its floor while both totals are met",
			[]gateCover{{gateUseCase, 100, 100}, {gateAdapter, 100, 100}, {"internal/infrastructure/auth/argon2.go", 49, 100}}, 1,
			[]string{"check.sh: these packages are below 50% (add tests, or name the package in COVER_EXEMPT with a reason):\n  internal/infrastructure/auth: 49.0%\n"}},
		{"the files of a package are counted together",
			[]gateCover{{gateUseCase, 100, 100}, {"internal/infrastructure/auth/argon2.go", 0, 10}, {"internal/infrastructure/auth/session.go", 90, 90}}, 0,
			[]string{packages}},
		{"a package named in COVER_EXEMPT is not held to the floor of a package",
			[]gateCover{{gateUseCase, 100, 100}, {gateAdapter, 100, 100}, {"internal/app/app.go", 0, 50}}, 0,
			[]string{fmt.Sprintf(everything, "80.0"), packages}},
		{"but it counts in the whole",
			[]gateCover{{gateUseCase, 100, 100}, {"internal/app/app.go", 0, 100}}, 1,
			[]string{fmt.Sprintf(everything, "50.0"), "check.sh: internal/ and cmd/ are below 70%\n"}},
		{"the generated mocks are left out of all three",
			[]gateCover{{gateUseCase, 90, 100}, {gateAdapter, 90, 100}, {"internal/core/port/resource/mocks/message_store.go", 0, 1000}}, 0,
			[]string{fmt.Sprintf(useCases, "90.0"), fmt.Sprintf(everything, "90.0"), packages}},
		// A share is printed with one decimal. It is cut there, not rounded, and the
		// floor is compared with the share itself: 84.96% is not 85%.
		{"the use cases a little under their floor are under it",
			[]gateCover{{gateUseCase, 2124, 2500}, {gateAdapter, 100, 100}}, 1,
			[]string{fmt.Sprintf(useCases, "84.9"), "check.sh: the use cases are below 85%\n"}},
		{"everything a little under its floor is under it",
			[]gateCover{{gateUseCase, 900, 1000}, {gateAdapter, 849, 1500}}, 1,
			[]string{fmt.Sprintf(everything, "69.9"), "check.sh: internal/ and cmd/ are below 70%\n"}},
		{"a package a little under its floor is listed with the share it has",
			[]gateCover{{gateUseCase, 5000, 5000}, {"internal/infrastructure/auth/argon2.go", 1249, 2500}}, 1,
			[]string{"check.sh: these packages are below 50% (add tests, or name the package in COVER_EXEMPT with a reason):\n  internal/infrastructure/auth: 49.9%\n"}},
		{"a share a little over a floor meets it",
			[]gateCover{{gateUseCase, 2126, 2500}, {gateAdapter, 100, 100}}, 0,
			[]string{fmt.Sprintf(useCases, "85.0"), packages}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newGateRig(t)
			r.cover(tc.cover...)

			out, exit := r.run()

			assert.Equal(t, tc.exit, exit, out)
			for _, line := range tc.says {
				assert.Contains(t, out, line)
			}
			assert.Equal(t, tc.exit == 0, strings.Contains(out, "All checks passed."), out)
		})
	}
}

// The gate says the same on every machine. awk prints a number the way the locale
// of the machine writes it, with a decimal comma in some, so the gate tells it to
// leave the locale aside.
func TestCheck_TheCoverageReadsTheSameInEveryLocale(t *testing.T) {
	locale := procCommaLocale(t)
	if locale == "" {
		t.Skip("this machine has no locale with a decimal comma")
	}
	tests := []struct {
		name  string
		cover []gateCover
		says  string
	}{
		{"the totals", []gateCover{{gateUseCase, 2124, 2500}, {gateAdapter, 100, 100}},
			"use cases (internal/core, internal/eval/core):  84.9% (target 85%)\neverything under internal/ and cmd/, no mocks: 85.5% (target 70%)\ncheck.sh: the use cases are below 85%\n"},
		{"a package", []gateCover{{gateUseCase, 5000, 5000}, {"internal/infrastructure/auth/argon2.go", 1249, 2500}},
			"  internal/infrastructure/auth: 49.9%\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newGateRig(t)
			r.env = []string{"LC_ALL=" + locale}
			r.cover(tc.cover...)

			out, exit := r.run()

			assert.Equal(t, 1, exit, out)
			assert.Contains(t, out, tc.says)
		})
	}
}

// What go test -v prints for the tests under it/ that need no Docker, cut down to
// the kinds of line the gate reads.
const (
	gateScriptsPassed = `=== RUN   TestGitignore
--- PASS: TestGitignore (0.01s)
PASS
ok  	github.com/v8tix/inside-agent-web-ui/it/scripts	38.112s
`
	gateScriptsSkipped = `=== RUN   TestCheck_TheCoverageReadsTheSameInEveryLocale
    check_test.go:403: this machine has no locale with a decimal comma
--- SKIP: TestCheck_TheCoverageReadsTheSameInEveryLocale (0.00s)
=== RUN   TestRealAgent_BuildsAgainstTheADKItIsToldTo
=== RUN   TestRealAgent_BuildsAgainstTheADKItIsToldTo/the_sample's_own_pin_when_told_so
    real_agent_test.go:217: lsof is not installed, so the script cannot tell where the agent listens
--- PASS: TestRealAgent_BuildsAgainstTheADKItIsToldTo (0.00s)
    --- SKIP: TestRealAgent_BuildsAgainstTheADKItIsToldTo/the_sample's_own_pin_when_told_so (0.00s)
PASS
ok  	github.com/v8tix/inside-agent-web-ui/it/scripts	12.004s
`
	gateScriptsFailed = `=== RUN   TestGitignore
    repo_test.go:61: .env is not ignored: the settings of this machine would be committed
--- FAIL: TestGitignore (0.01s)
=== RUN   TestLoadEnv
--- PASS: TestLoadEnv (0.02s)
FAIL
FAIL	github.com/v8tix/inside-agent-web-ui/it/scripts	0.512s
FAIL
`
	gateRoutesPassed = `=== RUN   TestRoutes_TheTableIsComplete
--- PASS: TestRoutes_TheTableIsComplete (0.00s)
PASS
ok  	github.com/v8tix/inside-agent-web-ui/it/security	0.402s
`
)

// The tests under it/ that need no Docker run with -v, the only way a test that
// was skipped says so, and the gate prints of their output what a run without -v
// prints, and the tests that were skipped.
func TestCheck_TestsThatNeedNoDocker(t *testing.T) {
	t.Run("a line for each package is all that is printed", func(t *testing.T) {
		r := newGateRig(t)
		r.answer("./it/scripts/", gateScriptsPassed, 0)
		r.answer("./it/security/", gateRoutesPassed, 0)

		out, exit := r.run()

		require.Zero(t, exit, out)
		assert.Contains(t, out, "\n== tests that need no Docker\nok  \tgithub.com/v8tix/inside-agent-web-ui/it/scripts\t38.112s\nok  \tgithub.com/v8tix/inside-agent-web-ui/it/security\t0.402s\n\nAll checks passed.\n")
		assert.NotContains(t, out, "were skipped")
		assert.Empty(t, r.leftBehind(), "their output is gone when the gate ends")
	})
	t.Run("the tests that were skipped are counted and named", func(t *testing.T) {
		r := newGateRig(t)
		r.answer("./it/scripts/", gateScriptsSkipped, 0)
		r.answer("./it/security/", gateRoutesPassed, 0)

		out, exit := r.run()

		require.Zero(t, exit, out)
		assert.Contains(t, out, "\n== tests that need no Docker\nok  \tgithub.com/v8tix/inside-agent-web-ui/it/scripts\t12.004s\nok  \tgithub.com/v8tix/inside-agent-web-ui/it/security\t0.402s\n"+
			"2 tests that need no Docker were skipped:\n  --- SKIP: TestCheck_TheCoverageReadsTheSameInEveryLocale (0.00s)\n"+
			"  --- SKIP: TestRealAgent_BuildsAgainstTheADKItIsToldTo/the_sample's_own_pin_when_told_so (0.00s)\n\nAll checks passed.\n")
		assert.Empty(t, r.leftBehind())
	})
	t.Run("a run that failed shows all of its output, and the gate fails", func(t *testing.T) {
		r := newGateRig(t)
		r.answer("./it/scripts/", gateScriptsFailed, 1)

		out, exit := r.run()

		assert.Equal(t, 1, exit, out)
		assert.Contains(t, out, "\n== tests that need no Docker\n"+gateScriptsFailed+"check.sh: the tests that need no Docker failed: all of their output is above\n",
			"the reasons too: the job of a pull request keeps no file")
		assert.NotContains(t, out, "checks passed")
		assert.Empty(t, r.leftBehind())
	})
}

// What go test -v prints for the packages under it/, cut down to the kinds of line
// the gate reads.
const (
	gateITPassed = `=== RUN   TestAppSuite
=== RUN   TestAppSuite/TestFirstUser
--- PASS: TestAppSuite (1.20s)
    --- PASS: TestAppSuite/TestFirstUser (0.31s)
PASS
ok  	github.com/v8tix/inside-agent-web-ui/it/app	1.532s
?   	github.com/v8tix/inside-agent-web-ui/it/internal/stack	[no test files]
`
	gateITSkipped = `=== RUN   TestLiveSuite
    live_test.go:31: AGENT_LIVE is not 1 in .env
--- SKIP: TestLiveSuite (0.00s)
=== RUN   TestMeasureUIOverhead
=== RUN   TestMeasureUIOverhead/attempts
    measure_test.go:40: EVAL_MEASURE is 0 in .env
--- PASS: TestMeasureUIOverhead (0.00s)
    --- SKIP: TestMeasureUIOverhead/attempts (0.00s)
PASS
ok  	github.com/v8tix/inside-agent-web-ui/it/live	0.211s
`
	gateITFailed = `=== RUN   TestChatSuite
=== RUN   TestChatSuite/TestStop
    chat_test.go:88: the reply was not saved as stopped
--- FAIL: TestChatSuite (2.10s)
    --- FAIL: TestChatSuite/TestStop (0.40s)
FAIL
FAIL	github.com/v8tix/inside-agent-web-ui/it/chat	2.412s
=== RUN   TestAppSuite
--- PASS: TestAppSuite (1.20s)
PASS
ok  	github.com/v8tix/inside-agent-web-ui/it/app	1.532s
FAIL
`
)

func TestCheck_IntegrationTests(t *testing.T) {
	t.Run("they run last, and a line for each package is all that is printed", func(t *testing.T) {
		r := newGateRig(t)
		r.answer("./it/...", gateITPassed, 0)

		out, exit := r.run("--it")

		require.Zero(t, exit, out)
		assert.Equal(t, slices.Concat(gateCalls, []string{"go test -count=1 -v ./it/..."}), r.calls(),
			"the tests that need no Docker are among them, so they are not run a second time")
		assert.NotContains(t, gateHeadings(out), "tests that need no Docker")
		assert.Contains(t, out, "\n== integration tests\nok  \tgithub.com/v8tix/inside-agent-web-ui/it/app\t1.532s\n?   \tgithub.com/v8tix/inside-agent-web-ui/it/internal/stack\t[no test files]\n\nAll checks passed.\n")
		assert.NotContains(t, out, "were skipped")
		assert.Empty(t, r.leftBehind(), "their output is gone when they passed")
	})
	t.Run("the tests that were skipped are counted and named", func(t *testing.T) {
		r := newGateRig(t)
		r.answer("./it/...", gateITPassed+gateITSkipped, 0)

		out, exit := r.run("--it")

		require.Zero(t, exit, out)
		assert.Contains(t, out, "\n2 integration tests were skipped:\n  --- SKIP: TestLiveSuite (0.00s)\n  --- SKIP: TestMeasureUIOverhead/attempts (0.00s)\n\nAll checks passed.\n")
	})
	t.Run("a run that failed names the tests that did, and the gate fails", func(t *testing.T) {
		r := newGateRig(t)
		r.answer("./it/...", gateITFailed, 1)

		out, exit := r.run("--it")

		assert.Equal(t, 1, exit, out)
		assert.Contains(t, out, "\n== integration tests\n--- FAIL: TestChatSuite (2.10s)\n    --- FAIL: TestChatSuite/TestStop (0.40s)\nFAIL\nFAIL\tgithub.com/v8tix/inside-agent-web-ui/it/chat\t2.412s\nFAIL\ncheck.sh: the integration tests failed")
		assert.NotContains(t, out, "checks passed")
	})
}

// Of a failed run the gate prints only the lines that name the tests, so the file
// with the rest, where the reasons are, has to be there when the gate has ended.
func TestCheck_AFailedIntegrationRunLeavesItsOutput(t *testing.T) {
	r := newGateRig(t)
	r.answer("./it/...", gateITFailed, 1)

	out, exit := r.run("--it")

	require.Equal(t, 1, exit, out)
	named := regexp.MustCompile(`(?m)^check\.sh: the integration tests failed\. .* their output, which stays in (\S+)$`).FindStringSubmatch(out)
	require.NotNil(t, named, "the gate says where the output is: %s", out)
	kept, err := os.ReadFile(named[1])
	require.NoError(t, err, "and the file it names is there")
	assert.Equal(t, gateITFailed, string(kept), "with all of the output, the reasons too")
	assert.Equal(t, []string{filepath.Base(named[1])}, r.leftBehind(), "it is the only file the gate leaves")
}

// The gate runs the tests of this package where there may be no Docker, the job of
// a pull request among them. So none of its files imports what starts a container:
// the helpers under it/internal, or Testcontainers.
func TestScripts_TheseTestsNeedNoDocker(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, file := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, imported := range parsed.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			require.NoError(t, err)
			assert.False(t, strings.Contains(path, "/it/internal/") || strings.Contains(path, "testcontainers"),
				"%s imports %s, which needs Docker: a test that needs it belongs in another package under it/", file, path)
		}
	}
}

// The gate picks the tests of it/security that start no container by their name,
// and go test passes without a word when a name fits no test. So the name the gate
// asks for has to be the name of the test that keeps the table of routes complete.
func TestCheck_TheRouteTestsItAsksForAreThere(t *testing.T) {
	gate, err := os.ReadFile("../../scripts/check.sh")
	require.NoError(t, err)
	asked := regexp.MustCompile(`-run '(\S+)' \./it/security/`).FindSubmatch(gate)
	require.NotNil(t, asked, "the gate picks the tests of it/security by name")
	files, err := filepath.Glob("../security/*_test.go")
	require.NoError(t, err)

	var picked []string
	for _, file := range files {
		src, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, m := range regexp.MustCompile(`(?m)^func (Test\w*)\(`).FindAllSubmatch(src, -1) {
			if regexp.MustCompile(string(asked[1])).Match(m[1]) {
				picked = append(picked, string(m[1]))
			}
		}
	}

	assert.Contains(t, picked, "TestRoutes_TheTableIsComplete")
}

// gateYAMLBlock is what a YAML file holds under a path of keys: the lines indented
// deeper than the last key, as they are written. It reads the block style of the
// workflow and of docker-compose.yml, two spaces a level, and nothing else of YAML.
func gateYAMLBlock(t *testing.T, src string, path ...string) string {
	t.Helper()
	block := src
	for depth, key := range path {
		indent := strings.Repeat("  ", depth)
		under := regexp.MustCompile(`(?m)^` + indent + regexp.QuoteMeta(key) + `:.*\n((?:` + indent + `  .*(?:\n|\z)|[ \t]*\n)*)`)
		m := under.FindStringSubmatch(block)
		require.NotNil(t, m, "the file has %s", strings.Join(path[:depth+1], "."))
		block = m[1]
	}
	return block
}

// CI runs the scripts a developer runs, so the two cannot check different things.
// A job that spelled out the commands of a script would drift from it.
func TestCI_RunsTheScripts(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/ci.yml")
	require.NoError(t, err)

	gate := gateYAMLBlock(t, string(workflow), "jobs", "gate")
	pullRequest := gateYAMLBlock(t, string(workflow), "jobs", "pull-request")

	assert.Regexp(t, `(?m)^\s+run: scripts/check\.sh$`, gate, "the gate job runs the whole gate, with no option")
	assert.Regexp(t, `(?m)^\s+run: scripts/pr-check\.sh `, pullRequest, "the pull request job runs the title and size check")
}
