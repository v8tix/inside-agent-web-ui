package store_test

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/report"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/store"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "to create it: go test ./internal/eval/infrastructure/store -update")
	assert.Equal(t, string(want), got)
}

func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	return store.New(dir), dir
}

func begin(t *testing.T, s *store.Store, id string, mutate ...func(*domain.RunMeta)) domain.RunMeta {
	t.Helper()
	meta := domain.RunMeta{RunID: id, Command: "run", StartedAt: time.Date(2026, 10, 2, 15, 30, 12, 0, time.UTC)}
	for _, m := range mutate {
		m(&meta)
	}
	require.NoError(t, s.Begin(t.Context(), meta))
	return meta
}

func includeText(m *domain.RunMeta) { m.IncludeText = true }

func result(task string, attempt int, status domain.AttemptStatus) domain.Result {
	return domain.Result{TaskID: task, Level: 1, Attempt: attempt, Status: status}
}

func resultsPath(dir, id string) string { return filepath.Join(dir, id, "results.jsonl") }

func TestRunDir_RejectsUnsafeIDs(t *testing.T) {
	s, dir := newStore(t)
	tests := []struct {
		id string
		ok bool
	}{
		{"20261002T153012Z-ab12", true},
		{"selftest.1_a-b", true},
		{"", false},
		{".", false},
		{"..", false},
		{"../escape", false},
		{"a/b", false},
		{`a\b`, false},
		{" lead", false},
		{"has space", false},
		{"-dash-first", false},
		{".hidden", false},
		{"a..b", false},
		{strings.Repeat("x", 81), false},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%q", tc.id), func(t *testing.T) {
			got, err := s.RunDir(tc.id)

			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, filepath.Join(dir, tc.id), got)
				return
			}
			require.ErrorIs(t, err, store.ErrBadRunID)
			assert.Empty(t, got)
		})
	}
	t.Run("every method refuses them", func(t *testing.T) {
		ctx := t.Context()
		require.ErrorIs(t, s.Begin(ctx, domain.RunMeta{RunID: "../x"}), store.ErrBadRunID)
		require.ErrorIs(t, s.Append(ctx, "../x", result("a", 1, domain.StatusCorrect)), store.ErrBadRunID)
		_, err := s.Load(ctx, "../x")
		require.ErrorIs(t, err, store.ErrBadRunID)
		_, err = s.EvidencePath(ctx, "../x", "f.png")
		require.ErrorIs(t, err, store.ErrBadRunID)
		entries, _ := os.ReadDir(filepath.Dir(dir))
		for _, e := range entries {
			assert.NotEqual(t, "x", e.Name(), "nothing was created next to the results directory")
		}
	})
}

func TestBegin_CreatesTheRunsFiles(t *testing.T) {
	s, dir := newStore(t)
	meta := begin(t, s, "r1", func(m *domain.RunMeta) {
		m.GitSHA, m.UIContract = "abc1234", "1"
		m.Versions = map[string]string{"go": "1.27"}
		m.Settings = map[string]string{"EVAL_WORKERS": "2"}
		m.Thresholds = domain.DefaultThresholds()
	})

	assert.FileExists(t, resultsPath(dir, "r1"), "an empty results file, so a run with no attempts yet loads")
	got, err := s.LoadMeta(t.Context(), "r1")

	require.NoError(t, err)
	assert.Equal(t, meta, got)
	rows, err := s.Load(t.Context(), "r1")
	require.NoError(t, err)
	assert.Empty(t, rows)

	t.Run("private to the user", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("file modes differ on Windows")
		}
		for _, p := range []string{filepath.Join(dir, "r1"), resultsPath(dir, "r1"), filepath.Join(dir, "r1", "run.json")} {
			info, err := os.Stat(p)
			require.NoError(t, err)
			assert.Zero(t, info.Mode().Perm()&0o077, "%s must not be readable by others", p)
		}
	})
}

func TestAppend_WritesOneCompleteLineAtATime(t *testing.T) {
	s, dir := newStore(t)
	begin(t, s, "r1")
	ms := int64(1500)

	row := result("t1", 1, domain.StatusCorrect)
	row.Score, row.LatencyMs = 1, &ms
	row.UIChecks = map[domain.CheckID]domain.CheckResult{domain.U01: domain.Pass}
	require.NoError(t, s.Append(t.Context(), "r1", row))

	t.Run("the line is on disk before Append returns", func(t *testing.T) {
		data, err := os.ReadFile(resultsPath(dir, "r1"))
		require.NoError(t, err)
		want := `{"run_id":"r1","task_id":"t1","level":1,"attempt":1,"status":"correct","score":1,"latency_ms":1500,"ui_checks":{"U-01":"pass"},"console_errors":0,"failed_requests":0}` + "\n"
		assert.Equal(t, want, string(data))
	})
	t.Run("a second row follows the first", func(t *testing.T) {
		require.NoError(t, s.Append(t.Context(), "r1", result("t2", 1, domain.StatusTimeout)))

		data, _ := os.ReadFile(resultsPath(dir, "r1"))
		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		require.Len(t, lines, 2)
		assert.Contains(t, lines[1], `"task_id":"t2"`)
		assert.Contains(t, lines[1], `"ui_checks":{}`, "a row without checks says so, not null")
	})
	t.Run("the rows come back in order", func(t *testing.T) {
		rows, err := s.Load(t.Context(), "r1")

		require.NoError(t, err)
		require.Len(t, rows, 2)
		assert.Equal(t, "t1", rows[0].TaskID)
		assert.Equal(t, int64(1500), *rows[0].LatencyMs)
		assert.Equal(t, domain.StatusTimeout, rows[1].Status)
	})
}

