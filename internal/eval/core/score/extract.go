package score

import "strings"

// marker is the convention the prompt asks the agent to finish with.
const marker = "final answer:"

// Extract returns the final answer in a reply, by a rule that is frozen so that
// results stay comparable: the text after the last "FINAL ANSWER:" (any case),
// its first non-empty line, with surrounding ** removed. A line that is empty
// once the ** and the spaces are gone (the closing ** of a bold label, say)
// does not count as a line. ok is false when there is no marker, or no answer
// after it.
//
// It does not try to be clever. An answer in backticks or with a trailing
// explanation is returned as written, and scored as written.
//
// Pattern: Frozen Extraction Rule
func Extract(raw string) (answer string, ok bool) {
	at := strings.LastIndex(asciiLower(raw), marker)
	if at < 0 {
		return "", false
	}
	for line := range strings.SplitSeq(raw[at+len(marker):], "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "**"), "**"))
		if line != "" {
			return line, true
		}
	}
	return "", false
}

// Answer is what to score: the extracted answer, or NoAnswer when there is none.
func Answer(raw string) string {
	if answer, ok := Extract(raw); ok {
		return answer
	}
	return NoAnswer
}
