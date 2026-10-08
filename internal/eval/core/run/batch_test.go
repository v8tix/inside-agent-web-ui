package run_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/run"
)

var startedAt = time.Date(2026, 10, 2, 15, 30, 0, 0, time.UTC)

// answersFor makes the pretend agent answer every problem with its truth.
func answersFor(src slice) map[string]string {
	answers := map[string]string{}
	for _, p := range src {
		answers[p.Question] = "Working it out.\nFINAL ANSWER: " + p.Truth
	}
	return answers
}

func request(mutate ...func(*service.RunRequest)) service.RunRequest {
	req := service.RunRequest{
		RunID: "run-1", Command: "run",
		Filter:  service.Filter{Attachments: domain.AttachmentsSkip},
		Repeats: 1, Workers: 1, Timeout: time.Minute,
		Email: "eval@example.com", Password: "pw",
	}
	for _, m := range mutate {
		m(&req)
	}
	return req
}

type setup struct {
	runner  *run.Runner
	browser *browser
	store   *memStore
	logs    *bytes.Buffer
}

func newSetup(src slice, mutate ...func(*setup)) *setup {
	s := &setup{browser: newBrowser(answersFor(src)), store: newMemStore(), logs: &bytes.Buffer{}}
	s.runner = &run.Runner{
		Source: src, Drivers: s.browser, Store: s.store,
		Thresholds: domain.DefaultThresholds(),
		Info:       run.RunInfo{GitSHA: "abc1234", UIContract: "1", Versions: map[string]string{"go": "1.27"}},
		Logger:     slog.New(slog.NewTextHandler(s.logs, nil)),
		Now:        func() time.Time { return startedAt },
	}
	for _, m := range mutate {
		m(s)
	}
	return s
}

func (s *setup) run(t *testing.T, req service.RunRequest) (service.Outcome, error) {
	t.Helper()
	return s.runner.Run(t.Context(), req)
}

func TestRun_AskingTheProblems(t *testing.T) {
	src := slice{newProblem("a", 1), newProblem("b", 2, withFile), newProblem("c", 3)}
	s := newSetup(src)

	out, err := s.run(t, request())

	require.NoError(t, err)
	assert.False(t, out.Interrupted)
	rows := s.store.results("run-1")
	assert.Equal(t, map[string]domain.AttemptStatus{"a#1": domain.StatusCorrect, "b#1": domain.StatusSkipped, "c#1": domain.StatusCorrect}, statuses(rows))
	assert.Equal(t, "attachment", rows[1].ErrorClass, "a skipped slot says why")
	assert.Equal(t, 2, rows[1].Level)

	t.Run("only what is not skipped was asked, in order", func(t *testing.T) {
		assert.Equal(t, []string{"Q a?", "Q c?"}, s.browser.questions())
	})
	t.Run("one browser, signed in, closed at the end", func(t *testing.T) {
		created, closed, logins := s.browser.counts()
		assert.Equal(t, [3]int{1, 1, 1}, [3]int{created, closed, logins})
	})
	t.Run("the summary counts the skipped one against coverage", func(t *testing.T) {
		a := out.Summary.Agent
		assert.Equal(t, [3]int{3, 2, 2}, [3]int{a.Eligible, a.Attempted, a.Correct})
		assert.InDelta(t, 2.0/3, a.Coverage, 1e-12)
		assert.Equal(t, "run-1", out.Summary.RunID)
		require.Len(t, s.store.summaries, 1, "and it was written")
		assert.Equal(t, out.Summary, s.store.summaries[0])
	})
	t.Run("the run is described before it starts and finished after", func(t *testing.T) {
		require.Len(t, s.store.began, 1)
		began := s.store.began[0]
		assert.Equal(t, domain.RunMeta{
			RunID: "run-1", Command: "run", StartedAt: startedAt, GitSHA: "abc1234", UIContract: "1",
			Versions:   map[string]string{"go": "1.27"},
			Settings:   map[string]string{"attachments": "skip", "repeats": "1", "workers": "1", "timeout": "1m0s", "keep_chats": "false"},
			Thresholds: domain.DefaultThresholds(),
		}, began)
		require.Len(t, s.store.finished, 1)
		assert.Equal(t, startedAt, s.store.finished[0].FinishedAt)
		assert.Equal(t, s.store.finished[0], out.Meta)
		assert.Equal(t, domain.DefaultThresholds(), out.Summary.Thresholds)
	})
}