func TestAppend_Refusals(t *testing.T) {
	s, dir := newStore(t)
	begin(t, s, "r1")
	tests := []struct {
		name string
		run  string
		row  domain.Result
		want string
	}{
		{"another run's row", "r1", domain.Result{RunID: "other", TaskID: "a", Attempt: 1, Status: domain.StatusCorrect}, `cannot go in run "r1"`},
		{"no task id", "r1", domain.Result{Attempt: 1, Status: domain.StatusCorrect}, "needs a task id"},
		{"an unknown status", "r1", domain.Result{TaskID: "a", Attempt: 1, Status: "great"}, `unknown status "great"`},
		{"no status", "r1", domain.Result{TaskID: "a", Attempt: 1}, "unknown status"},
		{"attempt zero", "r1", domain.Result{TaskID: "a", Status: domain.StatusCorrect}, "attempts start at 1"},
		{"a run that was not begun", "r2", domain.Result{TaskID: "a", Attempt: 1, Status: domain.StatusCorrect}, "was not begun"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Append(t.Context(), tc.run, tc.row)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
	t.Run("and nothing was written", func(t *testing.T) {
		data, err := os.ReadFile(resultsPath(dir, "r1"))
		require.NoError(t, err)
		assert.Empty(t, data)
		assert.NoDirExists(t, filepath.Join(dir, "r2"))
	})
	t.Run("a row without a run id takes the run's", func(t *testing.T) {
		require.NoError(t, s.Append(t.Context(), "r1", result("a", 1, domain.StatusCorrect)))

		rows, err := s.Load(t.Context(), "r1")
		require.NoError(t, err)
		assert.Equal(t, "r1", rows[0].RunID)
	})
	t.Run("a store that never began the run says so even if the files exist", func(t *testing.T) {
		fresh := store.New(dir)

		err := fresh.Append(t.Context(), "r1", result("b", 1, domain.StatusCorrect))

		require.ErrorIs(t, err, store.ErrRunNotBegun)
	})
}

func TestAppend_TextIsLeftOutUnlessAsked(t *testing.T) {
	withText := func() domain.Result {
		r := result("t1", 1, domain.StatusIncorrect)
		r.Question, r.Truth, r.Answer = "QUESTION-CANARY?", "TRUTH-CANARY", "ANSWER-CANARY"
		return r
	}

	t.Run("by default", func(t *testing.T) {
		s, dir := newStore(t)
		begin(t, s, "r1")

		require.NoError(t, s.Append(t.Context(), "r1", withText()))

		data, _ := os.ReadFile(resultsPath(dir, "r1"))
		for _, canary := range []string{"QUESTION-CANARY", "TRUTH-CANARY", "ANSWER-CANARY"} {
			assert.NotContains(t, string(data), canary)
		}
		for _, field := range []string{`"question"`, `"truth"`, `"answer"`} {
			assert.NotContains(t, string(data), field)
		}
	})
	t.Run("with include-text", func(t *testing.T) {
		s, dir := newStore(t)
		begin(t, s, "r1", includeText)

		require.NoError(t, s.Append(t.Context(), "r1", withText()))

		rows, err := s.Load(t.Context(), "r1")
		require.NoError(t, err)
		assert.Equal(t, "QUESTION-CANARY?", rows[0].Question)
		assert.Equal(t, "TRUTH-CANARY", rows[0].Truth)
		assert.Equal(t, "ANSWER-CANARY", rows[0].Answer)
		data, _ := os.ReadFile(resultsPath(dir, "r1"))
		assert.Contains(t, string(data), "ANSWER-CANARY")
	})
	t.Run("the latest Begin decides", func(t *testing.T) {
		s, dir := newStore(t)
		begin(t, s, "r1", includeText)
		begin(t, s, "r1") // resumed without the flag

		require.NoError(t, s.Append(t.Context(), "r1", withText()))

		data, _ := os.ReadFile(resultsPath(dir, "r1"))
		assert.NotContains(t, string(data), "CANARY")
	})
	t.Run("the caller's row is not changed", func(t *testing.T) {
		s, _ := newStore(t)
		begin(t, s, "r1")
		row := withText()

		require.NoError(t, s.Append(t.Context(), "r1", row))

		assert.Equal(t, "ANSWER-CANARY", row.Answer)
	})
}

// threeRows writes three rows and returns the bytes of the file and the offset
// at which each row's line break is.
func threeRows(t *testing.T) (data []byte, ends []int) {
	t.Helper()
	s, dir := newStore(t)
	begin(t, s, "src")
	for i, status := range []domain.AttemptStatus{domain.StatusCorrect, domain.StatusIncorrect, domain.StatusTimeout} {
		require.NoError(t, s.Append(t.Context(), "src", result(fmt.Sprintf("task-%d", i), 1, status)))
	}
	data, err := os.ReadFile(resultsPath(dir, "src"))
	require.NoError(t, err)
	for i, b := range data {
		if b == '\n' {
			ends = append(ends, i)
		}
	}
	require.Len(t, ends, 3)
	return data, ends
}

func TestLoad_IgnoresALastLineCutShort(t *testing.T) {
	data, ends := threeRows(t)

	for cut := range len(data) + 1 {
		t.Run(fmt.Sprintf("cut at byte %d", cut), func(t *testing.T) {
			s, dir := newStore(t)
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "r1"), 0o700))
			require.NoError(t, os.WriteFile(resultsPath(dir, "r1"), data[:cut], 0o600))
			want := 0
			for _, end := range ends {
				if end <= cut { // a row is whole once its closing brace is there
					want++
				}
			}

			rows, err := s.Load(t.Context(), "r1")

			require.NoError(t, err, "a crash must never make the results unreadable")
			assert.Len(t, rows, want)
		})
	}
}

