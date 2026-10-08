package gaia_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/encoding/thrift"
	"github.com/parquet-go/parquet-go/format"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/gaia"
)

// --- test files: tiny parquet files shaped like GAIA's, with made-up content ----

type annotator struct {
	Steps string `parquet:"Steps"`
	Tools string `parquet:"Tools"`
}

type row struct {
	TaskID      string    `parquet:"task_id"`
	Question    string    `parquet:"Question"`
	Level       string    `parquet:"Level"`
	FinalAnswer string    `parquet:"Final answer"`
	FileName    string    `parquet:"file_name"`
	FilePath    string    `parquet:"file_path"`
	Meta        annotator `parquet:"Annotator Metadata"`
}

type rowNumericLevel struct {
	TaskID      string `parquet:"task_id"`
	Question    string `parquet:"Question"`
	Level       int64  `parquet:"Level"`
	FinalAnswer string `parquet:"Final answer"`
}

type rowNoAnswerColumn struct {
	TaskID   string `parquet:"task_id"`
	Question string `parquet:"Question"`
	Level    string `parquet:"Level"`
}

func writeParquet[T any](t *testing.T, path string, rows []T, rowsPerGroup int64) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	var buf bytes.Buffer
	w := parquet.NewGenericWriter[T](&buf, parquet.MaxRowsPerRowGroup(rowsPerGroup))
	_, err := w.Write(rows)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

func gaiaRows(prefix string, level string, n int) []row {
	var rows []row
	for i := range n {
		id := prefix + "-" + string(rune('a'+i))
		rows = append(rows, row{
			TaskID: id, Question: "Question " + id + "?", Level: level, FinalAnswer: "answer " + id,
			Meta: annotator{Steps: "1. think", Tools: "none"},
		})
	}
	return rows
}

func collect(t *testing.T, seq iter.Seq2[domain.Problem, error]) ([]domain.Problem, error) {
	t.Helper()
	var out []domain.Problem
	for p, err := range seq {
		if err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, nil
}

func ids(problems []domain.Problem) []string {
	out := make([]string, len(problems))
	for i, p := range problems {
		out[i] = p.TaskID
	}
	return out
}

// --- parquet ---------------------------------------------------------------------

func TestParquet_ReadsGAIAColumns(t *testing.T) {
	dir := t.TempDir()
	writeParquet(t, filepath.Join(dir, "2023_level1", "validation", "0000.parquet"), []row{
		{TaskID: "t-1", Question: "What is 6 x 7?", Level: "1", FinalAnswer: "42", Meta: annotator{"s", "t"}},
		{TaskID: "t-2", Question: "Read the file.", Level: "2", FinalAnswer: "red, blue", FileName: "data.xlsx", FilePath: "x/data.xlsx"},
	}, 100)

	got, err := collect(t, gaia.NewParquet(dir, []string{"2023_level1"}, "validation").Problems(t.Context()))

	require.NoError(t, err)
	assert.Equal(t, []domain.Problem{
		{TaskID: "t-1", Level: 1, Question: "What is 6 x 7?", Truth: "42"},
		{TaskID: "t-2", Level: 2, Question: "Read the file.", Truth: "red, blue", FileName: "data.xlsx"},
	}, got, "the nested metadata and file_path are ignored")
}

func TestParquet_RowCountMatchesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2023_level1", "validation", "0000.parquet")
	writeParquet(t, path, gaiaRows("t", "1", 7), 3) // three row groups

	got, err := collect(t, gaia.NewParquet(dir, []string{"2023_level1"}, "validation").Problems(t.Context()))
	require.NoError(t, err)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	file, err := parquet.OpenFile(bytes.NewReader(raw), int64(len(raw)))
	require.NoError(t, err)
	assert.Len(t, file.RowGroups(), 3)
	assert.EqualValues(t, file.NumRows(), len(got), "as many problems as the file's metadata promises")
	assert.Equal(t, []string{"t-a", "t-b", "t-c", "t-d", "t-e", "t-f", "t-g"}, ids(got), "in file order, across row groups")
}