func TestRun_Settings(t *testing.T) {
	src := slice{newProblem("a", 1)}
	s := newSetup(src)

	_, err := s.run(t, request(func(r *service.RunRequest) {
		r.Filter.Level, r.Filter.Limit, r.Filter.TaskIDs = 1, 5, []string{"a"}
		r.KeepChats, r.IncludeText = true, true
	}))

	require.NoError(t, err)
	got := s.store.began[0]
	assert.Equal(t, "1", got.Settings["level"])
	assert.Equal(t, "5", got.Settings["limit"])
	assert.Equal(t, "a", got.Settings["task_ids"])
	assert.Equal(t, "true", got.Settings["keep_chats"])
	assert.True(t, got.IncludeText)
	for k, v := range got.Settings {
		assert.NotContains(t, v, "pw", k)
		assert.NotContains(t, v, "@", k)
	}
	rows := s.store.results("run-1")
	require.Len(t, rows, 1)
	assert.Equal(t, "Q a?", rows[0].Question, "include-text reached the attempts")

	t.Run("how the prompt was typed is recorded", func(t *testing.T) {
		typed := newSetup(src)

		_, err := typed.run(t, request(func(r *service.RunRequest) { r.HumanTyping = true }))

		require.NoError(t, err)
		assert.Equal(t, "true", typed.store.began[0].Settings["human_typing"])
		assert.NotContains(t, got.Settings, "human_typing", "a run that fills the prompt in has no such setting")
	})
}

func TestRun_Repeats(t *testing.T) {
	src := slice{newProblem("a", 1), newProblem("b", 1, withFile), newProblem("c", 1)}
	s := newSetup(src)

	out, err := s.run(t, request(func(r *service.RunRequest) { r.Repeats = 3 }))

	require.NoError(t, err)
	assert.Equal(t, []string{"Q a?", "Q c?", "Q a?", "Q c?", "Q a?", "Q c?"}, s.browser.questions(),
		"every problem once, then every problem again: a run cut short has whole repeats first")
	rows := s.store.results("run-1")
	assert.Len(t, rows, 9, "the skipped problem has a row for each repeat too")
	assert.Equal(t, [3]int{9, 6, 6}, [3]int{out.Summary.Agent.Eligible, out.Summary.Agent.Attempted, out.Summary.Agent.Correct})
	require.NotNil(t, out.Summary.Agent.Repeatability)
	assert.Equal(t, 3, out.Summary.Agent.Repeatability.Repeats)
	assert.Zero(t, out.Summary.Agent.Repeatability.Flipped)
}

func TestRun_WorkersShareTheWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var src slice
		for i := range 8 {
			src = append(src, newProblem(fmt.Sprintf("p%d", i), 1))
		}
		s := newSetup(src, func(s *setup) { s.browser.latency = 10 * time.Second })
		start := time.Now()

		out, err := s.run(t, request(func(r *service.RunRequest) { r.Workers = 4 }))

		require.NoError(t, err)
		assert.Equal(t, 20*time.Second, time.Since(start), "eight replies of ten seconds on four browsers take two rounds")
		assert.Len(t, s.store.results("run-1"), 8)
		assert.Equal(t, 8, out.Summary.Agent.Correct)
		created, closed, logins := s.browser.counts()
		assert.Equal(t, [3]int{4, 4, 4}, [3]int{created, closed, logins}, "each worker has its own browser")
		seen := map[string]int{}
		for _, q := range s.browser.questions() {
			seen[q]++
		}
		assert.Len(t, seen, 8)
		for q, n := range seen {
			assert.Equal(t, 1, n, "%s was asked once", q)
		}
	})
	t.Run("never more browsers than there is work", func(t *testing.T) {
		src := slice{newProblem("a", 1), newProblem("b", 1)}
		s := newSetup(src)

		_, err := s.run(t, request(func(r *service.RunRequest) { r.Workers = 6 }))

		require.NoError(t, err)
		created, _, _ := s.browser.counts()
		assert.Equal(t, 2, created)
	})
}

func TestRun_NothingToAsk(t *testing.T) {
	src := slice{newProblem("a", 1, withFile), newProblem("b", 1, unscored)}
	s := newSetup(src)

	out, err := s.run(t, request())

	require.NoError(t, err)
	created, _, _ := s.browser.counts()
	assert.Zero(t, created, "no browser is started for a run that asks nothing")
	assert.Equal(t, map[string]domain.AttemptStatus{"a#1": domain.StatusSkipped, "b#1": domain.StatusSkipped}, statuses(s.store.results("run-1")))
	assert.Equal(t, [3]int{2, 0, 0}, [3]int{out.Summary.Agent.Eligible, out.Summary.Agent.Attempted, out.Summary.Agent.Correct})
	assert.Equal(t, "unscored", s.store.results("run-1")[1].ErrorClass)
}