func TestLoad_DamageInTheMiddleIsAnError(t *testing.T) {
	data, ends := threeRows(t)
	damaged := append(bytes.Clone(data[:ends[0]+1]), []byte("{\"run_id\": nope}\n")...)
	damaged = append(damaged, data[ends[1]+1:]...)
	s, dir := newStore(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "r1"), 0o700))
	require.NoError(t, os.WriteFile(resultsPath(dir, "r1"), damaged, 0o600))

	_, err := s.Load(t.Context(), "r1")

	require.ErrorContains(t, err, "results.jsonl line 2 is damaged")
}

func TestLoad_ToleratesWhatAnotherVersionAdded(t *testing.T) {
	s, dir := newStore(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "r1"), 0o700))
	line := `{"run_id":"r1","task_id":"a","level":1,"attempt":1,"status":"correct","score":1,"ui_checks":{},"console_errors":0,"failed_requests":0,"added_later":{"x":1}}` + "\n\n"
	require.NoError(t, os.WriteFile(resultsPath(dir, "r1"), []byte(line), 0o600))

	rows, err := s.Load(t.Context(), "r1")

	require.NoError(t, err)
	require.Len(t, rows, 1, "unknown members and blank lines do not matter")
}

func TestBegin_ResumesARunThatWasCutShort(t *testing.T) {
	data, ends := threeRows(t)

	tests := []struct {
		name     string
		contents []byte
		wantRows int
	}{
		{"cut in the middle of the third row", data[:ends[2]-10], 2},
		{"third row whole but without its line break", data[:ends[2]], 3},
		{"cut right after a line break", data[:ends[1]+1], 2},
		{"cut after one byte of the next row", data[:ends[1]+2], 2},
		{"already clean", data, 3},
		{"empty", nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := newStore(t)
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "r1"), 0o700))
			require.NoError(t, os.WriteFile(resultsPath(dir, "r1"), tc.contents, 0o600))

			begin(t, s, "r1")
			require.NoError(t, s.Append(t.Context(), "r1", result("new", 1, domain.StatusCorrect)))

			rows, err := s.Load(t.Context(), "r1")
			require.NoError(t, err)
			assert.Len(t, rows, tc.wantRows+1)
			assert.Equal(t, "new", rows[len(rows)-1].TaskID)
			after, _ := os.ReadFile(resultsPath(dir, "r1"))
			for line := range bytes.Lines(after) {
				assert.True(t, bytes.HasSuffix(line, []byte("\n")), "every row is on a line of its own")
				var r domain.Result
				require.NoError(t, json.Unmarshal(line, &r), "and none of them is damaged: %q", line)
			}
		})
	}
}

