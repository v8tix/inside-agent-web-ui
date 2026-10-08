package probe_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	resmocks "github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource/mocks"
	svcmocks "github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service/mocks"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/probe"
)

// scripted is a set of probes whose outcomes are given, which records the order
// they were run in.
type scripted struct {
	probes  *resmocks.UIProbes
	order   []domain.CheckID
	results map[domain.CheckID]func(context.Context) (domain.ProbeOutcome, error)
	closed  int
	// closing, when set, is what closing the browser does.
	closing func(context.Context) error
}

func newScripted(t *testing.T) *scripted {
	t.Helper()
	s := &scripted{probes: resmocks.NewUIProbes(t), results: map[domain.CheckID]func(context.Context) (domain.ProbeOutcome, error){}}
	methods := map[domain.CheckID]string{
		domain.P01: "Stop", domain.P02: "Regenerate", domain.P03: "Lighthouse", domain.P04: "Mobile", domain.P05: "RenameDelete",
		domain.P06: "Theme", domain.P07: "ReloadMidStream", domain.P08: "KeyboardOnly", domain.P09: "LayoutShift", domain.P10: "AgentDown",
	}
	for id, method := range methods {
		s.probes.On(method, mock.Anything).Run(func(mock.Arguments) { s.order = append(s.order, id) }).
			Return(func(ctx context.Context) (domain.ProbeOutcome, error) {
				if f := s.results[id]; f != nil {
					return f(ctx)
				}
				return domain.ProbeOutcome{ID: id, Level: id.Level(), Result: domain.Pass, Detail: "fine"}, nil
			}).Maybe()
	}
	s.probes.On("Close", mock.Anything).Run(func(mock.Arguments) { s.closed++ }).Return(func(ctx context.Context) error {
		if s.closing != nil {
			return s.closing(ctx)
		}
		return nil
	}).Maybe()
	return s
}

func (s *scripted) returns(id domain.CheckID, o domain.ProbeOutcome, err error) {
	s.results[id] = func(context.Context) (domain.ProbeOutcome, error) { return o, err }
}

// suiteFor wires the probes to a store and a reporter that remember what they got.
// earlier is what the run already holds: the outcomes of a suite that ran before.
func suiteFor(t *testing.T, s *scripted, earlier ...domain.ProbeOutcome) (*probe.Suite, *[]domain.ProbeOutcome, *domain.Summary) {
	t.Helper()
	var saved []domain.ProbeOutcome
	store := resmocks.NewResultStore(t)
	store.On("LoadProbes", mock.Anything, "r1").Return(earlier, nil).Maybe()
	store.On("SaveProbes", mock.Anything, "r1", mock.Anything).Run(func(args mock.Arguments) {
		saved = args.Get(2).([]domain.ProbeOutcome)
	}).Return(nil).Maybe()
	summary := &domain.Summary{RunID: "r1"}
	reporter := svcmocks.NewReporter(t)
	reporter.On("Report", mock.Anything, "r1").Return(func(context.Context, string) (domain.Summary, error) {
		summary.UI.Probes = saved
		return *summary, nil
	}).Maybe()
	return &probe.Suite{Probes: s.probes, Store: store, Reporter: reporter}, &saved, summary
}

func TestSuite_RunsEveryProbeInOrderAndKeepsTheOutcomes(t *testing.T) {
	s := newScripted(t)
	s.returns(domain.P04, domain.ProbeOutcome{Result: domain.Fail, Detail: "scrolls sideways"}, nil)
	suite, saved, _ := suiteFor(t, s)

	summary, err := suite.Probe(t.Context(), "r1")

	require.NoError(t, err)
	assert.Equal(t, probe.Order, s.order, "in the order made for them")
	assert.Equal(t, 1, s.closed, "the browser is closed at the end")
	require.Len(t, *saved, 10)
	assert.Equal(t, []domain.CheckID{domain.P01, domain.P02, domain.P03, domain.P04, domain.P05, domain.P06, domain.P07, domain.P08, domain.P09, domain.P10},
		func() []domain.CheckID {
			var ids []domain.CheckID
			for _, o := range *saved {
				ids = append(ids, o.ID)
			}
			return ids
		}(), "kept in id order")
	failed := (*saved)[3]
	assert.Equal(t, domain.P04, failed.ID, "the id and the level are the spec's, whatever the probe wrote")
	assert.Equal(t, domain.Should, failed.Level)
	assert.Equal(t, domain.Fail, failed.Result)
	assert.Len(t, summary.UI.Probes, 10, "and the summary was made again with them")
}

