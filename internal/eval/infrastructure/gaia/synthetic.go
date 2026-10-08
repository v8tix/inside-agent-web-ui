package gaia

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"iter"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// Synthetic reads problems from JSONL: one JSON object per line with task_id,
// level, question, truth and optionally file_name. The self-test uses it with
// original problems, which are not GAIA content.
type Synthetic struct {
	name string
	data []byte
}

// NewSyntheticBytes reads problems from data, for embedded files. Errors call
// the data name.
func NewSyntheticBytes(name string, data []byte) *Synthetic {
	return &Synthetic{name: name, data: data}
}

// Problems implements resource.ProblemSource, in file order.
func (s *Synthetic) Problems(ctx context.Context) iter.Seq2[domain.Problem, error] {
	return func(yield func(domain.Problem, error) bool) {
		seen := map[string]bool{}
		scanner := bufio.NewScanner(bytes.NewReader(s.data))
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for line := 1; scanner.Scan(); line++ {
			if err := ctx.Err(); err != nil {
				yield(domain.Problem{}, err)
				return
			}
			text := bytes.TrimSpace(scanner.Bytes())
			if len(text) == 0 {
				continue
			}
			var p domain.Problem
			if err := json.Unmarshal(text, &p, json.RejectUnknownMembers(true)); err != nil {
				yield(domain.Problem{}, fmt.Errorf("%s line %d: %w", s.name, line, err))
				return
			}
			if err := p.Valid(); err != nil {
				yield(domain.Problem{}, fmt.Errorf("%s line %d: %w", s.name, line, err))
				return
			}
			if seen[p.TaskID] {
				yield(domain.Problem{}, fmt.Errorf("%s line %d: task id %q appears twice", s.name, line, p.TaskID))
				return
			}
			seen[p.TaskID] = true
			if !yield(p, nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			yield(domain.Problem{}, fmt.Errorf("reading %s: %w", s.name, err))
		}
	}
}
