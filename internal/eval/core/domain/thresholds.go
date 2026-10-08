package domain

// Thresholds are the limits the UI checks and probes judge against. They are
// settings with these defaults, never tuned to make a result pass.
type Thresholds struct {
	// OverheadP95Ms is the most the UI may add to the agent's time to first
	// token, at the 95th percentile (U-05).
	OverheadP95Ms float64 `json:"overhead_p95_ms"`
	// StreamMinChars is how long an answer must be before it has to show
	// incremental updates, and StreamMinUpdates how many (U-04).
	StreamMinChars   int `json:"stream_min_chars"`
	StreamMinUpdates int `json:"stream_min_updates"`
	// ScrollFollowPx is how far from the bottom the view may be while it is
	// supposed to follow the reply (U-07, SC-12).
	ScrollFollowPx float64 `json:"scroll_follow_px"`
	// StopWithinMs is how fast Stop must take effect (P-01).
	StopWithinMs float64 `json:"stop_within_ms"`
	// LighthouseMin is the lowest accessibility and best-practices score (P-03).
	LighthouseMin int `json:"lighthouse_min"`
	// CLSMax is the largest cumulative layout shift (P-09).
	CLSMax float64 `json:"cls_max"`
}

// DefaultThresholds returns the values the spec states.
func DefaultThresholds() Thresholds {
	return Thresholds{
		OverheadP95Ms:    100,
		StreamMinChars:   200,
		StreamMinUpdates: 3,
		ScrollFollowPx:   80,
		StopWithinMs:     1000,
		LighthouseMin:    90,
		CLSMax:           0.1,
	}
}