func TestIndex(t *testing.T) {
	s, _ := newStore(t)
	begin(t, s, "r1")
	for _, r := range []domain.Result{
		result("a", 1, domain.StatusCorrect), result("a", 2, domain.StatusTimeout),
		result("b", 1, domain.StatusSkipped), result("c", 1, domain.StatusUIError),
		result("d", 1, domain.StatusAgentError), result("e", 1, domain.StatusNoFinalAnswer),
		// asked again after the page broke it, and answered
		result("f", 1, domain.StatusUIError), result("f", 1, domain.StatusIncorrect),
	} {
		require.NoError(t, s.Append(t.Context(), "r1", r))
	}

	got, err := s.Index(t.Context(), "r1")

	require.NoError(t, err)
	want := map[domain.AttemptKey]struct{}{
		{TaskID: "a", Attempt: 1}: {}, {TaskID: "a", Attempt: 2}: {}, {TaskID: "b", Attempt: 1}: {},
		{TaskID: "d", Attempt: 1}: {}, {TaskID: "e", Attempt: 1}: {}, {TaskID: "f", Attempt: 1}: {},
	}
	assert.Equal(t, want, got, "an agent's failure is an answer, and resuming does not ask again; "+
		"an attempt the page broke is not one, and resuming asks it again")
}

func TestAppend_ManyWorkersAtOnce(t *testing.T) {
	s, _ := newStore(t)
	begin(t, s, "r1")
	const workers, each = 8, 25

	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range each {
				row := result(fmt.Sprintf("w%d-t%d", w, i), 1, domain.StatusCorrect)
				row.Evidence = []string{strings.Repeat("x", 3000)} // long enough that interleaving would show
				assert.NoError(t, s.Append(t.Context(), "r1", row))
			}
		})
	}
	wg.Wait()

	rows, err := s.Load(t.Context(), "r1")
	require.NoError(t, err, "no line was torn")
	assert.Len(t, rows, workers*each)
	index, _ := s.Index(t.Context(), "r1")
	assert.Len(t, index, workers*each, "and none was lost or repeated")
}

// run.json says whether the results file may hold question and answer text. A run
// resumed without --include-text still holds the text of its earlier session, and
// the file must not say otherwise; the rows of the new session hold none.
func TestBegin_ResumeKeepsWhetherTheRunHoldsText(t *testing.T) {
	s, _ := newStore(t)
	begin(t, s, "r1", includeText)
	first := result("a", 1, domain.StatusIncorrect)
	first.Question, first.Answer = "Q?", "A"
	require.NoError(t, s.Append(t.Context(), "r1", first))

	begin(t, s, "r1") // resumed without --include-text
	second := result("b", 1, domain.StatusIncorrect)
	second.Question, second.Answer = "Q2?", "A2"
	require.NoError(t, s.Append(t.Context(), "r1", second))

	meta, err := s.LoadMeta(t.Context(), "r1")
	require.NoError(t, err)
	assert.True(t, meta.IncludeText, "the earlier rows hold text, and run.json says the run can")
	rows, err := s.Load(t.Context(), "r1")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "Q?", rows[0].Question, "the earlier row keeps its text")
	assert.Empty(t, rows[1].Question, "and the new one holds none, as the new session asked")

	t.Run("a run that never held text is not said to", func(t *testing.T) {
		begin(t, s, "r2")
		begin(t, s, "r2")

		meta, err := s.LoadMeta(t.Context(), "r2")
		require.NoError(t, err)
		assert.False(t, meta.IncludeText)
	})
}

func TestBegin_Resume(t *testing.T) {
	s, _ := newStore(t)
	first := begin(t, s, "r1", func(m *domain.RunMeta) { m.Settings = map[string]string{"EVAL_WORKERS": "1"} })
	require.NoError(t, s.Append(t.Context(), "r1", result("a", 1, domain.StatusCorrect)))

	later := first
	later.StartedAt = first.StartedAt.Add(2 * time.Hour)
	later.Settings = map[string]string{"EVAL_WORKERS": "4"}
	require.NoError(t, s.Begin(t.Context(), later))

	got, err := s.LoadMeta(t.Context(), "r1")
	require.NoError(t, err)
	assert.Equal(t, first.StartedAt, got.StartedAt, "the run keeps the time it first started")
	assert.Equal(t, "4", got.Settings["EVAL_WORKERS"], "and the settings of the latest session")
	rows, _ := s.Load(t.Context(), "r1")
	assert.Len(t, rows, 1, "the finished attempt is still there")
}

func TestFinish(t *testing.T) {
	s, _ := newStore(t)
	meta := begin(t, s, "r1")

	meta.FinishedAt = meta.StartedAt.Add(11 * time.Minute)
	require.NoError(t, s.Finish(t.Context(), meta))

	got, err := s.LoadMeta(t.Context(), "r1")
	require.NoError(t, err)
	assert.Equal(t, meta.FinishedAt, got.FinishedAt)

	t.Run("a run that does not exist", func(t *testing.T) {
		err := s.Finish(t.Context(), domain.RunMeta{RunID: "ghost"})

		require.ErrorIs(t, err, resource.ErrRunNotFound)
	})
}