func TestParquet_ReadsMoreRowsThanOneBatch(t *testing.T) {
	dir := t.TempDir()
	var rows []row
	var want []string
	for i := range 2*gaia.ReadBatchForTest + 6 { // two whole batches and part of a third
		id := fmt.Sprintf("t-%03d", i)
		rows = append(rows, row{TaskID: id, Question: "Question " + id + "?", Level: "1", FinalAnswer: "answer " + id})
		want = append(want, id)
	}
	writeParquet(t, filepath.Join(dir, "c", "validation", "0000.parquet"), rows, 1000) // one row group

	got, err := collect(t, gaia.NewParquet(dir, []string{"c"}, "validation").Problems(t.Context()))

	require.NoError(t, err)
	assert.Equal(t, want, ids(got), "every row, once, in order")
}

func TestParquet_OrderIsStable(t *testing.T) {
	dir := t.TempDir()
	writeParquet(t, filepath.Join(dir, "2023_level1", "validation", "0001.parquet"), gaiaRows("second", "1", 2), 100)
	writeParquet(t, filepath.Join(dir, "2023_level1", "validation", "0000.parquet"), gaiaRows("first", "1", 2), 100)
	writeParquet(t, filepath.Join(dir, "2023_level2", "validation", "0000.parquet"), gaiaRows("other", "2", 1), 100)

	src := gaia.NewParquet(dir, []string{"2023_level2", "2023_level1"}, "validation")

	for range 3 {
		got, err := collect(t, src.Problems(t.Context()))
		require.NoError(t, err)
		assert.Equal(t, []string{"other-a", "first-a", "first-b", "second-a", "second-b"}, ids(got),
			"configs in the order asked, files by name, rows in file order")
	}
}

func TestParquet_TestSplitHasNoAnswers(t *testing.T) {
	dir := t.TempDir()
	writeParquet(t, filepath.Join(dir, "2023_level1", "test", "0000.parquet"), []row{
		{TaskID: "t-1", Question: "Q?", Level: "1", FinalAnswer: "?"},
		{TaskID: "t-2", Question: "Q2?", Level: "3", FinalAnswer: ""},
	}, 100)

	got, err := collect(t, gaia.NewParquet(dir, []string{"2023_level1"}, "test").Problems(t.Context()))

	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, p := range got {
		assert.True(t, p.Unscored, p.TaskID)
		assert.Empty(t, p.Truth, "a placeholder is not an answer")
		assert.False(t, p.Eligible(domain.AttachmentsInclude))
	}
}

func TestParquet_LevelAsANumber(t *testing.T) {
	dir := t.TempDir()
	writeParquet(t, filepath.Join(dir, "c", "validation", "0000.parquet"), []rowNumericLevel{
		{TaskID: "n-1", Question: "Q?", Level: 3, FinalAnswer: "x"},
	}, 100)

	got, err := collect(t, gaia.NewParquet(dir, []string{"c"}, "validation").Problems(t.Context()))

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 3, got[0].Level)
	assert.Empty(t, got[0].FileName, "the optional file_name column may be missing")
}

func TestParquet_RepeatedTaskIDsAreAskedOnce(t *testing.T) {
	dir := t.TempDir()
	writeParquet(t, filepath.Join(dir, "2023_level1", "validation", "0000.parquet"), gaiaRows("t", "1", 2), 100)
	writeParquet(t, filepath.Join(dir, "2023_all", "validation", "0000.parquet"), gaiaRows("t", "1", 3), 100) // overlaps

	got, err := collect(t, gaia.NewParquet(dir, []string{"2023_level1", "2023_all"}, "validation").Problems(t.Context()))

	require.NoError(t, err)
	assert.Equal(t, []string{"t-a", "t-b", "t-c"}, ids(got))
}

