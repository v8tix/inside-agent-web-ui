package score

import "strings"

// NoAnswer is what the official scorer reads when there is no answer at all.
const NoAnswer = "None"

// Question reports whether answer matches truth under the official GAIA rules
// (question_scorer in the leaderboard's scorer.py):
//
//   - A truth that Python can read as a number: the answer, minus $, % and
//     commas, must read as the same number.
//   - A truth with a comma or a semicolon is a list. Answer and truth are split
//     on both, must have the same length, and match element by element: numbers
//     as numbers, anything else with white space removed and the case folded,
//     but punctuation kept. So a truth of "1,000" is the list 1 and 000.
//   - Anything else: white space and ASCII punctuation removed, case folded.
//
// Pass NoAnswer when there is no answer.
func Question(answer, truth string) bool {
	if truthNumber, ok := pyFloat(truth); ok {
		return normalizeNumber(answer) == truthNumber
	}
	if strings.ContainsAny(truth, ",;") {
		truthParts, answerParts := splitList(truth), splitList(answer)
		if len(truthParts) != len(answerParts) {
			return false
		}
		for i, truthPart := range truthParts {
			if number, ok := pyFloat(truthPart); ok {
				if normalizeNumber(answerParts[i]) != number {
					return false
				}
				continue
			}
			if normalizeString(answerParts[i], false) != normalizeString(truthPart, false) {
				return false
			}
		}
		return true
	}
	return normalizeString(answer, true) == normalizeString(truth, true)
}