func TestMissingRuns(t *testing.T) {
	s, dir := newStore(t)
	ctx := t.Context()

	_, err := s.Load(ctx, "ghost")
	require.ErrorIs(t, err, resource.ErrRunNotFound)
	_, err = s.Index(ctx, "ghost")
	require.ErrorIs(t, err, resource.ErrRunNotFound)
	_, err = s.LoadMeta(ctx, "ghost")
	require.ErrorIs(t, err, resource.ErrRunNotFound)
	_, err = s.LoadProbes(ctx, "ghost")
	require.ErrorIs(t, err, resource.ErrRunNotFound)
	require.ErrorIs(t, s.SaveProbes(ctx, "ghost", nil), resource.ErrRunNotFound)
	require.ErrorIs(t, s.WriteSummary(ctx, domain.RunMeta{RunID: "ghost"}, domain.Summary{}), resource.ErrRunNotFound)
	_, err = s.EvidencePath(ctx, "ghost", "x.png")
	require.ErrorIs(t, err, resource.ErrRunNotFound)

	t.Run("a directory with no run.json is not a run to report on", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "stray"), 0o700))

		_, err := s.LoadMeta(ctx, "stray")

		require.ErrorIs(t, err, resource.ErrRunNotFound)
		assert.ErrorContains(t, err, "run.json")
	})
	t.Run("a run.json that is damaged says so", func(t *testing.T) {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "broken"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "broken", "run.json"), []byte("{nope"), 0o600))

		_, err := s.LoadMeta(ctx, "broken")

		require.Error(t, err)
		assert.False(t, errors.Is(err, resource.ErrRunNotFound))
		assert.ErrorContains(t, err, "run.json")
	})
}

func TestRunJSON_HasNoSecrets(t *testing.T) {
	const canary = "CANARY-9f3a1c"
	s, dir := newStore(t)
	meta := begin(t, s, "r1", func(m *domain.RunMeta) {
		m.Settings = map[string]string{
			"EVAL_WORKERS": "2", "EVAL_PASSWORD": canary + "-pw", "HF_TOKEN": canary + "-hf", "SESSION_COOKIE": canary + "-ck",
			"OPENAI_API_KEY": canary + "-key", "client_secret": canary + "-cs", "Db-Passwd": canary + "-db",
		}
		m.Versions = map[string]string{"go": "1.27", "auth_token": canary + "-v"}
	})
	meta.FinishedAt = meta.StartedAt.Add(time.Minute)
	require.NoError(t, s.Finish(t.Context(), meta))
	require.NoError(t, s.WriteSummary(t.Context(), meta, domain.Summary{RunID: "r1"}))

	got, err := s.LoadMeta(t.Context(), "r1")

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"EVAL_WORKERS": "2"}, got.Settings, "settings that are not secret stay")
	assert.Equal(t, map[string]string{"go": "1.27"}, got.Versions)
	assertNoneContain(t, dir, canary)
}

// assertNoneContain scans every file under dir for text.
func assertNoneContain(t *testing.T, dir, text string) {
	t.Helper()
	scanned := 0
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		scanned++
		content, readErr := os.ReadFile(p)
		require.NoError(t, readErr)
		assert.NotContains(t, string(content), text, p)
		assert.NotContains(t, filepath.Base(p), text)
		return nil
	}))
	assert.Positive(t, scanned, "there were files to scan")
}

func TestProbes(t *testing.T) {
	s, _ := newStore(t)
	begin(t, s, "r1")

	t.Run("none kept yet is not an error", func(t *testing.T) {
		got, err := s.LoadProbes(t.Context(), "r1")

		require.NoError(t, err)
		assert.Empty(t, got)
	})
	t.Run("what was saved comes back, and a second save replaces it", func(t *testing.T) {
		first := []domain.ProbeOutcome{{ID: domain.P01, Level: domain.Must, Result: domain.Pass, Measures: map[string]float64{"stop_ms": 312}}}
		require.NoError(t, s.SaveProbes(t.Context(), "r1", first))
		got, err := s.LoadProbes(t.Context(), "r1")
		require.NoError(t, err)
		assert.Equal(t, first, got)

		second := []domain.ProbeOutcome{{ID: domain.P04, Level: domain.Should, Result: domain.Fail, Detail: "horizontal scroll at 375 px", Evidence: []string{"p04.png"}}}
		require.NoError(t, s.SaveProbes(t.Context(), "r1", second))
		got, err = s.LoadProbes(t.Context(), "r1")
		require.NoError(t, err)
		assert.Equal(t, second, got)
	})
}

func TestEvidencePath(t *testing.T) {
	s, dir := newStore(t)
	begin(t, s, "r1")

	got, err := s.EvidencePath(t.Context(), "r1", "t1-1-fail.png")

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "r1", "evidence", "t1-1-fail.png"), got)
	assert.DirExists(t, filepath.Join(dir, "r1", "evidence"))

	for _, bad := range []string{"", "../x.png", "a/b.png", `a\b.png`, ".hidden", ".", ".."} {
		t.Run(fmt.Sprintf("refuses %q", bad), func(t *testing.T) {
			_, err := s.EvidencePath(t.Context(), "r1", bad)

			require.Error(t, err)
		})
	}
}

