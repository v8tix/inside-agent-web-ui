package run_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/run"
)

func recordedRun() *memStore {
	s := newMemStore()
	thresholds := domain.DefaultThresholds()
	thresholds.OverheadP95Ms = 30
	s.metas["r1"] = domain.RunMeta{RunID: "r1", Command: "run", Thresholds: thresholds}
	overhead := int64(50)
	s.rows["r1"] = []domain.Result{
		{TaskID: "a", Attempt: 1, Level: 1, Status: domain.StatusCorrect, UIOverheadMs: &overhead},
		{TaskID: "b", Attempt: 1, Level: 1, Status: domain.StatusIncorrect},
	}
	s.probes["r1"] = []domain.ProbeOutcome{{ID: domain.P01, Level: domain.Must, Result: domain.Fail}}
	return s
}

func TestReporter(t *testing.T) {
	t.Run("rebuilds the summary and writes it", func(t *testing.T) {
		s := recordedRun()
		r := &run.Reporter{Store: s, Thresholds: domain.DefaultThresholds()}

		got, err := r.Report(t.Context(), "r1")

		require.NoError(t, err)
		assert.Equal(t, [3]int{2, 2, 1}, [3]int{got.Agent.Eligible, got.Agent.Attempted, got.Agent.Correct})
		assert.Equal(t, []domain.CheckID{domain.P01, domain.U05}, got.UI.MustFailed, "the probe outcomes are included, and the limits are the run's own (30 ms, not 100)")
		assert.Equal(t, float64(30), got.Thresholds.OverheadP95Ms)
		require.Len(t, s.summaries, 1, "and written again")
		assert.Equal(t, got, s.summaries[0])
	})
	t.Run("a run that recorded no limits is judged by the given ones", func(t *testing.T) {
		s := recordedRun()
		meta := s.metas["r1"]
		meta.Thresholds = domain.Thresholds{}
		s.metas["r1"] = meta
		custom := domain.DefaultThresholds()
		custom.OverheadP95Ms = 40
		r := &run.Reporter{Store: s, Thresholds: custom}

		got, err := r.Report(t.Context(), "r1")

		require.NoError(t, err)
		assert.Equal(t, float64(40), got.Thresholds.OverheadP95Ms)
	})
	t.Run("what can go wrong is said", func(t *testing.T) {
		boom := errors.New("disk on fire")
		for method, want := range map[string]string{"Load": "reading the results", "LoadProbes": "reading the probe outcomes", "WriteSummary": "writing the summary"} {
			s := recordedRun()
			s.fail[method] = boom
			r := &run.Reporter{Store: s}

			_, err := r.Report(t.Context(), "r1")

			require.ErrorIs(t, err, boom, method)
			assert.ErrorContains(t, err, want)
		}
	})
	t.Run("a run that is not there", func(t *testing.T) {
		s := newMemStore()
		s.fail["LoadMeta"] = errors.New("no such run")
		r := &run.Reporter{Store: s}

		_, err := r.Report(t.Context(), "ghost")

		require.ErrorContains(t, err, "no such run")
	})
}