// A level the dataset does not have, or a typo in a filter, selects nothing. A
// run of nothing would finish at once, say that all went well, and leave an empty
// run behind. It is a mistake in the request, and is reported before anything is
// made.
func TestRun_NothingMatchesTheFilters(t *testing.T) {
	src := slice{newProblem("a", 1), newProblem("b", 1)}

	tests := []struct {
		name   string
		filter service.Filter
		want   []string
	}{
		{"a level the dataset has none of", service.Filter{Level: 2}, []string{"level 2"}},
		{"a level and a task id that do not meet", service.Filter{Level: 3, TaskIDs: []string{"a"}}, []string{"level 3", "a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSetup(src)
			tc.filter.Attachments = domain.AttachmentsSkip

			_, err := s.run(t, request(func(r *service.RunRequest) { r.Filter = tc.filter }))

			var selection *run.SelectionError
			require.ErrorAs(t, err, &selection)
			for _, want := range tc.want {
				assert.Contains(t, err.Error(), want)
			}
			assert.Contains(t, err.Error(), "no problem matches")
			assert.Empty(t, s.store.began, "no run was begun for it")
			created, _, _ := s.browser.counts()
			assert.Zero(t, created)
		})
	}
	t.Run("an empty dataset", func(t *testing.T) {
		s := newSetup(slice{})

		_, err := s.run(t, request())

		var selection *run.SelectionError
		require.ErrorAs(t, err, &selection)
		assert.Contains(t, err.Error(), "no problem matches")
	})
}

