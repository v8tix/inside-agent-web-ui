package scripts_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prRig is a throwaway git repository that holds a copy of scripts/pr-check.sh
// and its helper, a main branch and settings of its own. The script works in the
// repository it sits in, so it can never touch the real one.
type prRig struct {
	t   *testing.T
	dir string
}

// Small limits, so that a few lines are enough to cross them.
const prRigSettings = `PR_BASE=main
PR_PREFIXES="feat fix docs chore breaking refactor test ci"
PR_TITLE_MAX=30
PR_MAX_LINES=10
PR_MAX_FILES=3
`

func newPRRig(t *testing.T, settings string) *prRig {
	t.Helper()
	dir := t.TempDir()
	r := &prRig{t: t, dir: dir}
	for _, file := range []string{"scripts/pr-check.sh", "scripts/lib/process.sh"} {
		src, err := os.ReadFile(filepath.Join("..", "..", file))
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), src, 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".env.example"), []byte(settings), 0o644))

	r.git("init", "--quiet", "--initial-branch=main")
	r.write("base.txt", 1)
	r.commit("base")
	return r
}

// env is the environment of the commands: no secrets, and nothing from the
// developer's own git configuration (a signing key, hooks, a default branch).
func (r *prRig) env() []string {
	return append(withoutSecrets(os.Environ()),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
}

func (r *prRig) git(args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = r.dir, r.env()
	out, err := cmd.CombinedOutput()
	require.NoError(r.t, err, "git %s: %s", strings.Join(args, " "), out)
}

// write makes a file of n lines.
func (r *prRig) write(name string, n int) {
	r.t.Helper()
	require.NoError(r.t, os.WriteFile(filepath.Join(r.dir, name), []byte(strings.Repeat("line\n", n)), 0o644))
}

func (r *prRig) commit(message string) {
	r.t.Helper()
	r.git("add", "--all")
	r.git("commit", "--quiet", "--message", message)
}

// change puts a branch off main that adds the files, each with its number of lines.
func (r *prRig) change(files map[string]int) {
	r.t.Helper()
	r.git("switch", "--quiet", "--create", "change")
	for name, n := range files {
		r.write(name, n)
	}
	r.commit("change")
}

func (r *prRig) run(args ...string) (output string, exit int) {
	r.t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(r.dir, "scripts", "pr-check.sh")}, args...)...)
	cmd.Dir, cmd.Env = r.dir, r.env()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &exitErr):
		return out.String(), exitErr.ExitCode()
	}
	require.NoError(r.t, err)
	return "", -1
}

func TestPRCheck_Title(t *testing.T) {
	r := newPRRig(t, prRigSettings)
	r.change(map[string]int{"a.txt": 1})

	tests := []struct {
		name  string
		title string
		exit  int
		out   string
	}{
		{"a prefix, a colon and what changed", "fix: stop the partial reply", 0, "title: ok (27 of 30 characters)"},
		{"a scope after the prefix", "feat(web): add Stop", 0, "title: ok"},
		{"every prefix the settings list", "breaking: drop the old flag", 0, "title: ok"},
		{"exactly the longest allowed", "docs: " + strings.Repeat("x", 24), 0, "title: ok (30 of 30 characters)"},
		{"one character too long", "docs: " + strings.Repeat("x", 25), 1, "31 characters, over the limit of 30"},
		{"no prefix", "stop the partial reply", 1, "needs a prefix and a colon"},
		{"a prefix the settings do not list", "wip: stop the partial reply", 1, "one of: feat fix docs chore breaking refactor test ci"},
		{"a prefix that only starts like a listed one", "fixes: stop the reply", 1, "needs a prefix and a colon"},
		{"nothing after the colon", "fix: ", 1, "needs a prefix and a colon"},
		{"no space after the colon", "fix:stop", 1, "needs a prefix and a colon"},
		{"an empty scope", "fix(): stop", 1, "needs a prefix and a colon"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, exit := r.run("--title", tc.title)

			assert.Equal(t, tc.exit, exit, out)
			assert.Contains(t, out, tc.out)
		})
	}

	t.Run("without a title only the size is checked", func(t *testing.T) {
		out, exit := r.run()

		assert.Zero(t, exit, out)
		assert.NotContains(t, out, "title:")
		assert.Contains(t, out, "size:  1 lines changed in 1 files")
	})
}

