package score

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtract(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   string
		wantOK bool
	}{
		{"the usual", "I looked it up.\n\nFINAL ANSWER: 42", "42", true},
		{"lower case", "final answer: Paris", "Paris", true},
		{"mixed case", "Final Answer: Paris", "Paris", true},
		{"no marker", "The answer is 42.", "", false},
		{"empty reply", "", "", false},
		{"only white space", " \n\t ", "", false},
		{"the marker and nothing after it", "FINAL ANSWER:", "", false},
		{"the marker and only blanks after it", "FINAL ANSWER:  \n \n", "", false},
		{"several markers: the last one counts", "FINAL ANSWER: [YOUR FINAL ANSWER]\nthinking again...\nFINAL ANSWER: 7", "7", true},
		{"the template echoed as the answer", "FINAL ANSWER: [YOUR FINAL ANSWER]", "[YOUR FINAL ANSWER]", true},
		{"the answer on the next line", "FINAL ANSWER:\n42\nthanks", "42", true},
		{"only the first line", "FINAL ANSWER: red\nbecause it is the brightest", "red", true},
		{"blank lines before the answer", "FINAL ANSWER:\n\n\n  blue  \n", "blue", true},
		{"bold answer", "FINAL ANSWER: **42**", "42", true},
		{"bold marker", "**FINAL ANSWER:** 42", "42", true},
		{"bold marker and answer", "**FINAL ANSWER: 42**", "42", true},
		{"a bold label with space", "**FINAL ANSWER:**\n**Paris**", "Paris", true},
		{"asterisks in the middle are kept", "FINAL ANSWER: 2**10", "2**10", true},
		{"a closing pair of asterisks alone is removed", "FINAL ANSWER: 2**", "2", true},
		{"an opening pair of asterisks alone is removed", "FINAL ANSWER: **2", "2", true},
		{"windows line endings", "reasoning\r\nFINAL ANSWER: 42\r\nmore", "42", true},
		{"a list is one line", "FINAL ANSWER: 3, red, Paris", "3, red, Paris", true},
		{"a trailing explanation stays", "FINAL ANSWER: 42 (as asked)", "42 (as asked)", true},
		{"backticks stay: no cleverness", "FINAL ANSWER: `42`", "`42`", true},
		{"unicode around it", "Résumé\n\nFINAL ANSWER: Zürich", "Zürich", true},
		{"a marker inside a word is still a marker", "NOTFINAL ANSWER: 5", "5", true},
		{"the words without the colon", "FINAL ANSWER 42", "", false},
		{"a space before the colon", "FINAL ANSWER : 42", "", false},
		{"both bold markers only", "FINAL ANSWER: ****", "", false},
		{"an İ before the marker does not shift the offsets", "İİİ FINAL ANSWER: ok", "ok", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Extract(tc.raw)

			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestAnswer(t *testing.T) {
	assert.Equal(t, "42", Answer("FINAL ANSWER: 42"))
	assert.Equal(t, NoAnswer, Answer("no idea"), "the official scorer reads a missing answer as None")
	assert.Equal(t, NoAnswer, Answer(""))
}

func TestCompose(t *testing.T) {
	prompt := DefaultPrompt().Compose("What is six times seven?")

	t.Run("it has the instructions and the question", func(t *testing.T) {
		assert.Contains(t, prompt, "You are a general AI assistant")
		assert.Contains(t, prompt, "FINAL ANSWER: [YOUR FINAL ANSWER]")
		assert.Contains(t, prompt, "comma separated list")
		assert.Contains(t, prompt, "Question: What is six times seven?")
	})
	t.Run("the question comes last", func(t *testing.T) {
		assert.Regexp(t, `Question: What is six times seven\?$`, prompt)
	})
	t.Run("an answer that follows the instructions can be extracted", func(t *testing.T) {
		got, ok := Extract("It is 6 * 7.\nFINAL ANSWER: 42")
		require.True(t, ok)
		assert.True(t, Question(got, "42"))
	})
	t.Run("the question is inserted as it is: braces, quotes, line breaks", func(t *testing.T) {
		q := "What does {{.Question}} print in \"Go\" & <b>HTML</b>?\nSecond line."

		assert.Contains(t, DefaultPrompt().Compose(q), "Question: "+q)
	})
	t.Run("it does not end with a line break, which would submit nothing extra", func(t *testing.T) {
		assert.NotRegexp(t, `\n$`, prompt)
	})
}

func TestParsePrompt(t *testing.T) {
	t.Run("a custom template", func(t *testing.T) {
		p, err := ParsePrompt("Answer briefly.\n{{.Question}}\nEnd with FINAL ANSWER: <answer>\n")

		require.NoError(t, err)
		assert.Equal(t, "Answer briefly.\nWhy?\nEnd with FINAL ANSWER: <answer>", p.Compose("Why?"))
	})
	t.Run("a template of one line", func(t *testing.T) {
		p, err := ParsePrompt("Be brief. {{.Question}}")

		require.NoError(t, err)
		assert.Equal(t, "Be brief. Why?", p.Compose("Why?"))
	})
	t.Run("the spaced form of the field", func(t *testing.T) {
		_, err := ParsePrompt("Q: {{ .Question }}")
		require.NoError(t, err)
	})
	t.Run("a template without the question is refused", func(t *testing.T) {
		_, err := ParsePrompt("Just answer.")
		require.ErrorContains(t, err, "never asks the question")
	})
	t.Run("a broken template is refused", func(t *testing.T) {
		_, err := ParsePrompt("{{.Question")
		require.ErrorContains(t, err, "parsing the prompt template")
	})
	// A template that fails halfway would type what it had written so far, as the
	// prompt of every attempt, with no error to say so. It is tried when loaded.
	t.Run("an unknown field is refused when the template is loaded", func(t *testing.T) {
		_, err := ParsePrompt("{{.Question}} {{.Hint}}")

		require.Error(t, err)
		assert.ErrorContains(t, err, "Hint")
		assert.ErrorContains(t, err, "cannot be filled in")
	})
	t.Run("a call that fails is refused when the template is loaded", func(t *testing.T) {
		_, err := ParsePrompt("{{.Question}} {{index .Question 999}}")

		require.ErrorContains(t, err, "cannot be filled in")
	})
	t.Run("the question has to come out", func(t *testing.T) {
		_, err := ParsePrompt("Answer. {{if false}}{{.Question}}{{end}}")

		require.ErrorContains(t, err, "never asks the question")
	})
	t.Run("the field with trimmed space is the question too", func(t *testing.T) {
		p, err := ParsePrompt("Answer briefly:\n{{- .Question -}}\n")

		require.NoError(t, err)
		assert.Equal(t, "Answer briefly:Why?", p.Compose("Why?"))
	})
	t.Run("the question comes through as it is", func(t *testing.T) {
		p, err := ParsePrompt("{{.Question}}")

		require.NoError(t, err)
		assert.Equal(t, "a < b && \"c\" {{.Other}}", p.Compose("a < b && \"c\" {{.Other}}"), "text/template does not escape")
	})
}

func TestTheBuiltInPromptIsValid(t *testing.T) {
	assert.NotPanics(t, func() { DefaultPrompt() })
}