func TestRun_Resume(t *testing.T) {
	src := slice{newProblem("a", 1), newProblem("b", 1, withFile), newProblem("c", 1), newProblem("d", 1)}
	s := newSetup(src)
	earlier := []domain.Result{
		{RunID: "run-1", TaskID: "a", Level: 1, Attempt: 1, Status: domain.StatusIncorrect}, // the first session's answer stands
		{RunID: "run-1", TaskID: "b", Level: 1, Attempt: 1, Status: domain.StatusSkipped, ErrorClass: "attachment"},
		{RunID: "run-1", TaskID: "c", Level: 1, Attempt: 1, Status: domain.StatusTimeout},
	}
	s.store.rows["run-1"] = earlier

	out, err := s.run(t, request())

	require.NoError(t, err)
	assert.Equal(t, []string{"Q d?"}, s.browser.questions(), "only what was not finished is asked, a timeout included")
	rows := s.store.results("run-1")
	assert.Len(t, rows, 4, "nothing is written twice")
	assert.Equal(t, domain.StatusIncorrect, statuses(rows)["a#1"], "and nothing is overwritten")
	assert.Equal(t, [3]int{4, 3, 1}, [3]int{out.Summary.Agent.Eligible, out.Summary.Agent.Attempted, out.Summary.Agent.Correct},
		"the summary covers both sessions")
	require.Len(t, s.store.began, 1, "the run is begun again, which resumes it")

	t.Run("the run keeps the time it first started, in run.json, the summary and the outcome", func(t *testing.T) {
		firstStart := startedAt.Add(-2 * time.Hour)
		resumed := newSetup(src)
		resumed.store.metas["run-1"] = domain.RunMeta{RunID: "run-1", StartedAt: firstStart}
		resumed.store.rows["run-1"] = earlier

		out, err := resumed.run(t, request())

		require.NoError(t, err)
		assert.Equal(t, firstStart, out.Meta.StartedAt, "the outcome")
		require.Len(t, resumed.store.finished, 1)
		assert.Equal(t, firstStart, resumed.store.finished[0].StartedAt, "run.json when the run is finished")
		require.Len(t, resumed.store.summarized, 1)
		assert.Equal(t, firstStart, resumed.store.summarized[0].StartedAt, "and the summary")
		assert.Equal(t, startedAt, out.Meta.FinishedAt, "while the end is when this session ended")
	})
	t.Run("a run that is already complete asks nothing and starts no browser", func(t *testing.T) {
		again := newSetup(src)
		again.store.rows["run-1"] = s.store.results("run-1")

		out, err := again.run(t, request())

		require.NoError(t, err)
		created, _, _ := again.browser.counts()
		assert.Zero(t, created)
		assert.Empty(t, again.browser.questions())
		assert.Equal(t, 4, out.Summary.Agent.Eligible)
		assert.False(t, out.Interrupted)
	})
	// run.json keeps the latest settings, so a resume with other ones would
	// quietly change what the run is.
	t.Run("resuming with other settings says what changed", func(t *testing.T) {
		resumed := newSetup(slice{newProblem("a", 1), newProblem("b", 2)})
		resumed.store.metas["run-1"] = domain.RunMeta{RunID: "run-1", StartedAt: startedAt.Add(-time.Hour), Settings: map[string]string{
			"level": "1", "repeats": "1", "workers": "1", "timeout": "1m0s", "attachments": "skip", "keep_chats": "false",
		}}
		resumed.store.rows["run-1"] = []domain.Result{{RunID: "run-1", TaskID: "a", Level: 1, Attempt: 1, Status: domain.StatusCorrect}}

		_, err := resumed.run(t, request(func(r *service.RunRequest) {
			r.Filter.Level = 2
			r.Filter.Limit = 5 // a setting the run did not have before
			r.Repeats = 3
			r.HumanTyping = true // the first session filled the prompt in
		}))

		require.NoError(t, err)
		logs := resumed.logs.String()
		assert.Contains(t, logs, "resuming a run with other settings")
		assert.Contains(t, logs, "level: 1 -> 2")
		assert.Contains(t, logs, "repeats: 1 -> 3")
		assert.Contains(t, logs, "limit: (unset) -> 5")
		assert.Contains(t, logs, "human_typing: (unset) -> true")
		assert.NotContains(t, logs, "workers:", "what did not change is not listed")
	})
	t.Run("resuming with the same settings says nothing", func(t *testing.T) {
		same := newSetup(src)
		first := request()
		_, err := same.run(t, first)
		require.NoError(t, err)
		same.logs.Reset()

		_, err = same.run(t, first)

		require.NoError(t, err)
		assert.NotContains(t, same.logs.String(), "other settings")
	})
	t.Run("a new run has nothing to compare with", func(t *testing.T) {
		fresh := newSetup(src)

		_, err := fresh.run(t, request())

		require.NoError(t, err)
		assert.NotContains(t, fresh.logs.String(), "other settings")
	})
	// A page that broke an attempt (the session ended, a tab crashed) says nothing
	// about the agent, and asking again is the only way to find out.
	t.Run("an attempt the page broke is asked again, and the new row stands", func(t *testing.T) {
		again := newSetup(src)
		again.store.rows["run-1"] = []domain.Result{
			{RunID: "run-1", TaskID: "a", Level: 1, Attempt: 1, Status: domain.StatusCorrect},
			{RunID: "run-1", TaskID: "b", Level: 1, Attempt: 1, Status: domain.StatusSkipped, ErrorClass: "attachment"},
			{RunID: "run-1", TaskID: "c", Level: 1, Attempt: 1, Status: domain.StatusUIError, ErrorClass: run.ClassStartChat},
			{RunID: "run-1", TaskID: "d", Level: 1, Attempt: 1, Status: domain.StatusUIError, ErrorClass: run.ClassDriverDied},
		}

		out, err := again.run(t, request())

		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"Q c?", "Q d?"}, again.browser.questions(), "only the two the page broke")
		assert.Empty(t, out.Summary.UI.UIErrors, "and once they are answered they no longer count as broken")
		assert.EqualValues(t, 3, out.Summary.Agent.Correct)
	})
	t.Run("a larger --repeats adds the missing attempts", func(t *testing.T) {
		more := newSetup(src)
		more.store.rows["run-1"] = s.store.results("run-1")

		_, err := more.run(t, request(func(r *service.RunRequest) { r.Repeats = 2 }))

		require.NoError(t, err)
		assert.Equal(t, []string{"Q a?", "Q c?", "Q d?"}, more.browser.questions(), "attempt 2 of every askable problem")
	})
}

func TestRun_CtrlC(t *testing.T) {
	src := slice{newProblem("a", 1), newProblem("b", 1), newProblem("c", 1)}
	ctx, cancel := context.WithCancel(t.Context())
	s := newSetup(src, func(s *setup) {
		s.browser.onAwait = func(_ context.Context, question string) {
			if question == "Q b?" {
				cancel() // Ctrl-C while the second problem is being answered
			}
		}
	})

	out, err := s.runner.Run(ctx, request())

	require.NoError(t, err, "being interrupted is not a failure")
	assert.True(t, out.Interrupted)
	assert.Equal(t, map[string]domain.AttemptStatus{"a#1": domain.StatusCorrect}, statuses(s.store.results("run-1")),
		"what finished is kept; the attempt in flight is not recorded, so --resume asks it again")
	assert.Equal(t, []string{"Q a?", "Q b?"}, s.browser.questions(), "and nothing new was started")
	assert.True(t, out.Meta.FinishedAt.IsZero(), "the run is not marked finished")
	assert.Empty(t, s.store.finished)
	require.Len(t, s.store.summaries, 1, "but the summary of what there is was written")
	assert.Equal(t, 1, out.Summary.Agent.Attempted)
	created, closed, _ := s.browser.counts()
	assert.Equal(t, created, closed, "the browser was closed even though the context was cancelled")

	t.Run("and resuming finishes the job", func(t *testing.T) {
		resumed := newSetup(src)
		resumed.store.rows["run-1"] = s.store.results("run-1")

		out, err := resumed.run(t, request())

		require.NoError(t, err)
		assert.False(t, out.Interrupted)
		assert.Equal(t, []string{"Q b?", "Q c?"}, resumed.browser.questions())
		assert.Equal(t, 3, out.Summary.Agent.Correct)
	})
	t.Run("a context that is already cancelled does nothing", func(t *testing.T) {
		done, stop := context.WithCancel(t.Context())
		stop()
		fresh := newSetup(src)

		out, err := fresh.runner.Run(done, request())

		require.NoError(t, err)
		assert.True(t, out.Interrupted)
		assert.Empty(t, fresh.browser.questions())
	})
}