func TestPRCheck_Size(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]int
		out   string
		warn  bool
	}{
		{"a small change", map[string]int{"a.txt": 4}, "4 lines changed in 1 files", false},
		{"exactly at both limits", map[string]int{"a.txt": 5, "b.txt": 3, "c.txt": 2}, "10 lines changed in 3 files", false},
		{"one line over", map[string]int{"a.txt": 11}, "11 lines changed in 1 files", true},
		{"one file over", map[string]int{"a.txt": 1, "b.txt": 1, "c.txt": 1, "d.txt": 1}, "4 lines changed in 4 files", true},
		{"added and deleted lines both count", map[string]int{"base.txt": 0, "a.txt": 10}, "11 lines changed in 2 files", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newPRRig(t, prRigSettings)
			r.change(tc.files)

			out, exit := r.run()

			assert.Zero(t, exit, "the size only warns: %s", out)
			assert.Contains(t, out, tc.out)
			assert.Equal(t, tc.warn, strings.Contains(out, "warning:"), out)
		})
	}

	t.Run("what the base gained since the branch was made is not counted", func(t *testing.T) {
		r := newPRRig(t, prRigSettings)
		r.change(map[string]int{"a.txt": 2})
		r.git("switch", "--quiet", "main")
		r.write("later.txt", 9)
		r.commit("main moves on")
		r.git("switch", "--quiet", "change")

		out, exit := r.run()

		assert.Zero(t, exit, out)
		assert.Contains(t, out, "2 lines changed in 1 files", "a pull request shows the branch's own changes")
	})

	t.Run("the base can be named", func(t *testing.T) {
		r := newPRRig(t, prRigSettings)
		r.change(map[string]int{"a.txt": 2})
		r.git("switch", "--quiet", "--create", "next")
		r.write("b.txt", 3)
		r.commit("next")

		out, exit := r.run("--base", "change")

		assert.Zero(t, exit, out)
		assert.Contains(t, out, "3 lines changed in 1 files", "only what the second branch adds")
	})
}

func TestPRCheck_ReadsTheSettingsFileItIsTold(t *testing.T) {
	r := newPRRig(t, prRigSettings)
	r.change(map[string]int{"a.txt": 3})
	require.NoError(t, os.WriteFile(filepath.Join(r.dir, "stricter.env"), []byte("PR_MAX_LINES=2\nPR_TITLE_MAX=12\n"), 0o600))

	out, exit := r.run("--env", "stricter.env", "--title", "fix: too long for this one")

	assert.Equal(t, 1, exit, out)
	assert.Contains(t, out, "over the limit of 12", "the title limit is the named file's")
	assert.Contains(t, out, "guide: 2 lines, 3 files", "and so is the line limit, while the file limit still comes from the example")
	assert.Contains(t, out, "warning:")
}

func TestPRCheck_Mistakes(t *testing.T) {
	t.Run("a base that does not exist", func(t *testing.T) {
		r := newPRRig(t, prRigSettings)

		out, exit := r.run("--base", "nowhere")

		assert.Equal(t, 2, exit, out)
		assert.Contains(t, out, "no branch or commit called 'nowhere'")
	})
	t.Run("an option that does not exist", func(t *testing.T) {
		out, exit := newPRRig(t, prRigSettings).run("--nope")

		assert.Equal(t, 2, exit, out)
		assert.Contains(t, out, "unknown option: --nope")
	})
	t.Run("a title option without a title", func(t *testing.T) {
		out, exit := newPRRig(t, prRigSettings).run("--title")

		assert.Equal(t, 2, exit, out)
		assert.Contains(t, out, "--title needs the title")
	})
	t.Run("a limit that is not a number", func(t *testing.T) {
		out, exit := newPRRig(t, strings.Replace(prRigSettings, "PR_MAX_LINES=10", "PR_MAX_LINES=many", 1)).run()

		assert.Equal(t, 2, exit, out)
		assert.Contains(t, out, "must be whole numbers (got 'many')")
	})
	t.Run("a setting the example lost", func(t *testing.T) {
		out, exit := newPRRig(t, strings.Replace(prRigSettings, "PR_MAX_FILES=3\n", "", 1)).run()

		assert.NotZero(t, exit, out)
		assert.Contains(t, out, "PR_MAX_FILES is not set")
	})
}

// The values the real example gives are the ones the working agreement names.
func TestPRCheck_TheExampleHoldsTheAgreedLimits(t *testing.T) {
	for name, want := range map[string]string{
		"PR_BASE": "main", "PR_TITLE_MAX": "80", "PR_MAX_LINES": "500", "PR_MAX_FILES": "20",
	} {
		assert.Equal(t, want, exampleSetting(t, name), "%s in .env.example", name)
	}
	assert.Equal(t, "feat fix docs chore breaking refactor test ci", exampleSetting(t, "PR_PREFIXES"))
}
