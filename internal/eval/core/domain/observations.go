package domain

// UIObservations is what the browser showed during one attempt. All times are
// milliseconds on the page's clock (performance.timeOrigin + performance.now()),
// which stays comparable across documents and is not affected by how long calls
// to the browser tool take.
//
// Pattern: In-Page Clock
type UIObservations struct {
	// KeydownAt is when Enter was pressed in the composer, and StopAt when the
	// Stop button was first pressed (zero when it was not).
	KeydownAt float64          `json:"keydown_at,omitzero"`
	StopAt    float64          `json:"stop_at,omitzero"`
	Updates   []Update         `json:"updates,omitempty"`
	Statuses  []StatusChange   `json:"statuses,omitempty"`
	Scrolls   []ScrollSample   `json:"scrolls,omitempty"`
	Console   []ConsoleMessage `json:"console,omitempty"`
	Requests  []NetworkRequest `json:"requests,omitempty"`
	Message   MessageInfo      `json:"message,omitzero"`
	// VisibleText is the answer body's visible text at the end. It is answer
	// text, so it is never written to results.
	VisibleText string `json:"-"`
	// Unsafe is what the answer body holds that it should not.
	Unsafe BodyFindings `json:"unsafe,omitzero"`
	// ObserverLate is true when the observer was installed after the reply had
	// begun, so the start of the timeline is missing.
	ObserverLate bool `json:"observer_late,omitzero"`
}

// Update is a change of the answer body.
type Update struct {
	At    float64 `json:"at"`
	Chars int     `json:"chars"` // length of the visible text after the change
}

// StatusChange is a change of the message's data-status.
type StatusChange struct {
	At     float64     `json:"at"`
	Status ReplyStatus `json:"status"`
}

// ScrollSample is how far the message list was from its bottom just after an
// update, and whether it could scroll at all: a list that fits in the window is
// always at its bottom, which says nothing about whether it would follow a reply.
type ScrollSample struct {
	At           float64 `json:"at"`
	FromBottomPx float64 `json:"from_bottom_px"`
	Scrollable   bool    `json:"scrollable,omitzero"`
}

// ConsoleMessage is one browser console entry.
type ConsoleMessage struct {
	Type string `json:"type"` // log, info, warn, error, issue, exception
	Text string `json:"text,omitzero"`
}

// NetworkRequest is one request the page made.
type NetworkRequest struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Status int    `json:"status,omitzero"`
	Failed bool   `json:"failed,omitzero"`
	Type   string `json:"type,omitzero"` // document, fetch, eventsource, ...
}

// MessageInfo is the assistant message's own account of itself: its status and
// the data-* attributes the Web UI sets once it is finished. A nil value means
// the attribute was absent, which means unknown.
type MessageInfo struct {
	ID               string      `json:"id,omitzero"`
	Status           ReplyStatus `json:"status,omitzero"`
	TTFTMs           *int64      `json:"ttft_ms,omitzero"`
	DurationMs       *int64      `json:"duration_ms,omitzero"`
	PromptTokens     *int        `json:"prompt_tokens,omitzero"`
	CompletionTokens *int        `json:"completion_tokens,omitzero"`
	ToolCalls        []string    `json:"tool_calls,omitempty"`
}

// BodyFindings counts things that must not be in a rendered answer.
type BodyFindings struct {
	Scripts           int `json:"scripts,omitzero"`
	EventHandlerAttrs int `json:"event_handler_attrs,omitzero"`
	ExternalImages    int `json:"external_images,omitzero"`
}

// Any reports whether anything unsafe was found.
func (f BodyFindings) Any() bool { return f.Scripts+f.EventHandlerAttrs+f.ExternalImages > 0 }

// TTFTUI is the time from the Enter key to the first update that has text.
func (o UIObservations) TTFTUI() (ms float64, ok bool) {
	if o.KeydownAt == 0 {
		return 0, false
	}
	for _, u := range o.Updates {
		if u.Chars > 0 {
			return u.At - o.KeydownAt, true
		}
	}
	return 0, false
}

// Latency is the time from the Enter key until the reply reached a final status.
func (o UIObservations) Latency() (ms float64, ok bool) {
	if o.KeydownAt == 0 {
		return 0, false
	}
	for _, s := range o.Statuses {
		if s.Status.Terminal() {
			return s.At - o.KeydownAt, true
		}
	}
	return 0, false
}

// UIOverhead is what the UI adds to the agent's own time to first token: the
// UI's TTFT minus the one the message reports.
func (o UIObservations) UIOverhead() (ms float64, ok bool) {
	ui, ok := o.TTFTUI()
	if !ok || o.Message.TTFTMs == nil {
		return 0, false
	}
	return ui - float64(*o.Message.TTFTMs), true
}

// DistinctUpdates counts the updates that changed the length of the text.
func (o UIObservations) DistinctUpdates() int {
	n, last := 0, 0
	for _, u := range o.Updates {
		if u.Chars > 0 && u.Chars != last {
			n++
		}
		last = u.Chars
	}
	return n
}

// MaxGap is the longest pause between two consecutive updates.
func (o UIObservations) MaxGap() (ms float64, ok bool) {
	if len(o.Updates) < 2 {
		return 0, false
	}
	for i := 1; i < len(o.Updates); i++ {
		ms = max(ms, o.Updates[i].At-o.Updates[i-1].At)
	}
	return ms, true
}

// FinalChars is the length of the answer text at the last update.
func (o UIObservations) FinalChars() int {
	if len(o.Updates) == 0 {
		return 0
	}
	return o.Updates[len(o.Updates)-1].Chars
}

// ConsoleErrors counts error messages and uncaught exceptions.
func (o UIObservations) ConsoleErrors() int {
	n := 0
	for _, m := range o.Console {
		if m.Type == "error" || m.Type == "exception" {
			n++
		}
	}
	return n
}

// FailedRequests counts requests that failed or got a status of 400 or more.
func (o UIObservations) FailedRequests() int {
	n := 0
	for _, r := range o.Requests {
		if r.Failed || r.Status >= 400 {
			n++
		}
	}
	return n
}
