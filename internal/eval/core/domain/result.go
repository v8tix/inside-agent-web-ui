package domain

import "time"

// Tokens counts what the agent reports it used.
type Tokens struct {
	Prompt     int `json:"prompt,omitzero"`
	Completion int `json:"completion,omitzero"`
}

// Result is one attempt: one row of results.jsonl. Question, truth and answer
// text are left out unless the run asked to include text, because GAIA's terms
// forbid sharing them and results are easy to share by accident.
type Result struct {
	RunID   string        `json:"run_id"`
	TaskID  string        `json:"task_id"`
	Level   int           `json:"level"`
	Attempt int           `json:"attempt"`
	Status  AttemptStatus `json:"status"`
	Score   int           `json:"score"` // 1 when correct

	LatencyMs    *int64 `json:"latency_ms,omitzero"`
	TTFTUIMs     *int64 `json:"ttft_ui_ms,omitzero"`
	TTFTAgentMs  *int64 `json:"ttft_agent_ms,omitzero"`
	UIOverheadMs *int64 `json:"ui_overhead_ms,omitzero"`
	Updates      int    `json:"updates,omitzero"`
	MaxGapMs     *int64 `json:"max_gap_ms,omitzero"`

	Tokens    Tokens   `json:"tokens,omitzero"`
	ToolCalls []string `json:"tool_calls,omitempty"`

	UIChecks       map[CheckID]CheckResult `json:"ui_checks"`
	ConsoleErrors  int                     `json:"console_errors"`
	FailedRequests int                     `json:"failed_requests"`

	ConversationID string   `json:"conversation_id,omitzero"`
	ErrorClass     string   `json:"error_class,omitzero"`
	Evidence       []string `json:"evidence,omitempty"`
	// Notes say why checks failed, in words the evaluator wrote itself. They
	// never hold question, truth or answer text.
	Notes []string `json:"notes,omitempty"`

	// Present only when the run was started with --include-text.
	Question string `json:"question,omitzero"`
	Truth    string `json:"truth,omitzero"`
	Answer   string `json:"answer,omitzero"`
}

// Key identifies the attempt the row records.
func (r Result) Key() AttemptKey { return AttemptKey{TaskID: r.TaskID, Attempt: r.Attempt} }

// WithoutText returns the row with the text fields cleared.
func (r Result) WithoutText() Result {
	r.Question, r.Truth, r.Answer = "", "", ""
	return r
}

// RunMeta describes a run, for run.json. It holds no secrets.
type RunMeta struct {
	RunID      string            `json:"run_id"`
	Command    string            `json:"command"` // run, selftest, probe
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at,omitzero"`
	GitSHA     string            `json:"git_sha,omitzero"`
	UIContract string            `json:"ui_contract,omitzero"`
	Versions   map[string]string `json:"versions,omitempty"`
	// Settings are the non-secret settings the run used, as strings.
	Settings map[string]string `json:"settings,omitempty"`
	// Thresholds are the limits the run was judged against, so a report built
	// later judges the same way.
	Thresholds Thresholds `json:"thresholds,omitzero"`
	// IncludeText says the results keep question, truth and answer text.
	IncludeText bool `json:"include_text,omitzero"`
}

// ProbeOutcome is what one probe found.
type ProbeOutcome struct {
	ID       CheckID            `json:"id"`
	Level    Level              `json:"level"`
	Result   CheckResult        `json:"result"`
	Detail   string             `json:"detail,omitzero"`
	Measures map[string]float64 `json:"measures,omitempty"`
	Evidence []string           `json:"evidence,omitempty"`
}