func TestSuite_AProbeThatCannotBeCarriedOut(t *testing.T) {
	s := newScripted(t)
	s.returns(domain.P02, domain.ProbeOutcome{}, errors.New("the page has no button named \"Regenerate response\"\n(it has: ...)"))
	suite, saved, _ := suiteFor(t, s)

	_, err := suite.Probe(t.Context(), "r1")

	require.NoError(t, err)
	assert.Len(t, s.order, 10, "the others went on")
	var p02 domain.ProbeOutcome
	for _, o := range *saved {
		if o.ID == domain.P02 {
			p02 = o
		}
	}
	assert.Equal(t, domain.NA, p02.Result, "it says nothing about the UI")
	assert.Equal(t, `could not be carried out: the page has no button named "Regenerate response" (it has: ...)`, p02.Detail)
}

func TestSuite_ALongReasonIsShortened(t *testing.T) {
	s := newScripted(t)
	s.returns(domain.P01, domain.ProbeOutcome{}, errors.New(strings.Repeat("x", 1000)))
	suite, saved, _ := suiteFor(t, s)

	_, err := suite.Probe(t.Context(), "r1")

	require.NoError(t, err)
	assert.LessOrEqual(t, len([]rune((*saved)[0].Detail)), 270)
	assert.True(t, strings.HasSuffix((*saved)[0].Detail, "…"))
}

func TestSuite_TheBrowserDies(t *testing.T) {
	s := newScripted(t)
	s.returns(domain.P02, domain.ProbeOutcome{}, fmt.Errorf("calling click: %w", domain.ErrDriverDied))
	suite, saved, _ := suiteFor(t, s)

	_, err := suite.Probe(t.Context(), "r1")

	require.NoError(t, err)
	assert.Equal(t, []domain.CheckID{domain.P01, domain.P02}, s.order, "no probe was tried on a dead browser")
	require.Len(t, *saved, 10)
	for _, o := range *saved {
		if o.ID == domain.P01 {
			assert.Equal(t, domain.Pass, o.Result)
			continue
		}
		assert.Equal(t, domain.NA, o.Result, o.ID)
		assert.Contains(t, o.Detail, "browser session ended")
	}
}

func TestSuite_Interrupted(t *testing.T) {
	s := newScripted(t)
	ctx, cancel := context.WithCancel(t.Context())
	s.results[domain.P02] = func(context.Context) (domain.ProbeOutcome, error) {
		cancel() // Ctrl-C while the second probe runs
		return domain.ProbeOutcome{}, fmt.Errorf("calling click: %w", context.Canceled)
	}
	suite, saved, summary := suiteFor(t, s)

	got, err := suite.Probe(ctx, "r1")

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []domain.CheckID{domain.P01, domain.P02}, s.order, "nothing more was started")
	require.Len(t, *saved, 1, "the probe that finished is kept; the one that was cut short is not")
	assert.Equal(t, domain.P01, (*saved)[0].ID)
	assert.Equal(t, *summary, got, "and the summary is made")
	assert.Equal(t, 1, s.closed)
}

func TestSuite_InterruptedWithAVerdictInHand(t *testing.T) {
	// The probe got as far as a verdict when Ctrl-C came, but what it waited for
	// was cut short: the verdict says nothing about the page.
	s := newScripted(t)
	ctx, cancel := context.WithCancel(t.Context())
	s.results[domain.P02] = func(context.Context) (domain.ProbeOutcome, error) {
		cancel()
		return domain.ProbeOutcome{Result: domain.Fail, Detail: "the reply never reached a final status"}, nil
	}
	suite, saved, _ := suiteFor(t, s)

	_, err := suite.Probe(ctx, "r1")

	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, *saved, 1)
	assert.Equal(t, domain.P01, (*saved)[0].ID, "only the probe that was finished before")
}

// foundEarlier is what a suite that ran to its end left with the run: an outcome
// for every probe, in id order.
func foundEarlier() []domain.ProbeOutcome {
	var out []domain.ProbeOutcome
	for _, id := range domain.Probes {
		out = append(out, domain.ProbeOutcome{ID: id, Level: id.Level(), Result: domain.Pass, Detail: "found earlier"})
	}
	return out
}

