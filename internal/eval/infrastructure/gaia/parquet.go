// Package gaia provides the evaluator's problems: GAIA from local parquet
// files (downloaded by fetch.go), and a synthetic set for the self-test.
package gaia

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/parquet-go/parquet-go"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
)

// Column names in GAIA's parquet files. Other columns (the nested annotator
// metadata, file_path) are not read.
const (
	colTaskID   = "task_id"
	colQuestion = "Question"
	colLevel    = "Level"
	colAnswer   = "Final answer"
	colFileName = "file_name"
)

// unscoredAnswer is what the test split holds in place of the answer.
const unscoredAnswer = "?"

// readBatch is how many rows are asked of a row group at a time.
const readBatch = 32

// Parquet reads problems from the files under dir laid out as
// <dir>/<config>/<split>/NNNN.parquet, which is what `datasets fetch` writes.
type Parquet struct {
	dir     string
	configs []string
	split   string
}

// NewParquet returns a source for the given GAIA configs (such as 2023_level1)
// and split.
func NewParquet(dir string, configs []string, split string) *Parquet {
	return &Parquet{dir: dir, configs: configs, split: split}
}

// Problems implements resource.ProblemSource. Configs are read in the order
// given, files in name order, rows in file order; a task id met a second time
// (2023_level1 is part of 2023_all) is skipped.
func (p *Parquet) Problems(ctx context.Context) iter.Seq2[domain.Problem, error] {
	return func(yield func(domain.Problem, error) bool) {
		seen := map[string]bool{}
		for rec, err := range p.allRecords(ctx) {
			if err != nil {
				yield(domain.Problem{}, err)
				return
			}
			problem, err := rec.problem()
			if err != nil {
				yield(domain.Problem{}, fmt.Errorf("%s: %w", rec.path, err))
				return
			}
			if seen[problem.TaskID] {
				continue
			}
			seen[problem.TaskID] = true
			if !yield(problem, nil) {
				return
			}
		}
	}
}

// allRecords yields the rows of every file: configs in the order given, files in
// name order, rows in file order. An error ends the sequence.
func (p *Parquet) allRecords(ctx context.Context) iter.Seq2[record, error] {
	return func(yield func(record, error) bool) {
		for _, config := range p.configs {
			files, err := p.files(config)
			if err != nil {
				yield(record{}, err)
				return
			}
			for _, file := range files {
				for rec, err := range fileRecords(ctx, file) {
					if !yield(rec, err) || err != nil {
						return
					}
				}
			}
		}
	}
}

// files lists the parquet files of one config, in name order.
func (p *Parquet) files(config string) ([]string, error) {
	dir := filepath.Join(p.dir, config, p.split)
	files, err := filepath.Glob(filepath.Join(dir, "*.parquet"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no GAIA files for config %s, split %s in %s: run `agent-eval datasets fetch` first", config, p.split, p.dir)
	}
	slices.Sort(files)
	return files, nil
}

// record is one row of a GAIA file, as text, and the path of the file it is from.
type record struct {
	path                                      string
	taskID, question, level, answer, fileName string
}

// unscored is true for a row with no published answer: GAIA leaves it out of the
// test split's answers.
func (r record) unscored() bool { return r.answer == "" || r.answer == unscoredAnswer }

func (r record) problem() (domain.Problem, error) {
	level, err := strconv.Atoi(strings.TrimSpace(r.level))
	if err != nil {
		return domain.Problem{}, fmt.Errorf("task %q: level %q is not a number", r.taskID, r.level)
	}
	p := domain.Problem{
		TaskID:   r.taskID,
		Level:    level,
		Question: r.question,
		Truth:    r.answer,
		FileName: r.fileName,
	}
	if r.unscored() {
		p.Truth, p.Unscored = "", true
	}
	if err := p.Valid(); err != nil {
		return domain.Problem{}, err
	}
	return p, nil
}

// fileRecords yields every row of a parquet file, in file order. It ends with an
// error when the file cannot be read, when ctx ends, or when the number of rows
// read is not the number the file's metadata promises.
func fileRecords(ctx context.Context, path string) iter.Seq2[record, error] {
	return func(yield func(record, error) bool) {
		f, err := os.Open(path)
		if err != nil {
			yield(record{}, err)
			return
		}
		defer func() { _ = f.Close() }()
		file, columns, err := parseFile(f, path)
		if err != nil {
			yield(record{}, err)
			return
		}

		var rows int64
		for _, group := range file.RowGroups() {
			for row, err := range groupRows(group) {
				if err != nil {
					yield(record{}, fmt.Errorf("%s: %w", path, err))
					return
				}
				if err := ctx.Err(); err != nil {
					yield(record{}, err)
					return
				}
				rows++
				rec := columns.record(row)
				rec.path = path
				if !yield(rec, nil) {
					return
				}
			}
		}
		if rows != file.NumRows() {
			yield(record{}, fmt.Errorf("%s: read %d rows but the file says it has %d", path, rows, file.NumRows()))
		}
	}
}

// parseFile reads the footer of an open parquet file and finds the columns we read.
func parseFile(f *os.File, path string) (*parquet.File, columnIndexes, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, columnIndexes{}, err
	}
	file, err := parquet.OpenFile(f, info.Size())
	if err != nil {
		return nil, columnIndexes{}, fmt.Errorf("%s is not a parquet file: %w", path, err)
	}
	columns, err := lookupColumns(file.Schema())
	if err != nil {
		return nil, columnIndexes{}, fmt.Errorf("%s: %w", path, err)
	}
	return file, columns, nil
}

