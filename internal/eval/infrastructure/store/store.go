// Package store keeps a run's files on disk: its results one row at a time,
// its description, what the probes found, its summaries and its evidence.
//
// A run is a directory under the results directory:
//
//	<dir>/<run-id>/results.jsonl   one JSON row per attempt, appended as it finishes
//	               run.json        what ran, with what, and when; no secrets
//	               probes.json     what the probes found
//	               summary.json    the scorecards
//	               summary.md      the same, for people
//	               evidence/       screenshots of failures
package store

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
)

// File and directory names inside a run's directory.
const (
	resultsFile  = "results.jsonl"
	runFile      = "run.json"
	probesFile   = "probes.json"
	summaryJSON  = "summary.json"
	summaryMD    = "summary.md"
	evidenceDir  = "evidence"
	dirPerm      = 0o700
	filePerm     = 0o600
	tempFilePart = ".tmp-"
)

var _ resource.ResultStore = (*Store)(nil)

// ErrBadRunID is returned for a run id that is not safe to use as a directory name.
var ErrBadRunID = errors.New("invalid run id")

// ErrRunNotBegun is returned when a row is appended to a run that Begin has not
// opened in this process.
var ErrRunNotBegun = errors.New("the run was not begun")

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)

// Store is a ResultStore on the local disk. It is safe for use by several
// goroutines, which is how workers share it.
//
// It does not keep a second process off a run: two agent-eval processes given
// the same run id both append to its results.jsonl. Every row is still one whole
// line, because a row is one write to a file opened for appending, but the run
// then holds the rows of both, and an attempt both asked has two, of which the
// later counts. So it is one agent-eval for a run id at a time.
type Store struct {
	dir string

	mu          sync.Mutex
	includeText map[string]bool // by run id, as set by the latest Begin
}

// New returns a Store that keeps runs under dir.
func New(dir string) *Store {
	return &Store{dir: dir, includeText: map[string]bool{}}
}

// RunDir is where a run's files are.
func (s *Store) RunDir(runID string) (string, error) {
	if !runIDPattern.MatchString(runID) || strings.Contains(runID, "..") {
		return "", fmt.Errorf("%w %q: use letters, digits, '.', '_' and '-', starting with a letter or digit", ErrBadRunID, runID)
	}
	return filepath.Join(s.dir, runID), nil
}

// existingRunDir is RunDir for a run that must already have files.
func (s *Store) existingRunDir(runID string) (string, error) {
	dir, err := s.RunDir(runID)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: %s", resource.ErrRunNotFound, runID)
	}
	return dir, nil
}

// writeJSON writes v as indented JSON, replacing path in one step so a reader
// never sees half a file. Map keys are sorted, so the same value gives the same
// bytes.
func writeJSON(path string, v any) error {
	data, err := json.Marshal(v, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// writeFileAtomic writes data to a temporary file next to path and renames it.
func writeFileAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), tempFilePart+"*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp.Name(), filePerm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// readJSON reads a file written by writeJSON. Members it does not know are
// ignored, so an older tool can still read a newer run.
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes.TrimSpace(data), v)
}