func TestParquet_Problems(t *testing.T) {
	t.Run("no files", func(t *testing.T) {
		_, err := collect(t, gaia.NewParquet(t.TempDir(), []string{"2023_level1"}, "validation").Problems(t.Context()))

		require.ErrorContains(t, err, "no GAIA files for config 2023_level1, split validation")
		assert.ErrorContains(t, err, "datasets fetch")
	})
	t.Run("a column is missing", func(t *testing.T) {
		dir := t.TempDir()
		writeParquet(t, filepath.Join(dir, "c", "validation", "0000.parquet"), []rowNoAnswerColumn{{"t", "Q?", "1"}}, 100)

		_, err := collect(t, gaia.NewParquet(dir, []string{"c"}, "validation").Problems(t.Context()))

		require.ErrorContains(t, err, "not a GAIA file: no column named Final answer")
	})
	t.Run("not a parquet file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "c", "validation", "0000.parquet")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("this is not parquet"), 0o644))

		_, err := collect(t, gaia.NewParquet(dir, []string{"c"}, "validation").Problems(t.Context()))

		require.ErrorContains(t, err, "is not a parquet file")
	})
	t.Run("a level that is not a number", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "c", "validation", "0000.parquet")
		writeParquet(t, path, []row{{TaskID: "t-9", Question: "Q?", Level: "hard", FinalAnswer: "a"}}, 100)

		_, err := collect(t, gaia.NewParquet(dir, []string{"c"}, "validation").Problems(t.Context()))

		require.ErrorContains(t, err, `task "t-9": level "hard" is not a number`)
		assert.ErrorContains(t, err, path, "the error says which file the row is in")
	})
	t.Run("a level out of range", func(t *testing.T) {
		dir := t.TempDir()
		writeParquet(t, filepath.Join(dir, "c", "validation", "0000.parquet"), []row{{TaskID: "t-9", Question: "Q?", Level: "7", FinalAnswer: "a"}}, 100)

		_, err := collect(t, gaia.NewParquet(dir, []string{"c"}, "validation").Problems(t.Context()))

		require.ErrorContains(t, err, "level 7")
	})
	t.Run("the caller can stop early", func(t *testing.T) {
		dir := t.TempDir()
		writeParquet(t, filepath.Join(dir, "c", "validation", "0000.parquet"), gaiaRows("t", "1", 5), 2)

		var seen int
		for _, err := range gaia.NewParquet(dir, []string{"c"}, "validation").Problems(t.Context()) {
			require.NoError(t, err)
			seen++
			if seen == 2 {
				break
			}
		}

		assert.Equal(t, 2, seen)
	})
	t.Run("a cancelled context stops the read", func(t *testing.T) {
		dir := t.TempDir()
		writeParquet(t, filepath.Join(dir, "c", "validation", "0000.parquet"), gaiaRows("t", "1", 5), 100)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := collect(t, gaia.NewParquet(dir, []string{"c"}, "validation").Problems(ctx))

		require.ErrorIs(t, err, context.Canceled)
	})
}

// Problems and Inventory read the files with the same reader, so a file it
// cannot read is an error for both, and neither hands back a smaller count.
func TestParquet_UnreadableFile(t *testing.T) {
	tests := []struct {
		name  string
		write func(t *testing.T, path string)
		check func(t *testing.T, err error, path string)
	}{
		{
			name: "it cannot be opened",
			write: func(t *testing.T, path string) {
				require.NoError(t, os.Symlink(path+".missing", path)) // a link to nothing
			},
			check: func(t *testing.T, err error, _ string) { assert.ErrorIs(t, err, fs.ErrNotExist) },
		},
		{
			name: "its data is damaged and its footer is not",
			write: func(t *testing.T, path string) {
				writeParquet(t, path, gaiaRows("t", "1", 40), 10)
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Greater(t, len(raw), 400, "room to damage the data without touching the footer")
				clear(raw[4:200])
				require.NoError(t, os.WriteFile(path, raw, 0o644))
			},
			check: func(t *testing.T, err error, path string) {
				assert.ErrorContains(t, err, path, "the error says which file")
				assert.NotContains(t, err.Error(), "the file says it has", "the error gives the cause, not only the rows that went missing")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "c", "validation", "0000.parquet")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			tc.write(t, path)

			_, problemsErr := collect(t, gaia.NewParquet(dir, []string{"c"}, "validation").Problems(t.Context()))
			_, inventoryErr := gaia.Inventory(t.Context(), dir)

			require.Error(t, problemsErr, "Problems")
			tc.check(t, problemsErr, path)
			require.Error(t, inventoryErr, "Inventory")
			tc.check(t, inventoryErr, path)
		})
	}
}