// --- summaries ---------------------------------------------------------------------

func fixedSummary() (domain.RunMeta, domain.Summary) {
	ms := func(v int64) *int64 { return &v }
	pass := func(extra ...[2]string) map[domain.CheckID]domain.CheckResult {
		m := map[domain.CheckID]domain.CheckResult{}
		for _, id := range domain.PassiveChecks {
			m[id] = domain.Pass
		}
		for _, e := range extra {
			m[domain.CheckID(e[0])] = domain.CheckResult(e[1])
		}
		return m
	}
	row := func(task string, level, attempt int, status domain.AttemptStatus, mutate func(*domain.Result)) domain.Result {
		r := domain.Result{RunID: "20261002T153012Z-ab12", TaskID: task, Level: level, Attempt: attempt, Status: status}
		if status == domain.StatusCorrect {
			r.Score = 1
		}
		if mutate != nil {
			mutate(&r)
		}
		return r
	}
	rows := []domain.Result{
		row("t1", 1, 1, domain.StatusCorrect, func(r *domain.Result) {
			r.LatencyMs, r.TTFTUIMs, r.UIOverheadMs, r.MaxGapMs, r.Updates = ms(1000), ms(150), ms(20), ms(100), 10
			r.Tokens, r.ToolCalls, r.UIChecks = domain.Tokens{Prompt: 100, Completion: 20}, []string{"search", "calc"}, pass()
		}),
		row("t2", 1, 1, domain.StatusCorrect, func(r *domain.Result) {
			r.LatencyMs, r.TTFTUIMs, r.UIOverheadMs, r.MaxGapMs, r.Updates = ms(2000), ms(250), ms(40), ms(200), 30
			r.Tokens, r.UIChecks = domain.Tokens{Prompt: 200, Completion: 40}, pass()
		}),
		row("t3", 1, 1, domain.StatusIncorrect, func(r *domain.Result) {
			r.LatencyMs, r.TTFTUIMs, r.UIOverheadMs, r.Updates = ms(3000), ms(350), ms(60), 20
			r.Tokens, r.ToolCalls, r.UIChecks = domain.Tokens{Prompt: 300, Completion: 60}, []string{"search"}, pass([2]string{"U-04", "na"}, [2]string{"U-06", "fail"})
		}),
		row("t4", 1, 1, domain.StatusSkipped, nil),
		row("t5", 2, 1, domain.StatusCorrect, func(r *domain.Result) {
			r.LatencyMs, r.TTFTUIMs, r.UIOverheadMs, r.Updates = ms(4000), ms(450), ms(80), 40
			r.UIChecks = pass([2]string{"U-07", "na"})
		}),
		row("t6", 2, 1, domain.StatusNoFinalAnswer, func(r *domain.Result) {
			r.LatencyMs, r.TTFTUIMs, r.UIOverheadMs = ms(5000), ms(550), ms(120)
			r.UIChecks = pass([2]string{"U-06", "na"}, [2]string{"U-05", "fail"})
		}),
		row("t7", 2, 1, domain.StatusAgentError, func(r *domain.Result) {
			r.LatencyMs, r.UIChecks = ms(600), map[domain.CheckID]domain.CheckResult{domain.U01: domain.Pass, domain.U02: domain.Pass, domain.U03: domain.Pass}
		}),
		row("t8", 3, 1, domain.StatusTimeout, func(r *domain.Result) {
			r.LatencyMs, r.UIChecks = ms(60000), map[domain.CheckID]domain.CheckResult{domain.U03: domain.Fail}
		}),
		row("t9", 3, 1, domain.StatusUIError, nil),
		row("t10", 3, 1, domain.StatusSkipped, nil),
	}
	probes := []domain.ProbeOutcome{
		{ID: domain.P04, Level: domain.Should, Result: domain.Fail, Detail: "horizontal scroll | at 375 px\nsecond line"},
		{ID: domain.P01, Level: domain.Must, Result: domain.Pass, Detail: "stopped in 312 ms"},
	}
	meta := domain.RunMeta{
		RunID: "20261002T153012Z-ab12", Command: "run",
		StartedAt: time.Date(2026, 10, 2, 15, 30, 12, 0, time.UTC), FinishedAt: time.Date(2026, 10, 2, 15, 41, 3, 0, time.UTC),
		GitSHA: "abc1234", UIContract: "1",
		Versions: map[string]string{"go": "1.27", "chrome-devtools-mcp": "1.10.1", "chrome": "143.0.7499.0"},
		Settings: map[string]string{"EVAL_REPEATS": "1", "EVAL_WORKERS": "2", "GAIA_LEVEL": "1", "HF_TOKEN": "must-not-show"},
	}
	return meta, report.Aggregate(report.Input{RunID: meta.RunID, Rows: rows, Probes: probes})
}