func TestSuite_AnInterruptedSuiteKeepsWhatAnEarlierOneFound(t *testing.T) {
	// A run that is resumed has its probes carried out again. A suite that is cut
	// short did not get to every probe, and those keep the outcome the run has.
	// cutShortAt makes Ctrl-C arrive while the probe at that place in the order runs.
	cutShortAt := func(t *testing.T, s *scripted, place int) context.Context {
		ctx, cancel := context.WithCancel(t.Context())
		s.results[probe.Order[place]] = func(context.Context) (domain.ProbeOutcome, error) {
			cancel()
			return domain.ProbeOutcome{}, fmt.Errorf("calling click: %w", context.Canceled)
		}
		return ctx
	}

	t.Run("cut short during the first probe: every outcome stays", func(t *testing.T) {
		s := newScripted(t)
		ctx := cutShortAt(t, s, 0)
		suite, saved, _ := suiteFor(t, s, foundEarlier()...)

		got, err := suite.Probe(ctx, "r1")

		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, foundEarlier(), *saved)
		assert.Equal(t, foundEarlier(), got.UI.Probes, "and the summary is made with them")
	})
	t.Run("cut short during the third: the two that finished replace theirs", func(t *testing.T) {
		s := newScripted(t)
		first, second := probe.Order[0], probe.Order[1]
		s.returns(first, domain.ProbeOutcome{Result: domain.Fail, Detail: "found now"}, nil)
		s.returns(second, domain.ProbeOutcome{Result: domain.Pass, Detail: "found now"}, nil)
		ctx := cutShortAt(t, s, 2)
		suite, saved, _ := suiteFor(t, s, foundEarlier()...)

		_, err := suite.Probe(ctx, "r1")

		require.ErrorIs(t, err, context.Canceled)
		want := foundEarlier()
		for i, o := range want {
			switch o.ID {
			case first:
				want[i].Result, want[i].Detail = domain.Fail, "found now"
			case second:
				want[i].Detail = "found now"
			}
		}
		assert.Equal(t, want, *saved, "one outcome for each probe, in id order")
	})
	t.Run("a suite that ran to its end replaces them all", func(t *testing.T) {
		s := newScripted(t)
		suite, saved, _ := suiteFor(t, s, foundEarlier()...)

		_, err := suite.Probe(t.Context(), "r1")

		require.NoError(t, err)
		require.Len(t, *saved, 10)
		for _, o := range *saved {
			assert.Equal(t, "fine", o.Detail, o.ID)
		}
	})
}

// hang is what a store, a reporter or a browser does when it has stopped
// answering: it waits for its context to end. If nothing ends it, an hour of the
// test's clock does, so that a missing limit fails a test with a message and does
// not stop the program of the test.
func hang(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Hour):
		return errors.New("nothing ended the wait")
	}
}

func TestSuite_TheBrowserGetsFifteenSecondsToCloseEvenAfterCtrlC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newScripted(t)
		s.closing = func(ctx context.Context) error {
			assert.NoError(t, ctx.Err(), "Ctrl-C does not end the time to close")
			return hang(ctx)
		}
		suite, _, summary := suiteFor(t, s)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		start := time.Now()

		got, err := suite.Probe(ctx, "r1")

		require.ErrorIs(t, err, context.Canceled, "the run was cut short")
		assert.Equal(t, 15*time.Second, time.Since(start))
		assert.Equal(t, *summary, got, "and a browser that would not close does not stop the summary")
	})
}

func TestSuite_KeepingWhatWasFoundHasAnEnd(t *testing.T) {
	// What the probes found is kept with a context that Ctrl-C does not end. A
	// store or a reporter that has stopped answering must not hold the program for
	// ever on that account, and running out of that time is a failure to keep, which
	// the command line must not take for the user's Ctrl-C.
	tests := []struct {
		name        string
		hangs       string
		interrupted bool
		want        string
	}{
		{"the earlier outcomes of a run cut short", "LoadProbes", true, "reading the earlier probe outcomes: took longer than 10s"},
		{"the outcomes of a run cut short", "SaveProbes", true, "keeping the probe outcomes: took longer than 10s"},
		{"the outcomes of a run that finished", "SaveProbes", false, "keeping the probe outcomes: took longer than 10s"},
		{"the summary of a run cut short", "Report", true, "making the summary again: took longer than 10s"},
		{"the summary of a run that finished", "Report", false, "making the summary again: took longer than 10s"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newScripted(t)
				store := resmocks.NewResultStore(t)
				store.On("LoadProbes", mock.Anything, "r1").Return(func(ctx context.Context, _ string) ([]domain.ProbeOutcome, error) {
					if tc.hangs == "LoadProbes" {
						return nil, hang(ctx)
					}
					return nil, nil
				}).Maybe()
				if tc.hangs != "LoadProbes" { // outcomes that could not be read are not written over
					store.On("SaveProbes", mock.Anything, "r1", mock.Anything).Return(func(ctx context.Context, _ string, _ []domain.ProbeOutcome) error {
						if tc.hangs == "SaveProbes" {
							return hang(ctx)
						}
						return nil
					})
				}
				reporter := svcmocks.NewReporter(t)
				reporter.On("Report", mock.Anything, "r1").Return(func(ctx context.Context, _ string) (domain.Summary, error) {
					return domain.Summary{}, hang(ctx)
				}).Maybe()
				suite := &probe.Suite{Probes: s.probes, Store: store, Reporter: reporter}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.interrupted {
					cancel()
				}
				start := time.Now()

				_, err := suite.Probe(ctx, "r1")

				require.Error(t, err)
				assert.EqualError(t, err, tc.want)
				assert.NotErrorIs(t, err, context.DeadlineExceeded, "so it is not taken for Ctrl-C")
				assert.NotErrorIs(t, err, context.Canceled)
				assert.Equal(t, 10*time.Second, time.Since(start), "keeping is given ten seconds, once the browser is closed")
			})
		})
	}
}