func TestRun_NoProblemIsHandedOutAfterTheEnd(t *testing.T) {
	t.Run("a run that goes on hands out every slot", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			assert.Equal(t, 3, run.HandOutForTest(t.Context(), 3, synctest.Wait))
		})
	})
	t.Run("a context that has ended wins over a worker that is ready", func(t *testing.T) {
		// When a worker waits for a slot and the context has ended, a select can go
		// either way and picks at random. Tried a hundred times, a feeder that only
		// selects hands a slot out in about half of them.
		for range 100 {
			synctest.Test(t, func(t *testing.T) {
				ctx, stop := context.WithCancel(t.Context())
				stop()

				assert.Zero(t, run.HandOutForTest(ctx, 3, synctest.Wait), "a slot was handed out after the end")
			})
		}
	})
}

func TestRun_TheBrowserDies(t *testing.T) {
	src := slice{newProblem("a", 1), newProblem("b", 1), newProblem("c", 1)}

	t.Run("once: a new browser is started and the attempt tried again", func(t *testing.T) {
		s := newSetup(src)
		s.browser.dieOn["Q b?"] = 1

		out, err := s.run(t, request())

		require.NoError(t, err)
		assert.Equal(t, map[string]domain.AttemptStatus{"a#1": domain.StatusCorrect, "b#1": domain.StatusCorrect, "c#1": domain.StatusCorrect}, statuses(s.store.results("run-1")))
		assert.Equal(t, []string{"Q a?", "Q b?", "Q b?", "Q c?"}, s.browser.questions())
		created, closed, logins := s.browser.counts()
		assert.Equal(t, [3]int{2, 2, 2}, [3]int{created, closed, logins}, "the dead one was let go of, the new one signed in")
		assert.Equal(t, 3, out.Summary.Agent.Correct)
	})
	t.Run("twice on the same attempt: it becomes a failed row and the run goes on", func(t *testing.T) {
		s := newSetup(src)
		s.browser.dieOn["Q b?"] = 2

		out, err := s.run(t, request())

		require.NoError(t, err)
		rows := s.store.results("run-1")
		assert.Equal(t, domain.StatusUIError, statuses(rows)["b#1"])
		assert.Equal(t, run.ClassDriverDied, rows[1].ErrorClass)
		assert.Equal(t, domain.StatusCorrect, statuses(rows)["c#1"], "the next problem is asked on a fresh browser")
		created, closed, _ := s.browser.counts()
		assert.Equal(t, [2]int{3, 3}, [2]int{created, closed})
		assert.Equal(t, 1, out.Summary.Agent.Statuses[domain.StatusUIError])
	})
	t.Run("and no browser can be started again: the run cannot go on", func(t *testing.T) {
		s := newSetup(src)
		s.browser.dieOn["Q b?"] = 1
		s.browser.failNewAfter = 1

		out, err := s.run(t, request())

		require.Error(t, err)
		assert.ErrorContains(t, err, "starting a browser")
		assert.Equal(t, map[string]domain.AttemptStatus{"a#1": domain.StatusCorrect}, statuses(s.store.results("run-1")), "what was done is kept")
		assert.Equal(t, 1, out.Summary.Agent.Attempted, "and summarised")
		assert.Empty(t, s.store.finished, "but the run is not marked finished")
	})
	t.Run("twice, and no third browser can be started: the failed row is kept and the run cannot go on", func(t *testing.T) {
		s := newSetup(src)
		s.browser.dieOn["Q b?"] = 2
		s.browser.failNewAfter = 2

		out, err := s.run(t, request())

		require.Error(t, err)
		assert.ErrorContains(t, err, "starting a browser")
		rows := s.store.results("run-1")
		require.Equal(t, map[string]domain.AttemptStatus{"a#1": domain.StatusCorrect, "b#1": domain.StatusUIError}, statuses(rows),
			"the attempt both browsers died on is in the results")
		assert.Equal(t, run.ClassDriverDied, rows[1].ErrorClass)
		assert.Equal(t, 1, out.Summary.UI.UIErrors[run.ClassDriverDied], "and in the summary")
		assert.Empty(t, s.store.finished, "but the run is not marked finished")
	})
}