// promiseOneRowMore rewrites the footer of the parquet file at path, so that its
// metadata says the file has one row more than its row groups hold. A parquet file
// ends with the footer, the length of the footer in four bytes, and four magic
// bytes; the data before the footer stays as it is.
func promiseOneRowMore(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	const trailer = 8 // the length of the footer and the magic bytes
	require.Greater(t, len(raw), trailer)
	end := len(raw) - trailer
	start := end - int(binary.LittleEndian.Uint32(raw[end:]))
	require.Positive(t, start, "the footer comes after the data")

	var meta format.FileMetaData
	require.NoError(t, thrift.Unmarshal(new(thrift.CompactProtocol), raw[start:end], &meta))
	held := meta.NumRows
	meta.NumRows++
	footer, err := thrift.Marshal(new(thrift.CompactProtocol), &meta)
	require.NoError(t, err)
	lying := append(bytes.Clone(raw[:start]), footer...)
	lying = binary.LittleEndian.AppendUint32(lying, uint32(len(footer)))
	lying = append(lying, raw[end+4:]...)
	require.NoError(t, os.WriteFile(path, lying, 0o644))

	file, err := parquet.OpenFile(bytes.NewReader(lying), int64(len(lying)))
	require.NoError(t, err, "the file still opens")
	require.Equal(t, held+1, file.NumRows(), "its footer promises one row more")
	var inGroups int64
	for _, group := range file.RowGroups() {
		inGroups += group.NumRows()
	}
	require.Equal(t, held, inGroups, "than its row groups hold")
}

// The footer of a file says how many rows it has, and the reader counts the rows it
// reads. A file whose footer promises a row that is not there is refused, by the
// loader and by the inventory: neither ends quietly with the smaller count.
func TestParquet_AFooterThatLiesAboutItsRowsIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c", "validation", "0000.parquet")
	writeParquet(t, path, gaiaRows("t", "1", 3), 100)
	promiseOneRowMore(t, path)

	_, problemsErr := collect(t, gaia.NewParquet(dir, []string{"c"}, "validation").Problems(t.Context()))
	_, inventoryErr := gaia.Inventory(t.Context(), dir)

	const refusal = "read 3 rows but the file says it has 4"
	assert.ErrorContains(t, problemsErr, refusal, "Problems")
	assert.ErrorContains(t, problemsErr, path, "Problems: the error says which file")
	assert.ErrorContains(t, inventoryErr, refusal, "Inventory")
	assert.ErrorContains(t, inventoryErr, path, "Inventory: the error says which file")
}

// openFiles is how many descriptors this process has open, which says whether a
// reader left a file open behind it.
func openFiles(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skip("cannot count the open files here")
	}
	return len(entries)
}

// A consumer that stops early, a damaged file and a context that ends all leave the
// iterator midway; none of them may leave a file open behind it. Garbage collection
// would close a file nobody holds, so it is switched off while the files are counted.
func TestParquet_ClosesWhatItOpens(t *testing.T) {
	dir := t.TempDir()
	writeParquet(t, filepath.Join(dir, "good", "validation", "0000.parquet"), gaiaRows("t", "1", 5), 2)
	damaged := filepath.Join(dir, "damaged", "validation", "0000.parquet")
	writeParquet(t, damaged, gaiaRows("d", "1", 40), 10)
	raw, err := os.ReadFile(damaged)
	require.NoError(t, err)
	clear(raw[4:200])
	require.NoError(t, os.WriteFile(damaged, raw, 0o644))
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name string
		read func()
	}{
		{"read to the end", func() {
			for range gaia.NewParquet(dir, []string{"good"}, "validation").Problems(t.Context()) {
			}
		}},
		{"stopped after the first problem", func() {
			for range gaia.NewParquet(dir, []string{"good"}, "validation").Problems(t.Context()) {
				break
			}
		}},
		{"a file that turns out to be damaged", func() {
			for range gaia.NewParquet(dir, []string{"damaged"}, "validation").Problems(t.Context()) {
			}
		}},
		{"a context that has ended", func() {
			for range gaia.NewParquet(dir, []string{"good"}, "validation").Problems(cancelled) {
			}
		}},
		{"the inventory", func() { _, _ = gaia.Inventory(t.Context(), filepath.Join(dir)) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer debug.SetGCPercent(debug.SetGCPercent(-1))
			tc.read() // once, so that whatever the runtime opens for itself is open already
			before := openFiles(t)

			for range 40 {
				tc.read()
			}

			assert.LessOrEqual(t, openFiles(t), before, "no file is left open")
		})
	}
}