func TestSuite_KeepingGetsItsTenSecondsWhateverTheClosingTook(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newScripted(t)
		s.closing = func(context.Context) error {
			time.Sleep(14 * time.Second) // nearly all of the fifteen the browser is given
			return nil
		}
		store := resmocks.NewResultStore(t)
		store.On("SaveProbes", mock.Anything, "r1", mock.Anything).Return(func(ctx context.Context, _ string, _ []domain.ProbeOutcome) error {
			return hang(ctx)
		})
		suite := &probe.Suite{Probes: s.probes, Store: store, Reporter: svcmocks.NewReporter(t)}
		start := time.Now()

		_, err := suite.Probe(t.Context(), "r1")

		require.Error(t, err)
		assert.Equal(t, 24*time.Second, time.Since(start), "fourteen to close, then ten to keep")
	})
}

func TestSuite_TheStoreOrTheReporterFails(t *testing.T) {
	boom := errors.New("disk on fire")

	t.Run("the outcomes cannot be kept", func(t *testing.T) {
		s := newScripted(t)
		store := resmocks.NewResultStore(t)
		store.On("SaveProbes", mock.Anything, "r1", mock.Anything).Return(boom)
		suite := &probe.Suite{Probes: s.probes, Store: store, Reporter: svcmocks.NewReporter(t)}

		_, err := suite.Probe(t.Context(), "r1")

		require.ErrorIs(t, err, boom)
		assert.ErrorContains(t, err, "keeping the probe outcomes")
	})
	t.Run("the earlier outcomes of a run cut short cannot be read: they are not written over", func(t *testing.T) {
		s := newScripted(t)
		store := resmocks.NewResultStore(t) // which fails the test if it is asked to save
		store.On("LoadProbes", mock.Anything, "r1").Return(nil, boom)
		suite := &probe.Suite{Probes: s.probes, Store: store, Reporter: svcmocks.NewReporter(t)}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := suite.Probe(ctx, "r1")

		require.ErrorIs(t, err, boom)
		assert.ErrorContains(t, err, "reading the earlier probe outcomes")
		assert.NotErrorIs(t, err, context.Canceled, "so it is not taken for a run that was only cut short")
	})
	t.Run("the summary cannot be made", func(t *testing.T) {
		s := newScripted(t)
		store := resmocks.NewResultStore(t)
		store.On("SaveProbes", mock.Anything, "r1", mock.Anything).Return(nil)
		reporter := svcmocks.NewReporter(t)
		reporter.On("Report", mock.Anything, "r1").Return(domain.Summary{}, boom)
		suite := &probe.Suite{Probes: s.probes, Store: store, Reporter: reporter}

		_, err := suite.Probe(t.Context(), "r1")

		require.ErrorIs(t, err, boom)
	})
	t.Run("a browser that will not close is a warning", func(t *testing.T) {
		probes := resmocks.NewUIProbes(t)
		for _, m := range []string{"Stop", "Regenerate", "Lighthouse", "Mobile", "RenameDelete", "Theme", "ReloadMidStream", "KeyboardOnly", "LayoutShift", "AgentDown"} {
			probes.On(m, mock.Anything).Return(domain.ProbeOutcome{Result: domain.Pass}, nil)
		}
		probes.On("Close", mock.Anything).Return(boom)
		store := resmocks.NewResultStore(t)
		store.On("SaveProbes", mock.Anything, "r1", mock.Anything).Return(nil)
		reporter := svcmocks.NewReporter(t)
		reporter.On("Report", mock.Anything, "r1").Return(domain.Summary{RunID: "r1"}, nil)
		suite := &probe.Suite{Probes: probes, Store: store, Reporter: reporter}

		summary, err := suite.Probe(t.Context(), "r1")

		require.NoError(t, err)
		assert.Equal(t, "r1", summary.RunID)
	})
}