func TestRun_CannotGetStarted(t *testing.T) {
	src := slice{newProblem("a", 1), newProblem("b", 1)}

	t.Run("wrong credentials", func(t *testing.T) {
		s := newSetup(src)
		s.browser.loginErr = fmt.Errorf("the page said no: %w", domain.ErrLoginFailed)

		_, err := s.run(t, request())

		require.ErrorIs(t, err, domain.ErrLoginFailed)
		assert.ErrorContains(t, err, "signing in")
		assert.Empty(t, s.browser.questions())
		created, closed, _ := s.browser.counts()
		assert.Equal(t, created, closed, "the browser that could not sign in was closed")
	})
	t.Run("a page speaking another UI contract", func(t *testing.T) {
		s := newSetup(src)
		s.browser.loginErr = fmt.Errorf("data-ui-contract is 2: %w", domain.ErrContractVersion)

		_, err := s.run(t, request())

		require.ErrorIs(t, err, domain.ErrContractVersion)
	})
	t.Run("no browser", func(t *testing.T) {
		s := newSetup(src)
		s.browser.newErr = errors.New("chrome-devtools-mcp not found")

		_, err := s.run(t, request())

		require.ErrorContains(t, err, "starting a browser: chrome-devtools-mcp not found")
	})
	t.Run("one of several workers cannot start: the others stop too", func(t *testing.T) {
		s := newSetup(src)
		s.browser.failNewAfter = 1

		_, err := s.run(t, request(func(r *service.RunRequest) { r.Workers = 2 }))

		require.ErrorContains(t, err, "starting a browser")
	})
	t.Run("nowhere to write", func(t *testing.T) {
		s := newSetup(src)
		s.store.appendErr = errors.New("disk full")

		_, err := s.run(t, request())

		require.ErrorContains(t, err, "disk full")
	})
}

func TestRun_BadRequests(t *testing.T) {
	src := slice{newProblem("a", 1)}
	tests := []struct {
		name   string
		mutate func(*service.RunRequest)
		want   string
	}{
		{"no id", func(r *service.RunRequest) { r.RunID = "" }, "needs an id"},
		{"no repeats", func(r *service.RunRequest) { r.Repeats = 0 }, "repeats must be at least 1"},
		{"no workers", func(r *service.RunRequest) { r.Workers = 0 }, "workers must be at least 1"},
		{"no timeout", func(r *service.RunRequest) { r.Timeout = 0 }, "timeout must be positive"},
		{"no email", func(r *service.RunRequest) { r.Email = "" }, "email and password"},
		{"no password", func(r *service.RunRequest) { r.Password = "" }, "email and password"},
		{"a task id that does not exist", func(r *service.RunRequest) { r.Filter.TaskIDs = []string{"zzz"} }, "no such task id: zzz"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSetup(src)

			out, err := s.run(t, request(tc.mutate))

			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, service.Outcome{}, out)
			assert.Empty(t, s.store.began, "nothing was created")
			created, _, _ := s.browser.counts()
			assert.Zero(t, created)
		})
	}
	t.Run("a source that cannot be read", func(t *testing.T) {
		s := newSetup(src)
		s.runner.Source = failing{err: errors.New("disk on fire")}

		_, err := s.run(t, request())

		require.ErrorContains(t, err, "choosing the problems: disk on fire")
		assert.Empty(t, s.store.began)
	})
}

func TestRun_Logs(t *testing.T) {
	src := slice{newProblem("a", 1), newProblem("b", 1)}
	s := newSetup(src)

	_, err := s.run(t, request())

	require.NoError(t, err)
	logs := s.logs.String()
	assert.Contains(t, logs, "run starting")
	assert.Contains(t, logs, "progress=1/2")
	assert.Contains(t, logs, "progress=2/2")
	assert.Contains(t, logs, "status=correct")
	assert.NotContains(t, logs, "pw", "the password is not logged")
	assert.NotContains(t, logs, "eval@example.com", "nor is the user")
}