func TestWriteSummary(t *testing.T) {
	s, dir := newStore(t)
	meta, summary := fixedSummary()
	begin(t, s, meta.RunID)

	require.NoError(t, s.WriteSummary(t.Context(), meta, summary))
	md, err := os.ReadFile(filepath.Join(dir, meta.RunID, "summary.md"))
	require.NoError(t, err)
	js, err := os.ReadFile(filepath.Join(dir, meta.RunID, "summary.json"))
	require.NoError(t, err)

	t.Run("summary.md matches the golden file", func(t *testing.T) {
		golden(t, "summary.golden.md", string(md))
	})
	t.Run("summary.json reads back to the same summary", func(t *testing.T) {
		var back domain.Summary
		require.NoError(t, json.Unmarshal(js, &back))
		assert.Equal(t, summary, back)
	})
	t.Run("the same summary gives the same bytes", func(t *testing.T) {
		require.NoError(t, s.WriteSummary(t.Context(), meta, summary))

		md2, _ := os.ReadFile(filepath.Join(dir, meta.RunID, "summary.md"))
		js2, _ := os.ReadFile(filepath.Join(dir, meta.RunID, "summary.json"))
		assert.Equal(t, md, md2)
		assert.Equal(t, js, js2, "map keys are written in order")
	})
	t.Run("no temporary files are left behind", func(t *testing.T) {
		entries, err := os.ReadDir(filepath.Join(dir, meta.RunID))
		require.NoError(t, err)
		for _, e := range entries {
			assert.NotContains(t, e.Name(), ".tmp-")
		}
	})
	t.Run("a secret-looking setting is not shown", func(t *testing.T) {
		assert.NotContains(t, string(md), "must-not-show")
		assert.NotContains(t, string(md), "HF_TOKEN")
		assert.Contains(t, string(md), "EVAL_WORKERS")
	})
	t.Run("the summary of a run with no overhead names none", func(t *testing.T) {
		checks := map[domain.CheckID]domain.CheckResult{}
		for _, id := range domain.PassiveChecks {
			checks[id] = domain.Pass
		}
		checks[domain.U05] = domain.NA // the reply carried no time to first token to compare with
		bare := domain.RunMeta{RunID: "bare", Command: "run"}
		begin(t, s, bare.RunID)
		rows := []domain.Result{{TaskID: "a", Attempt: 1, Level: 1, Status: domain.StatusCorrect, UIChecks: checks}}

		require.NoError(t, s.WriteSummary(t.Context(), bare, report.Aggregate(report.Input{RunID: bare.RunID, Rows: rows})))

		forPeople, err := os.ReadFile(filepath.Join(dir, bare.RunID, "summary.md"))
		require.NoError(t, err)
		assert.Contains(t, string(forPeople), "| U-05 UI overhead | must | 0 | 0 | 1 | na |")
		assert.Contains(t, string(forPeople), "U-05 is not judged, because no attempt measured the overhead.")
		assert.NotContains(t, string(forPeople), "median")
		assert.NotContains(t, string(forPeople), "95th percentile")
		forPrograms, err := os.ReadFile(filepath.Join(dir, bare.RunID, "summary.json"))
		require.NoError(t, err)
		assert.Contains(t, string(forPrograms), `"ui_overhead_ms"`, "the member for the overhead is there, with nothing counted")
		assert.NotContains(t, string(forPrograms), "ui_overhead_basis")
	})
}

func TestSummaries_HoldNoQuestionOrAnswerText(t *testing.T) {
	s, dir := newStore(t)
	begin(t, s, "r1", includeText)
	row := result("t1", 1, domain.StatusIncorrect)
	row.Question, row.Truth, row.Answer = "QUESTION-CANARY?", "TRUTH-CANARY", "ANSWER-CANARY"
	require.NoError(t, s.Append(t.Context(), "r1", row))
	rows, err := s.Load(t.Context(), "r1")
	require.NoError(t, err)
	require.Equal(t, "ANSWER-CANARY", rows[0].Answer, "the results file does hold it, as asked")

	meta := domain.RunMeta{RunID: "r1", Command: "run"}
	require.NoError(t, s.WriteSummary(t.Context(), meta, report.Aggregate(report.Input{RunID: "r1", Rows: rows})))

	for _, name := range []string{"summary.json", "summary.md", "run.json"} {
		data, err := os.ReadFile(filepath.Join(dir, "r1", name))
		require.NoError(t, err)
		assert.NotContains(t, string(data), "CANARY", name)
	}
}