func TestInventory(t *testing.T) {
	dir := t.TempDir()
	rows := gaiaRows("a", "1", 3)
	rows[1].FileName = "f.png"
	rows[2].FileName = "g.pdf"
	writeParquet(t, filepath.Join(dir, "2023_level1", "validation", "0000.parquet"), rows, 100)
	writeParquet(t, filepath.Join(dir, "2023_level1", "validation", "0001.parquet"), gaiaRows("b", "1", 2), 100)
	writeParquet(t, filepath.Join(dir, "2023_level2", "test", "0000.parquet"), []row{
		{TaskID: "x", Question: "Q?", Level: "2", FinalAnswer: "?"},
		{TaskID: "y", Question: "Q?", Level: "3", FinalAnswer: "?", FileName: "z.csv"},
	}, 100)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "empty", "validation"), 0o755))

	got, err := gaia.Inventory(t.Context(), dir)

	require.NoError(t, err)
	require.Len(t, got, 2, "a directory without parquet files is not a dataset")
	assert.Equal(t, "2023_level1", got[0].Config)
	assert.Equal(t, "validation", got[0].Split)
	assert.Equal(t, 2, got[0].Files)
	assert.Equal(t, 5, got[0].Rows)
	assert.Equal(t, 2, got[0].Attachments)
	assert.Zero(t, got[0].Unscored)
	assert.Equal(t, map[int]int{1: 5}, got[0].Levels)
	assert.Positive(t, got[0].Bytes)
	assert.Equal(t, "2023_level2", got[1].Config)
	assert.Equal(t, "test", got[1].Split)
	assert.Equal(t, 2, got[1].Unscored)
	assert.Equal(t, 1, got[1].Attachments)
	assert.Equal(t, map[int]int{2: 1, 3: 1}, got[1].Levels)

	t.Run("a missing directory is an empty inventory", func(t *testing.T) {
		got, err := gaia.Inventory(t.Context(), filepath.Join(dir, "nowhere"))

		require.NoError(t, err)
		assert.Empty(t, got)
	})
	t.Run("a cancelled context stops the count", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := gaia.Inventory(ctx, dir)

		require.ErrorIs(t, err, context.Canceled)
	})
}

// --- synthetic ---------------------------------------------------------------------

const syntheticFile = `{"task_id":"s-1","level":1,"question":"What is 2 plus 2?","truth":"4"}

{"task_id":"s-2","level":2,"question":"Name two colors.","truth":"red, blue","file_name":"chart.png"}
{"task_id":"s-3","level":3,"question":"Spell it.","truth":"hello"}
`

func TestSynthetic(t *testing.T) {
	t.Run("reads problems in file order, skipping blank lines", func(t *testing.T) {
		got, err := collect(t, gaia.NewSyntheticBytes("mem", []byte(syntheticFile)).Problems(t.Context()))

		require.NoError(t, err)
		assert.Equal(t, []domain.Problem{
			{TaskID: "s-1", Level: 1, Question: "What is 2 plus 2?", Truth: "4"},
			{TaskID: "s-2", Level: 2, Question: "Name two colors.", Truth: "red, blue", FileName: "chart.png"},
			{TaskID: "s-3", Level: 3, Question: "Spell it.", Truth: "hello"},
		}, got)
	})
	problems := []struct {
		name, file, want string
	}{
		{"a typo in a field name", `{"task_id":"s-1","level":1,"question":"Q?","truht":"4"}`, "line 1"},
		{"not JSON", "task one", "line 1"},
		{"no answer", `{"task_id":"s-1","level":1,"question":"Q?"}`, "no answer"},
		{"a level out of range", `{"task_id":"s-1","level":9,"question":"Q?","truth":"4"}`, "level 9"},
		{"no question", `{"task_id":"s-1","level":1,"truth":"4"}`, "no question"},
		{"a repeated id", `{"task_id":"s-1","level":1,"question":"Q?","truth":"4"}` + "\n" + `{"task_id":"s-1","level":1,"question":"Q2?","truth":"4"}`, `"s-1" appears twice`},
		{"the second line is wrong", `{"task_id":"s-1","level":1,"question":"Q?","truth":"4"}` + "\n{bad", "line 2"},
	}
	for _, tc := range problems {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collect(t, gaia.NewSyntheticBytes("mem", []byte(tc.file)).Problems(t.Context()))

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
	t.Run("problems before the bad line are still delivered", func(t *testing.T) {
		got, err := collect(t, gaia.NewSyntheticBytes("mem", []byte(`{"task_id":"a","level":1,"question":"Q?","truth":"1"}`+"\n{bad")).Problems(t.Context()))

		require.Error(t, err)
		assert.Equal(t, []string{"a"}, ids(got))
	})
}