func TestRun_TheStoreFails(t *testing.T) {
	boom := errors.New("disk on fire")
	withFile := slice{newProblem("a", 1), newProblem("b", 1, withFile)}
	tests := []struct {
		method string
		want   string
	}{
		{"Begin", "starting the run"},
		{"Index", "reading the finished attempts"},
		{"Load", "reading the results"},
		{"LoadProbes", "reading the probe outcomes"},
		{"WriteSummary", "writing the summary"},
		{"Finish", "finishing the run"},
	}
	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			s := newSetup(withFile)
			s.store.fail[tc.method] = boom

			_, err := s.run(t, request())

			require.ErrorIs(t, err, boom)
			assert.ErrorContains(t, err, tc.want)
		})
	}
	t.Run("writing a skipped problem", func(t *testing.T) {
		s := newSetup(withFile)
		s.store.appendErr = boom

		_, err := s.run(t, request(func(r *service.RunRequest) { r.Filter.TaskIDs = []string{"b"} }))

		require.ErrorIs(t, err, boom)
		assert.ErrorContains(t, err, "recording a skipped problem")
	})
	t.Run("a failure while the run was already failing is reported with it", func(t *testing.T) {
		s := newSetup(withFile)
		s.browser.newErr = errors.New("no browser")
		s.store.fail["Load"] = boom

		_, err := s.run(t, request())

		require.ErrorIs(t, err, boom)
		assert.ErrorContains(t, err, "no browser")
	})
}

func TestRun_AProblemThatCannotBeAsked(t *testing.T) {
	broken := newProblem("a", 1)
	broken.Question = "" // the source let one through
	s := newSetup(slice{broken})

	_, err := s.run(t, request())

	require.ErrorContains(t, err, "has no question")
	assert.ErrorContains(t, err, "worker 1")
	assert.Empty(t, s.store.results("run-1"))
}

func TestRun_ClosingTheBrowserFailsOrNot(t *testing.T) {
	s := newSetup(slice{newProblem("a", 1)})
	s.browser.closeErr = errors.New("chrome already gone")

	out, err := s.run(t, request())

	require.NoError(t, err, "a browser that will not close is a warning")
	assert.Equal(t, 1, out.Summary.Agent.Correct)
	assert.Contains(t, s.logs.String(), "could not close the browser cleanly")
}

func TestRun_DefaultsForWhatIsLeftOut(t *testing.T) {
	before := time.Now()
	b := newBrowser(answersFor(slice{newProblem("a", 1)}))
	store := newMemStore()
	runner := &run.Runner{Source: slice{newProblem("a", 1)}, Drivers: b, Store: store} // no Logger, no Now

	out, err := runner.Run(t.Context(), request())

	require.NoError(t, err)
	assert.False(t, out.Meta.FinishedAt.Before(before), "the real clock")
	assert.Equal(t, domain.DefaultThresholds(), out.Summary.Thresholds, "and the default limits when none are given")
}

func TestRun_TwoWorkersAndAFailure(t *testing.T) {
	// One worker cannot go on while the other is partway through an attempt: the
	// run stops, and the attempt that was cut short is not recorded.
	synctest.Test(t, func(t *testing.T) {
		src := slice{newProblem("a", 1), newProblem("b", 1), newProblem("c", 1), newProblem("d", 1)}
		s := newSetup(src, func(s *setup) { s.browser.latency = time.Minute })
		s.browser.failNewAfter = 1
		s.browser.dieOn["Q a?"] = 1

		out, err := s.run(t, request(func(r *service.RunRequest) { r.Workers = 2; r.Timeout = 10 * time.Minute }))

		require.Error(t, err)
		assert.Zero(t, out.Summary.Agent.Correct, "nothing finished before the failure")
		assert.True(t, out.Meta.FinishedAt.IsZero())
	})
}

func TestRun_TheContextHasADeadline(t *testing.T) {
	// A deadline on the context is the same as Ctrl-C: the run stops, keeps what
	// it has, and says it was interrupted; it is not an error.
	synctest.Test(t, func(t *testing.T) {
		src := slice{newProblem("a", 1), newProblem("b", 1), newProblem("c", 1)}
		s := newSetup(src, func(s *setup) { s.browser.latency = time.Minute })
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()

		out, err := s.runner.Run(ctx, request(func(r *service.RunRequest) { r.Timeout = 10 * time.Minute }))

		require.NoError(t, err)
		assert.True(t, out.Interrupted)
		assert.Equal(t, map[string]domain.AttemptStatus{"a#1": domain.StatusCorrect}, statuses(s.store.results("run-1")),
			"the first reply took a minute and was recorded; the second was cut short")
		assert.True(t, out.Meta.FinishedAt.IsZero())
	})
}