// groupRows yields the rows of one row group, in order. A row is only good until
// the next one is asked for.
func groupRows(group parquet.RowGroup) iter.Seq2[parquet.Row, error] {
	return func(yield func(parquet.Row, error) bool) {
		reader := group.Rows()
		defer func() { _ = reader.Close() }()
		batch := make([]parquet.Row, readBatch)
		for {
			n, err := reader.ReadRows(batch)
			for _, row := range batch[:n] {
				if !yield(row, nil) {
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					yield(nil, err)
				}
				return
			}
		}
	}
}

// columnIndexes are the positions of the columns we read among the file's
// leaf columns. -1 means the column is absent.
type columnIndexes struct{ taskID, question, level, answer, fileName int }

func lookupColumns(schema *parquet.Schema) (columnIndexes, error) {
	find := func(name string) int {
		if leaf, ok := schema.Lookup(name); ok {
			return leaf.ColumnIndex
		}
		return -1
	}
	c := columnIndexes{
		taskID: find(colTaskID), question: find(colQuestion), level: find(colLevel),
		answer: find(colAnswer), fileName: find(colFileName),
	}
	var missing []string
	for name, idx := range map[string]int{colTaskID: c.taskID, colQuestion: c.question, colLevel: c.level, colAnswer: c.answer} {
		if idx < 0 {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return c, fmt.Errorf("not a GAIA file: no column named %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func (c columnIndexes) record(row parquet.Row) record {
	text := func(index int) string {
		if index < 0 {
			return ""
		}
		for _, v := range row {
			if v.Column() == index {
				return valueText(v)
			}
		}
		return ""
	}
	return record{
		taskID: text(c.taskID), question: text(c.question), level: text(c.level),
		answer: text(c.answer), fileName: text(c.fileName),
	}
}

// valueText renders a cell as text, whatever its type: GAIA stores Level as
// text, and a converted copy may store it as a number.
func valueText(v parquet.Value) string {
	if v.IsNull() {
		return ""
	}
	switch v.Kind() {
	case parquet.ByteArray, parquet.FixedLenByteArray:
		return string(v.ByteArray())
	case parquet.Int32:
		return strconv.FormatInt(int64(v.Int32()), 10)
	case parquet.Int64:
		return strconv.FormatInt(v.Int64(), 10)
	case parquet.Boolean:
		return strconv.FormatBool(v.Boolean())
	case parquet.Float:
		return strconv.FormatFloat(float64(v.Float()), 'f', -1, 32)
	case parquet.Double:
		return strconv.FormatFloat(v.Double(), 'f', -1, 64)
	}
	return v.String()
}

// DatasetInfo describes the local files of one config and split.
type DatasetInfo struct {
	Config, Split string
	Files         int
	Bytes         int64
	Rows          int
	// Attachments counts rows that come with a file, Unscored rows with no
	// published answer.
	Attachments, Unscored int
	Levels                map[int]int
}

// Inventory reads the layout under dir (<config>/<split>/*.parquet) and counts
// what is in it, for `datasets list`. A missing directory is an empty inventory.
func Inventory(ctx context.Context, dir string) ([]DatasetInfo, error) {
	splits, err := filepath.Glob(filepath.Join(dir, "*", "*"))
	if err != nil {
		return nil, err
	}
	slices.Sort(splits)
	var out []DatasetInfo
	for _, splitDir := range splits {
		files, _ := filepath.Glob(filepath.Join(splitDir, "*.parquet"))
		if len(files) == 0 {
			continue
		}
		slices.Sort(files)
		info := DatasetInfo{
			Config: filepath.Base(filepath.Dir(splitDir)), Split: filepath.Base(splitDir),
			Files: len(files), Levels: map[int]int{},
		}
		for _, file := range files {
			if err := info.add(ctx, file); err != nil {
				return nil, err
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// add counts the size of a file and what is in its rows.
func (info *DatasetInfo) add(ctx context.Context, file string) error {
	if stat, err := os.Stat(file); err == nil {
		info.Bytes += stat.Size()
	}
	for rec, err := range fileRecords(ctx, file) {
		if err != nil {
			return err
		}
		info.Rows++
		if rec.fileName != "" {
			info.Attachments++
		}
		if rec.unscored() {
			info.Unscored++
		}
		if level, err := strconv.Atoi(strings.TrimSpace(rec.level)); err == nil {
			info.Levels[level]++
		}
	}
	return nil
}
