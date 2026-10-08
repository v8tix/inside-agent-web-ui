package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The files that must never be committed, and the ones that must: the settings
// hold keys and passwords, the dataset is gated, and a rule that is one star too
// wide hides a file somebody needed. git says what it would do.
func TestGitignore(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skip("this is not a git checkout, so there is nothing for git to say")
	}
	ignored := func(path string) bool {
		t.Helper()
		err := exec.CommandContext(t.Context(), "git", "-C", root, "check-ignore", "-q", "--", path).Run()
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
			return false // git ran and the path is not ignored
		}
		require.NoError(t, err, "git check-ignore %s", path)
		return true
	}

	tests := []struct {
		path string
		want bool
		why  string
	}{
		{".env", true, "the settings, with the keys"},
		{".env.local", true, "any copy of them"},
		{".env.production", true, "any copy of them"},
		{"staging.env", true, "a copy of them under another name, for --env"},
		{"deploy/other.env", true, "in any directory"},
		{".env.example", false, "the example is the one that is shared"},
		{"datasets/gaia/2023/validation/metadata.jsonl", true, "GAIA is gated: it is never committed"},
		{"results/run-1/results.jsonl", true, "results hold the dataset's answers"},
		{"logs/web.log", true, "logs"},
		{"bin/tools", true, "tools built here, or a link to tools built elsewhere: git says nothing about what is beyond a link"},
		{"temp/skills-output/phase-1-spec.md", true, "scratch"},
		{".idea/modules.xml", true, "the IDE's own settings, which can hold database connections"},
		{"internal/idea/idea.go", false, "a package that happens to be called idea is code"},
		{"internal/eval/infrastructure/gaia/dataset.go", false, "code that mentions the dataset is code"},
		{"cmd/eval/main.go", false, "and the evaluator's command"},
		{"README.md", false, "the readme"},
		{"docs/spec.md", false, "the spec the build ends with"},
		{"scripts/real-agent.sh", false, "scripts"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, ignored(tc.path), tc.why)
		})
	}
}

// What Docker leaves out of the build context of the two images, and what it has
// to let in: the settings hold keys and passwords, under whatever name and in
// whatever directory, and a rule that is one star too wide would keep out a file
// the images are built from. The file is read here by Docker's rules for it, so
// the test needs no Docker.
func TestDockerignore(t *testing.T) {
	file, err := os.ReadFile("../../.dockerignore")
	require.NoError(t, err)

	// The rules: a line is a pattern for a path from the root of the build context;
	// * and ? stay inside one name, and ** stands for any number of directories,
	// none included; a pattern that fits a directory covers what is in it; a line
	// that starts with ! lets back in what an earlier line left out; and the last
	// line that fits a path decides. A # in the first column starts a comment, and
	// a pattern is tidied as a path is (bin/ and ./bin and /bin are bin).
	type rule struct {
		fits *regexp.Regexp
		in   bool
	}
	var rules []rule
	wildcards := strings.NewReplacer(`\*\*/`, `(.*/)?`, `\*\*`, `.*`, `\*`, `[^/]*`, `\?`, `[^/]`)
	for line := range strings.SplitSeq(string(file), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		pattern, in := strings.CutPrefix(strings.TrimSpace(line), "!")
		if pattern = strings.TrimSpace(pattern); pattern == "" {
			continue
		}
		if pattern = path.Clean(pattern); len(pattern) > 1 {
			pattern = strings.TrimPrefix(pattern, "/")
		}
		// What this test does not read as Docker does: a set of characters, an escape,
		// and two stars that neither end the pattern nor stand before a slash.
		require.False(t, strings.ContainsAny(pattern, `[]\`) || regexp.MustCompile(`\*\*[^/]`).MatchString(pattern),
			"this test reads the wildcards *, ? and ** only, and %q has more", pattern)
		rules = append(rules, rule{regexp.MustCompile("^" + wildcards.Replace(regexp.QuoteMeta(pattern)) + "$"), in})
	}
	leftOut := func(name string) bool {
		out := false
		for _, r := range rules {
			for at := name; at != "."; at = path.Dir(at) {
				if r.fits.MatchString(at) {
					out = !r.in
					break
				}
			}
		}
		return out
	}

	tests := []struct {
		path string
		want bool
		why  string
	}{
		{".env", true, "the settings, with the keys"},
		{".env.local", true, "any copy of them"},
		{".env.production", true, "any copy of them"},
		{"staging.env", true, "a copy of them under another name, for --env"},
		{"deploy/other.env", true, "in any directory"},
		{"deploy/.env", true, "in any directory"},
		{"deploy/.env.local", true, "in any directory"},
		{".env.example", false, "the example holds no secret"},
		{".git/config", true, "the history"},
		{".idea/modules.xml", true, "the IDE's own settings, which can hold database connections"},
		{"bin/tools/mockery", true, "tools built here"},
		{"logs/agent-web-ui-20261005-101500.log", true, "logs"},
		{"results/run-1/results.jsonl", true, "results hold the dataset's answers"},
		{"datasets/gaia/2023/validation/metadata.jsonl", true, "GAIA is gated: it goes into no image"},
		{"temp/skills-output/phase-1-spec.md", true, "scratch"},
		{"it/scripts/scripts.test", true, "a test binary somebody built"},
		{"go.mod", false, "what the images are built from"},
		{"go.sum", false, "what the images are built from"},
		{"cmd/web/main.go", false, "the web server"},
		{"cmd/test-agent/main.go", false, "the test agent"},
		{"internal/platform/envcfg/envcfg.go", false, "code that reads the settings is code"},
		{"internal/infrastructure/postgres/migrations/0001_init.up.sql", false, "the migrations, which the program carries"},
		{"assets/templates/base.html", false, "the page, which the program carries"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, leftOut(tc.path), tc.why)
		})
	}
}