func TestRun_TheEndOfARunIsNotWaitedForForever(t *testing.T) {
	// The summary is written with a context that Ctrl-C does not end, because a
	// run cut short still has rows worth a summary. A store that has stopped
	// answering must not hold the program for ever on that account.
	tests := []struct {
		name        string
		hangsOn     string
		interrupted bool
		want        string
	}{
		{"the summary of a run cut short", "WriteSummary", true, "writing the summary"},
		{"the summary of a run that finished", "WriteSummary", false, "writing the summary"},
		{"marking the run finished", "Finish", false, "finishing the run"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newSetup(slice{newProblem("a", 1)})
				s.runner.Store = hangingStore{memStore: s.store, on: tc.hangsOn}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.interrupted {
					cancel()
				}
				start := time.Now()

				_, err := s.runner.Run(ctx, request())

				require.ErrorIs(t, err, context.DeadlineExceeded)
				assert.ErrorContains(t, err, tc.want)
				assert.Equal(t, 10*time.Second, time.Since(start), "the closing is given ten seconds, like the browser's")
			})
		})
	}
}

func TestRun_ConversationsAreDeletedUnlessKept(t *testing.T) {
	src := slice{newProblem("a", 1), newProblem("b", 1)}

	t.Run("deleted", func(t *testing.T) {
		s := newSetup(src)

		_, err := s.run(t, request())

		require.NoError(t, err)
		assert.Equal(t, 2, s.browser.deleted)
	})
	t.Run("kept", func(t *testing.T) {
		s := newSetup(src)

		_, err := s.run(t, request(func(r *service.RunRequest) { r.KeepChats = true }))

		require.NoError(t, err)
		assert.Zero(t, s.browser.deleted)
	})
}

func TestRun_TheLimitsGivenAreTheLimitsUsed(t *testing.T) {
	s := newSetup(slice{newProblem("a", 1)})
	s.runner.Thresholds = domain.DefaultThresholds()
	s.runner.Thresholds.OverheadP95Ms = 7 // the healthy page adds 50 ms

	out, err := s.run(t, request())

	require.NoError(t, err)
	assert.InDelta(t, 7, out.Summary.Thresholds.OverheadP95Ms, 0)
	assert.Equal(t, domain.Fail, out.Summary.UI.Verdicts[domain.U05], "and they judged the attempt")
	assert.Contains(t, out.Summary.UI.MustFailed, domain.U05)
	assert.InDelta(t, 7, s.store.began[0].Thresholds.OverheadP95Ms, 0, "and run.json says which")
}

func TestRun_VersionsKnownOnlyAfterTheBrowserStarted(t *testing.T) {
	s := newSetup(slice{newProblem("a", 1)})
	s.runner.Versions = func() map[string]string {
		return map[string]string{"chrome": "143.0.7499.0", "go": "overridden"}
	}

	out, err := s.run(t, request())

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"go": "overridden", "chrome": "143.0.7499.0"}, out.Meta.Versions, "added to what was known, the later word winning")
	require.Len(t, s.store.finished, 1)
	assert.Equal(t, "143.0.7499.0", s.store.finished[0].Versions["chrome"], "and recorded in run.json")
	assert.Equal(t, "1.27", s.store.began[0].Versions["go"], "which began without it")

	t.Run("when nothing is known", func(t *testing.T) {
		s := newSetup(slice{newProblem("a", 1)})
		s.runner.Versions = func() map[string]string { return nil }

		out, err := s.run(t, request())

		require.NoError(t, err)
		assert.Equal(t, map[string]string{"go": "1.27"}, out.Meta.Versions)
	})
	t.Run("when there were no versions to begin with", func(t *testing.T) {
		s := newSetup(slice{newProblem("a", 1)})
		s.runner.Info.Versions = nil
		s.runner.Versions = func() map[string]string { return map[string]string{"chrome": "1"} }

		out, err := s.run(t, request())

		require.NoError(t, err)
		assert.Equal(t, map[string]string{"chrome": "1"}, out.Meta.Versions)
	})
}

func TestRun_ScreenshotsFollowTheRequest(t *testing.T) {
	failing := func() *setup {
		s := newSetup(slice{newProblem("a", 1)})
		s.browser.alwaysFailConsole = true
		return s
	}
	t.Run("kept when the request asks", func(t *testing.T) {
		s := failing()

		_, err := s.run(t, request(func(r *service.RunRequest) { r.Screenshots = true }))

		require.NoError(t, err)
		assert.Equal(t, 1, s.browser.shots)
	})
	t.Run("not kept otherwise", func(t *testing.T) {
		s := failing()

		_, err := s.run(t, request())

		require.NoError(t, err)
		assert.Zero(t, s.browser.shots)
	})
}
