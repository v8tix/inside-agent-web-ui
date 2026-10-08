// Package domain holds the evaluator's vocabulary: problems, attempts, what
// the browser observed, and the scorecards built from them. It has no
// dependencies outside the standard library.
package domain

import "fmt"

// Problem is one question to put to the agent.
type Problem struct {
	TaskID   string `json:"task_id"`
	Level    int    `json:"level"`
	Question string `json:"question"`
	// Truth is the expected answer. It is empty when the answer is withheld.
	Truth string `json:"truth,omitzero"`
	// FileName names the attachment the problem comes with, if any.
	FileName string `json:"file_name,omitzero"`
	// Unscored marks a problem with no known answer (the GAIA test split): it
	// can be asked but not graded.
	Unscored bool `json:"unscored,omitzero"`
}

// AttachmentsMode says what to do with problems that come with a file. The UI
// has no way to attach one, so the agent cannot see it.
type AttachmentsMode string

// Attachment modes.
const (
	// AttachmentsSkip leaves those problems out. Coverage reports how many.
	AttachmentsSkip AttachmentsMode = "skip"
	// AttachmentsInclude asks them anyway, without the file.
	AttachmentsInclude AttachmentsMode = "include"
)

// ParseAttachmentsMode reads a configuration value.
func ParseAttachmentsMode(s string) (AttachmentsMode, error) {
	switch m := AttachmentsMode(s); m {
	case AttachmentsSkip, AttachmentsInclude:
		return m, nil
	}
	return "", fmt.Errorf("attachments mode %q is not one of skip, include", s)
}

// Eligible reports whether the problem can be asked and graded under mode.
func (p Problem) Eligible(mode AttachmentsMode) bool {
	if p.Unscored {
		return false
	}
	return p.FileName == "" || mode == AttachmentsInclude
}

// Valid reports whether the problem is complete enough to ask.
func (p Problem) Valid() error {
	switch {
	case p.TaskID == "":
		return fmt.Errorf("a problem has no task id")
	case p.Question == "":
		return fmt.Errorf("problem %s has no question", p.TaskID)
	case p.Level < 1 || p.Level > 3:
		return fmt.Errorf("problem %s has level %d, expected 1 to 3", p.TaskID, p.Level)
	case !p.Unscored && p.Truth == "":
		return fmt.Errorf("problem %s has no answer and is not marked unscored", p.TaskID)
	}
	return nil
}
