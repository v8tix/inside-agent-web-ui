package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
)

// secretKey matches setting names that hold secrets. run.json is meant to be
// shared, so these never go in, whatever the caller passes.
var secretKey = regexp.MustCompile(`(?i)(password|passwd|secret|token|cookie|credential|api[_-]?key|private[_-]?key)`)

// Begin creates the run's directory and writes run.json. Beginning a run that
// exists resumes it: its rows stay, a row cut short by a crash is removed, and
// the run keeps the time it first started.
func (s *Store) Begin(ctx context.Context, meta domain.RunMeta) error {
	dir, err := s.RunDir(meta.RunID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("creating the run directory: %w", err)
	}
	results := filepath.Join(dir, resultsFile)
	if f, err := os.OpenFile(results, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm); err != nil {
		return err
	} else if err := f.Close(); err != nil {
		return err
	}
	if err := repairTail(results); err != nil {
		return fmt.Errorf("repairing %s: %w", resultsFile, err)
	}

	// What this session keeps of the text is what it was asked to keep. What run.json
	// says is whether the results file may hold any: it does if an earlier session
	// kept it, whatever this one does.
	keepsText := meta.IncludeText
	if previous, err := s.LoadMeta(ctx, meta.RunID); err == nil {
		if !previous.StartedAt.IsZero() {
			meta.StartedAt = previous.StartedAt
		}
		meta.IncludeText = meta.IncludeText || previous.IncludeText
	}
	if err := s.writeMeta(dir, meta); err != nil {
		return err
	}
	s.mu.Lock()
	s.includeText[meta.RunID] = keepsText
	s.mu.Unlock()
	return nil
}

// Finish records how the run ended in run.json.
func (s *Store) Finish(_ context.Context, meta domain.RunMeta) error {
	dir, err := s.existingRunDir(meta.RunID)
	if err != nil {
		return err
	}
	return s.writeMeta(dir, meta)
}

func (s *Store) writeMeta(dir string, meta domain.RunMeta) error {
	meta.Settings = withoutSecrets(meta.Settings)
	meta.Versions = withoutSecrets(meta.Versions)
	return writeJSON(filepath.Join(dir, runFile), meta)
}

func withoutSecrets(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if !secretKey.MatchString(k) {
			out[k] = v
		}
	}
	return out
}

// LoadMeta returns what run.json says about the run.
func (s *Store) LoadMeta(_ context.Context, runID string) (domain.RunMeta, error) {
	dir, err := s.existingRunDir(runID)
	if err != nil {
		return domain.RunMeta{}, err
	}
	var meta domain.RunMeta
	if err := readJSON(filepath.Join(dir, runFile), &meta); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.RunMeta{}, fmt.Errorf("%w: %s has no %s", resource.ErrRunNotFound, runID, runFile)
		}
		return domain.RunMeta{}, fmt.Errorf("reading %s of %s: %w", runFile, runID, err)
	}
	return meta, nil
}

// SaveProbes keeps what the probes found, replacing any earlier outcomes.
func (s *Store) SaveProbes(_ context.Context, runID string, probes []domain.ProbeOutcome) error {
	dir, err := s.existingRunDir(runID)
	if err != nil {
		return err
	}
	if probes == nil {
		probes = []domain.ProbeOutcome{}
	}
	return writeJSON(filepath.Join(dir, probesFile), probes)
}

// LoadProbes returns what SaveProbes kept; a run with none has none.
func (s *Store) LoadProbes(_ context.Context, runID string) ([]domain.ProbeOutcome, error) {
	dir, err := s.existingRunDir(runID)
	if err != nil {
		return nil, err
	}
	var probes []domain.ProbeOutcome
	if err := readJSON(filepath.Join(dir, probesFile), &probes); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s of %s: %w", probesFile, runID, err)
	}
	return probes, nil
}

// EvidencePath returns where a file called name goes in the run's evidence
// directory, creating the directory.
func (s *Store) EvidencePath(_ context.Context, runID, name string) (string, error) {
	dir, err := s.existingRunDir(runID)
	if err != nil {
		return "", err
	}
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("evidence file name %q must be a plain file name", name)
	}
	evidence := filepath.Join(dir, evidenceDir)
	if err := os.MkdirAll(evidence, dirPerm); err != nil {
		return "", err
	}
	return filepath.Join(evidence, name), nil
}