func TestRenderMarkdown_Edges(t *testing.T) {
	meta := domain.RunMeta{RunID: "r1", Command: "selftest"}

	t.Run("a run with nothing in it", func(t *testing.T) {
		md := store.RenderMarkdown(meta, report.Aggregate(report.Input{RunID: "r1"}))

		assert.Contains(t, md, "| **All** | 0 | 0 | 0 | n/a | n/a | n/a | n/a |")
		assert.Contains(t, md, "**Coverage:** 0.0% (0 of 0 asked")
		assert.Contains(t, md, "**Statuses:** none")
		assert.Contains(t, md, "no replies to judge")
		assert.Contains(t, md, "no replies timed")
		assert.Contains(t, md, "**Tokens:** not reported by the agent")
		assert.Contains(t, md, "**Tool calls:** none reported")
		assert.NotContains(t, md, "all Must checks passed", "nothing was checked, so nothing passed")
		assert.Contains(t, md, "no Must check failed, but these were not decided: U-01, U-02, U-03, U-04, U-05, U-06, U-07, U-08")
		assert.NotContains(t, md, "NaN")
		assert.NotContains(t, md, "Inf")
	})
	t.Run("a run that decided everything says all Must checks passed", func(t *testing.T) {
		ms := func(v int64) *int64 { return &v }
		pass := map[domain.CheckID]domain.CheckResult{}
		for _, id := range domain.PassiveChecks {
			pass[id] = domain.Pass
		}
		rows := []domain.Result{{TaskID: "a", Attempt: 1, Level: 1, Status: domain.StatusCorrect, UIChecks: pass, UIOverheadMs: ms(20)}}

		md := store.RenderMarkdown(meta, report.Aggregate(report.Input{Rows: rows}))

		assert.Contains(t, md, "**Result:** all Must checks passed.")
	})
	t.Run("attempts the page broke are named", func(t *testing.T) {
		rows := []domain.Result{
			{TaskID: "a", Attempt: 1, Level: 1, Status: domain.StatusUIError, ErrorClass: "start_chat"},
			{TaskID: "b", Attempt: 1, Level: 1, Status: domain.StatusUIError, ErrorClass: "start_chat"},
			{TaskID: "c", Attempt: 1, Level: 1, Status: domain.StatusUIError, ErrorClass: "submit"},
		}

		md := store.RenderMarkdown(meta, report.Aggregate(report.Input{Rows: rows}))

		assert.Contains(t, md, "The page broke 3 attempts: start_chat 2 · submit 1.")
		assert.NotContains(t, md, "all Must checks passed")
	})
	t.Run("a run that is not finished says so", func(t *testing.T) {
		m := meta
		m.StartedAt = time.Date(2026, 10, 2, 15, 30, 12, 0, time.UTC)

		assert.Contains(t, store.RenderMarkdown(m, domain.Summary{}), "2026-10-02 15:30:12 UTC, not finished")
	})
	t.Run("repeats are described", func(t *testing.T) {
		rows := []domain.Result{
			result("a", 1, domain.StatusCorrect), result("a", 2, domain.StatusIncorrect),
			result("b", 1, domain.StatusCorrect), result("b", 2, domain.StatusCorrect),
		}

		md := store.RenderMarkdown(meta, report.Aggregate(report.Input{Rows: rows}))

		assert.Contains(t, md, "**Repeatability** (2 repeats, 2 problems): mean accuracy 75.0% ± 35.4 pp")
		assert.Contains(t, md, "1 problems flipped between right and wrong (50.0%)")
	})
	t.Run("a status this version does not know is still listed", func(t *testing.T) {
		md := store.RenderMarkdown(meta, report.Aggregate(report.Input{Rows: []domain.Result{{TaskID: "a", Attempt: 1, Level: 1, Status: "odd"}}}))

		assert.Contains(t, md, "odd 1")
	})
	t.Run("times read as a person would", func(t *testing.T) {
		ms := func(v int64) *int64 { return &v }
		rows := []domain.Result{
			{TaskID: "a", Attempt: 1, Level: 1, Status: domain.StatusCorrect, LatencyMs: ms(850)},
			{TaskID: "b", Attempt: 1, Level: 1, Status: domain.StatusCorrect, LatencyMs: ms(12500)},
		}

		md := store.RenderMarkdown(meta, report.Aggregate(report.Input{Rows: rows}))

		assert.Contains(t, md, "p50 850 ms · p95 12.5 s")
	})
	t.Run("long probe details are cut and tables stay whole", func(t *testing.T) {
		probes := []domain.ProbeOutcome{{ID: domain.P03, Result: domain.Fail, Detail: strings.Repeat("a|b ", 100)}}

		md := store.RenderMarkdown(meta, report.Aggregate(report.Input{Probes: probes}))

		found := false
		for line := range strings.SplitSeq(md, "\n") {
			if strings.HasPrefix(line, "| P-03") {
				found = true
				assert.Equal(t, 5, strings.Count(strings.ReplaceAll(line, `\|`, ""), "|"), "four cells have five bars: %s", line)
				assert.Contains(t, line, "…")
			}
		}
		assert.True(t, found, "the probe row is there")
		assert.Contains(t, md, "Must checks failed: P-03")
	})
}
