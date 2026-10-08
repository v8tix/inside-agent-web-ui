package store

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// Append adds one row to the run's results and syncs it, so that an interrupted
// run leaves a file with every finished attempt in it and nothing half written.
//
// Question, truth and answer text are dropped unless the run was begun with
// IncludeText: GAIA's terms forbid sharing them, and result files get shared.
//
// Append does not look at ctx. A row being written is always written whole;
// whether to record an interrupted attempt is for the caller to decide.
//
// Pattern: Append-Only Rows
func (s *Store) Append(_ context.Context, runID string, row domain.Result) error {
	dir, err := s.RunDir(runID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	include, begun := s.includeText[runID]
	if !begun {
		return fmt.Errorf("%w: call Begin before appending to %s", ErrRunNotBegun, runID)
	}

	if row.RunID == "" {
		row.RunID = runID
	}
	switch {
	case row.RunID != runID:
		return fmt.Errorf("a row of run %q cannot go in run %q", row.RunID, runID)
	case row.TaskID == "":
		return errors.New("a row needs a task id")
	case !row.Status.Valid():
		return fmt.Errorf("row %s has the unknown status %q", row.Key(), row.Status)
	case row.Attempt < 1:
		return fmt.Errorf("row %s has attempt %d: attempts start at 1", row.TaskID, row.Attempt)
	}
	if !include {
		row = row.WithoutText()
	}
	line, err := json.Marshal(row, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encoding row %s: %w", row.Key(), err)
	}
	f, err := os.OpenFile(filepath.Join(dir, resultsFile), os.O_APPEND|os.O_WRONLY, filePerm)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing row %s: %w", row.Key(), err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Load returns every complete row of the run, in the order they were written.
// A last line cut short by a crash is ignored; an unreadable line anywhere else
// is an error, because it means the file was damaged, not interrupted.
func (s *Store) Load(_ context.Context, runID string) ([]domain.Result, error) {
	dir, err := s.existingRunDir(runID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, resultsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseRows(data)
}

func parseRows(data []byte) ([]domain.Result, error) {
	var rows []domain.Result
	lines := bytes.Split(data, []byte{'\n'})
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var row domain.Result
		if err := json.Unmarshal(line, &row); err != nil {
			if i == len(lines)-1 {
				break // no newline after it: the write was cut short
			}
			return nil, fmt.Errorf("%s line %d is damaged: %w", resultsFile, i+1, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Index returns the attempts the run has finished, so that resuming does not
// ask them again. A timeout or an error from the agent is an answer too. An
// attempt the page broke is not: the evaluator could not do its work, so it is
// asked again, and the row it then writes replaces the one that broke.
func (s *Store) Index(ctx context.Context, runID string) (map[domain.AttemptKey]struct{}, error) {
	rows, err := s.Load(ctx, runID)
	if err != nil {
		return nil, err
	}
	finished := make(map[domain.AttemptKey]bool, len(rows))
	for _, r := range rows {
		finished[r.Key()] = r.Status != domain.StatusUIError // the last row for a slot decides
	}
	index := make(map[domain.AttemptKey]struct{}, len(finished))
	for key, done := range finished {
		if done {
			index[key] = struct{}{}
		}
	}
	return index, nil
}

// repairTail makes the results file end on a line break, so that the next row
// starts on a line of its own. A last line that is a whole row only lacks its
// break; one that is cut short is removed, which is what Load ignores anyway.
func repairTail(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return nil
	}
	cut := bytes.LastIndexByte(data, '\n') + 1
	var row domain.Result
	if json.Unmarshal(data[cut:], &row) == nil {
		return appendBytes(path, []byte{'\n'})
	}
	return os.Truncate(path, int64(cut))
}

func appendBytes(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, filePerm)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
