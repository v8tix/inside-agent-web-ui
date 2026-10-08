package evalselftest_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/synthetic"
	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
	"github.com/v8tix/inside-agent-web-ui/it/internal/evalenv"
)

// scriptedPrompts are what the probes say to the scripted agent: ids that its
// fixture knows, for a long reply and for one that fails once.
var scriptedPrompts = devtools.ProbePrompts{
	Long:  "Write a long essay about the printing press. [probe-long]",
	Short: "Reply with the single word OK.",
	Flaky: "Are you back? [probe-flaky]",
}

func newProbes(t *testing.T, env *evalenv.Env, th domain.Thresholds) *devtools.Probes {
	t.Helper()
	p := &devtools.Probes{
		Factory: &devtools.Factory{
			Session: evalenv.BrowserOptions(t),
			Driver:  devtools.DriverConfig{BaseURL: env.URL},
		},
		Email: env.Email, Password: env.Password, Prompts: scriptedPrompts, Thresholds: th,
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = p.Close(ctx)
	})
	return p
}

// Every probe, against the real Web UI. The scripted agent gives replies that
// take time to write (probe-long) and one that fails once (probe-flaky).
func TestProbesSuite(t *testing.T) {
	evalenv.RequireBrowser(t)
	fixture, err := testagent.ParseFixture(synthetic.Replies())
	require.NoError(t, err)
	env := evalenv.Start(t, testagent.WithFixture(fixture), testagent.WithWordDelay(5*time.Millisecond))
	th := domain.DefaultThresholds()
	probes := newProbes(t, env, th)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()

	steps := []struct {
		id  domain.CheckID
		run func(context.Context) (domain.ProbeOutcome, error)
	}{
		{domain.P01, probes.Stop},
		{domain.P02, probes.Regenerate},
		{domain.P07, probes.ReloadMidStream},
		{domain.P05, probes.RenameDelete},
		{domain.P10, probes.AgentDown},
		{domain.P06, probes.Theme},
		{domain.P08, probes.KeyboardOnly},
		{domain.P09, probes.LayoutShift},
		{domain.P04, probes.Mobile},
		{domain.P03, probes.Lighthouse},
	}
	for _, step := range steps {
		t.Run(string(step.id)+" "+step.id.Name(), func(t *testing.T) {
			started := time.Now()

			got, err := step.run(ctx)

			require.NoError(t, err, "the probe could be carried out")
			t.Logf("%s %s in %s: %s %v", got.ID, got.Result, time.Since(started).Round(time.Millisecond), got.Detail, got.Measures)
			assert.Equal(t, domain.Pass, got.Result, got.Detail)
		})
	}
}

// The measurement of Stop is tied to the limit: a limit no Stop could meet
// fails it, with the time it took.
func TestProbes_StopFailsAgainstAnImpossibleLimit(t *testing.T) {
	evalenv.RequireBrowser(t)
	fixture, err := testagent.ParseFixture(synthetic.Replies())
	require.NoError(t, err)
	env := evalenv.Start(t, testagent.WithFixture(fixture), testagent.WithWordDelay(5*time.Millisecond))
	th := domain.DefaultThresholds()
	th.StopWithinMs = 0.001
	probes := newProbes(t, env, th)

	got, err := probes.Stop(t.Context())

	require.NoError(t, err)
	assert.Equal(t, domain.Fail, got.Result)
	assert.Contains(t, got.Detail, "over the 0 ms limit")
	assert.Positive(t, got.Measures["stop_ms"])
	assert.Less(t, got.Measures["stop_ms"], 1000.0, "and the real Stop is well within the real limit")
}

// Against an agent that cannot be told to fail, the probe that needs one is not run.
func TestProbes_AgentDownNeedsTheScriptedAgent(t *testing.T) {
	evalenv.RequireBrowser(t)
	probes := &devtools.Probes{Prompts: devtools.ProbePrompts{Long: "x", Short: "y"}}

	got, err := probes.AgentDown(t.Context())

	require.NoError(t, err)
	assert.Equal(t, domain.NA, got.Result)
	assert.Equal(t, domain.P10, got.ID)
	assert.Contains(t, got.Detail, "needs the scripted agent")
}
