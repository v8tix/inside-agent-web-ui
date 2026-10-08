package devtools_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
)

func TestParseLighthouse(t *testing.T) {
	t.Run("the recorded audits", func(t *testing.T) {
		desktop, desktopErr := fixture(t, "lighthouse_desktop")
		mobile, mobileErr := fixture(t, "lighthouse_mobile_dir")
		require.False(t, desktopErr || mobileErr)
		for device, text := range map[string]string{"desktop": desktop, "mobile": mobile} {
			got, err := devtools.ParseLighthouse(text)

			require.NoError(t, err, device)
			assert.Equal(t, map[string]float64{"accessibility": 95, "best-practices": 100, "seo": 90, "agentic-browsing": 100}, got, device)
		}
	})
	t.Run("scores are read by category, whatever the order", func(t *testing.T) {
		got, err := devtools.ParseLighthouse("### Category Scores\n- Best Practices: 87 (best-practices)\n- Accessibility: 100 (accessibility)\n### Audit Summary\nPassed: 3\n")

		require.NoError(t, err)
		assert.Equal(t, map[string]float64{"best-practices": 87, "accessibility": 100}, got)
	})
	t.Run("the numbers of the summary are not scores", func(t *testing.T) {
		got, err := devtools.ParseLighthouse("### Category Scores\n- Accessibility: 95 (accessibility)\n### Audit Summary\nPassed: 42\nFailed: 2\n")

		require.NoError(t, err)
		assert.Len(t, got, 1)
	})
	for name, text := range map[string]string{
		"an error":      "Error: Lighthouse failed",
		"empty":         "",
		"no categories": "## Lighthouse Audit Results\nMode: navigation\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := devtools.ParseLighthouse(text)

			require.ErrorIs(t, err, devtools.ErrNoLighthouseScores)
		})
	}
}

func TestParseCLS(t *testing.T) {
	t.Run("the recorded trace", func(t *testing.T) {
		text, _ := fixture(t, "performance_stop_trace")

		got, err := devtools.ParseCLS(text)

		require.NoError(t, err)
		assert.Zero(t, got)
	})
	t.Run("a shift", func(t *testing.T) {
		got, err := devtools.ParseCLS("Metrics (lab / observed):\n  - LCP: 120 ms\n  - CLS: 0.17\nMetrics (field / real users): n/a\n")

		require.NoError(t, err)
		assert.InDelta(t, 0.17, got, 1e-9)
	})
	t.Run("the first insight set", func(t *testing.T) {
		got, err := devtools.ParseCLS("## insight set id: NAVIGATION_0\nMetrics (lab / observed):\n  - CLS: 0.05\n## insight set id: NAVIGATION_1\nMetrics (lab / observed):\n  - CLS: 0.40\n")

		require.NoError(t, err)
		assert.InDelta(t, 0.05, got, 1e-9, "the first part of the trace is the page the reader was on")
	})
	for name, text := range map[string]string{
		"no metrics":     "The performance trace has been stopped.\n",
		"a field figure": "Metrics (field / real users): n/a – no data\n",
		"empty":          "",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := devtools.ParseCLS(text)

			require.ErrorIs(t, err, devtools.ErrNoCLS)
		})
	}
}
