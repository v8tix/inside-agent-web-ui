// Package safety holds the checks that keep data and secrets out of places they
// must not go.
package safety

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Guard checks directories against git.
type Guard struct {
	// Git is the git command; empty means "git".
	Git string
}

// Check refuses a directory that git would pick up: one inside a work tree
// that is not ignored, or one that already holds tracked files. GAIA's terms
// forbid sharing its questions and answers, and results can contain both, so
// they must never be committed by accident.
//
// A directory outside any repository, or a machine without git, passes. The
// directory need not exist yet.
//
// Pattern: Git Guard
func (g Guard) Check(ctx context.Context, dirs ...string) error {
	var problems []error
	for _, dir := range dirs {
		if err := g.checkOne(ctx, dir); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

func (g Guard) checkOne(ctx context.Context, dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	anchor := nearestExisting(abs)

	if out, err := g.run(ctx, anchor, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return nil // not in a repository (or no git): nothing can be committed
	}
	// A file inside the directory stands for the directory, which may not exist.
	probe := filepath.Join(abs, ".agent-eval-probe")
	if _, err := g.run(ctx, anchor, "check-ignore", "-q", "--", probe); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return fmt.Errorf("%s is inside a git work tree and is not ignored: it would be committed with the code. Add it to .gitignore (for example /%s/)", dir, filepath.Base(abs))
		}
		return fmt.Errorf("asking git about %s: %w", dir, err)
	}
	if out, err := g.run(ctx, anchor, "ls-files", "--", abs); err == nil && strings.TrimSpace(out) != "" {
		return fmt.Errorf("%s is ignored, but git already tracks files in it (%s): untrack them with `git rm --cached`", dir, firstLine(out))
	}
	return nil
}

func (g Guard) run(ctx context.Context, dir string, args ...string) (string, error) {
	git := g.Git
	if git == "" {
		git = "git"
	}
	cmd := exec.CommandContext(ctx, git, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "LC_ALL=C")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	return stdout.String(), err
}

// nearestExisting is the directory itself, or the closest parent that exists.
func nearestExisting(path string) string {
	for {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
