package score

import (
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"text/template"
)

// defaultTemplate holds the answer-format instructions from the GAIA paper (the
// ones the leaderboard's own examples use), followed by the question. Reports
// are comparable only when every run asks the same way, so it changes rarely
// and on purpose.
//
//go:embed prompt.gotmpl
var defaultTemplate string

// Prompt turns a question into the text typed into the composer: the answer
// format instructions, then the question, with the FINAL ANSWER convention the
// extraction below relies on.
type Prompt struct{ tmpl *template.Template }

// DefaultPrompt is the built-in prompt.
func DefaultPrompt() Prompt {
	p, err := ParsePrompt(defaultTemplate)
	if err != nil {
		panic(fmt.Sprintf("the built-in prompt template is broken: %v", err)) // a test keeps this from happening
	}
	return p
}

// sampleQuestion is what a template is tried with when it is loaded.
const sampleQuestion = "SAMPLE-QUESTION-0f3a"

// ParsePrompt reads a template. It is tried with a sample question, which has to
// come out: a prompt that does not contain the question asks nothing, and one
// that fails halfway would be typed truncated, for every attempt, with no error.
func ParsePrompt(text string) (Prompt, error) {
	tmpl, err := template.New("prompt").Option("missingkey=error").Parse(text)
	if err != nil {
		return Prompt{}, fmt.Errorf("parsing the prompt template: %w", err)
	}
	var tried strings.Builder
	if err := tmpl.Execute(&tried, struct{ Question string }{sampleQuestion}); err != nil {
		return Prompt{}, fmt.Errorf("the prompt template cannot be filled in: %w", err)
	}
	if !strings.Contains(tried.String(), sampleQuestion) {
		return Prompt{}, errors.New("the prompt template never asks the question: {{.Question}} does not come out of it")
	}
	return Prompt{tmpl: tmpl}, nil
}

// Compose returns the text to type for question.
func (p Prompt) Compose(question string) string {
	var b strings.Builder
	// Execution cannot fail: ParsePrompt tried the template with the same kind of
	// data, and the only value that changes is the question.
	_ = p.tmpl.Execute(&b, struct{ Question string }{question})
	return strings.TrimRight(b.String(), "\n")
}
