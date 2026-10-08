package safety_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/safety"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

// repo makes a git repository in a temporary directory, away from any global
// git settings, and returns its path.
func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	dir := filepath.Join(t.TempDir(), "work")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	git(t, dir, "init", "-q")
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestGuard_Check(t *testing.T) {
	ctx := context.Background()
	guard := safety.Guard{}

	t.Run("a directory in a work tree that is not ignored is refused", func(t *testing.T) {
		work := repo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(work, "datasets"), 0o755))

		err := guard.Check(ctx, filepath.Join(work, "datasets"))

		require.Error(t, err)
		assert.Contains(t, err.Error(), "is inside a git work tree and is not ignored")
		assert.Contains(t, err.Error(), "Add it to .gitignore")
	})
	t.Run("one that does not exist yet is refused too", func(t *testing.T) {
		work := repo(t)

		err := guard.Check(ctx, filepath.Join(work, "results", "run-1"))

		require.ErrorContains(t, err, "not ignored")
	})
	t.Run("an ignored directory passes, there or not yet", func(t *testing.T) {
		work := repo(t)
		write(t, filepath.Join(work, ".gitignore"), "/datasets/\n/results/\n")
		require.NoError(t, os.MkdirAll(filepath.Join(work, "datasets"), 0o755))

		require.NoError(t, guard.Check(ctx, filepath.Join(work, "datasets")))
		require.NoError(t, guard.Check(ctx, filepath.Join(work, "results")), "not created yet")
		require.NoError(t, guard.Check(ctx, filepath.Join(work, "results", "deep", "er")), "below an ignored directory")
	})
	t.Run("a relative path works", func(t *testing.T) {
		work := repo(t)
		write(t, filepath.Join(work, ".gitignore"), "/datasets/\n")
		t.Chdir(work)

		require.NoError(t, guard.Check(ctx, "./datasets"))
		require.ErrorContains(t, guard.Check(ctx, "./results"), "not ignored")
	})
	t.Run("an ignored directory that already has tracked files is refused", func(t *testing.T) {
		work := repo(t)
		write(t, filepath.Join(work, "datasets", "validation.parquet"), "PAR1")
		git(t, work, "add", "-f", "datasets/validation.parquet")
		write(t, filepath.Join(work, ".gitignore"), "/datasets/\n")

		err := guard.Check(ctx, filepath.Join(work, "datasets"))

		require.ErrorContains(t, err, "git already tracks files in it")
		assert.ErrorContains(t, err, "git rm --cached")
	})
	t.Run("a directory outside any repository passes", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git is not installed")
		}
		outside := t.TempDir()

		require.NoError(t, guard.Check(ctx, filepath.Join(outside, "results")))
	})
	t.Run("without git there is nothing to check", func(t *testing.T) {
		work := repo(t)

		require.NoError(t, safety.Guard{Git: filepath.Join(work, "no-such-git")}.Check(ctx, filepath.Join(work, "results")))
	})
	t.Run("every problem is reported", func(t *testing.T) {
		work := repo(t)
		write(t, filepath.Join(work, ".gitignore"), "/datasets/\n")

		err := guard.Check(ctx, filepath.Join(work, "datasets"), filepath.Join(work, "results"), filepath.Join(work, "evidence"))

		require.Error(t, err)
		assert.Contains(t, err.Error(), "results")
		assert.Contains(t, err.Error(), "evidence")
		assert.NotContains(t, err.Error(), "datasets is inside")
	})
	t.Run("this repository ignores the directories the evaluator uses", func(t *testing.T) {
		root, err := filepath.Abs("../../../..")
		require.NoError(t, err)
		if _, err := os.Stat(filepath.Join(root, ".gitignore")); err != nil {
			t.Skip("not running inside the repository")
		}

		require.NoError(t, guard.Check(ctx, filepath.Join(root, "datasets"), filepath.Join(root, "results")),
			"datasets/ and results/ belong in .gitignore (spec §5.9)")
	})
}
