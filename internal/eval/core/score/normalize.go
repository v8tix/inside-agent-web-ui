// Package score grades answers and builds the prompt. The scorer is a port of
// the official GAIA question_scorer, quirks included: the point is to give the
// score the leaderboard would give, not the score that seems right.
package score

import (
	"math"
	"strconv"
	"strings"
	"unicode"
)

// isSpace is Python's str.isspace for one character: Unicode white space plus
// the four ASCII information separators (U+001C to U+001F). It is what the
// string rule removes.
func isSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// floatSpace reports whether Python's float() skips r at either end of what it
// reads: the six ASCII white-space characters (tab, line feed, vertical tab,
// form feed, carriage return, space) and the Unicode white space above ASCII.
// The information separators are not among them, so a number with one beside
// it is not a number.
func floatSpace(r rune) bool { return unicode.IsSpace(r) }

// asciiPunctuation is Python's string.punctuation.
const asciiPunctuation = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"

// decimalDigit returns the value of a Unicode decimal digit (category Nd), as
// Python's float() reads them: Arabic-Indic and full-width digits count.
func decimalDigit(r rune) (int, bool) {
	if r >= '0' && r <= '9' {
		return int(r - '0'), true
	}
	if r < 0x80 || !unicode.Is(unicode.Nd, r) {
		return 0, false
	}
	// Digits come in runs of ten starting at zero; a table entry can hold
	// several runs back to back.
	for _, rg := range unicode.Nd.R16 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10, true
		}
	}
	for _, rg := range unicode.Nd.R32 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10, true
		}
	}
	return 0, false
}

// asciiLower lowers A-Z only. The words inf, infinity and nan are matched with
// it, because a wider lowering would turn "İnf" into "inf".
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// pyFloat parses s the way Python's float() does:
//
//	[sign] (digits ["." [digits]] | "." digits) [("e"|"E") [sign] digits]
//
// or inf, infinity or nan in any case. The white space that float() skips is
// allowed around it (floatSpace); digits may be any Unicode decimal digit; a
// single underscore may sit between two digits. Anything else (hex, a second
// sign, "1,5") is not a float.
//
// Pattern: Bug-for-Bug Port
func pyFloat(s string) (float64, bool) {
	s = strings.TrimFunc(s, floatSpace)
	if s == "" {
		return 0, false
	}
	runes := []rune(s)
	negative := false
	if runes[0] == '+' || runes[0] == '-' {
		negative = runes[0] == '-'
		runes = runes[1:]
	}
	if len(runes) == 0 {
		return 0, false
	}
	switch asciiLower(string(runes)) {
	case "inf", "infinity":
		if negative {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	case "nan":
		return math.NaN(), true
	}

	var ascii strings.Builder
	if negative {
		ascii.WriteByte('-')
	}
	pos := 0
	// digits reads digit (["_"] digit)* and reports whether it found any.
	digits := func() bool {
		start := pos
		for pos < len(runes) {
			if d, ok := decimalDigit(runes[pos]); ok {
				ascii.WriteByte(byte('0' + d))
				pos++
				continue
			}
			if runes[pos] == '_' && pos > start && pos+1 < len(runes) {
				if _, next := decimalDigit(runes[pos+1]); next {
					pos++
					continue
				}
			}
			break
		}
		return pos > start
	}

	whole := digits()
	fraction := false
	if pos < len(runes) && runes[pos] == '.' {
		ascii.WriteByte('.')
		pos++
		fraction = digits()
	}
	if !whole && !fraction {
		return 0, false
	}
	if pos < len(runes) && (runes[pos] == 'e' || runes[pos] == 'E') {
		ascii.WriteByte('e')
		pos++
		if pos < len(runes) && (runes[pos] == '+' || runes[pos] == '-') {
			ascii.WriteRune(runes[pos])
			pos++
		}
		if !digits() {
			return 0, false
		}
	}
	if pos != len(runes) {
		return 0, false
	}
	f, err := strconv.ParseFloat(ascii.String(), 64)
	if err != nil && !isRangeError(err) {
		return 0, false
	}
	return f, true // an out-of-range number is infinity, as in Python
}

func isRangeError(err error) bool {
	numErr, ok := err.(*strconv.NumError)
	return ok && numErr.Err == strconv.ErrRange
}

// normalizeNumber is the official normalize_number_str: drop $, % and commas,
// read the rest as a float, and when that fails answer infinity (which equals a
// truth of "inf", a quirk kept on purpose).
func normalizeNumber(s string) float64 {
	s = strings.NewReplacer("$", "", "%", "", ",", "").Replace(s)
	if f, ok := pyFloat(s); ok {
		return f
	}
	return math.Inf(1)
}

// splitList splits on every comma or semicolon and keeps empty pieces, like
// Python's re.split("[,;]", s).
func splitList(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' || s[i] == ';' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// normalizeString is the official normalize_str: remove all white space, lower
// the case, and optionally remove ASCII punctuation.
func normalizeString(s string, removePunctuation bool) string {
	noSpaces := strings.Map(func(r rune) rune {
		if isSpace(r) {
			return -1
		}
		return r
	}, s)
	lowered := pyLower(noSpaces)
	if !removePunctuation {
		return lowered
	}
	return strings.Map(func(r rune) rune {
		if r < 0x80 && strings.ContainsRune(asciiPunctuation, r) {
			return -1
		}
		return r
	}, lowered)
}

// pyLower lowers the case the way Python's str.lower() does. It differs from a
// plain per-character mapping in two places: a capital İ becomes "i" plus a
// combining dot, and a capital Σ becomes ς at the end of a word and σ elsewhere.
func pyLower(s string) string {
	runes := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range runes {
		switch r {
		case 0x130:
			b.WriteString("i̇")
		case 0x3a3:
			if finalSigma(runes, i) {
				b.WriteRune('ς')
			} else {
				b.WriteRune('σ')
			}
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// finalSigma implements Unicode's Final_Sigma rule: the sigma follows a cased
// letter and no cased letter follows it, ignoring case-ignorable characters.
func finalSigma(runes []rune, i int) bool {
	j := i - 1
	for j >= 0 && caseIgnorable(runes[j]) {
		j--
	}
	if j < 0 || !cased(runes[j]) {
		return false
	}
	j = i + 1
	for j < len(runes) && caseIgnorable(runes[j]) {
		j++
	}
	return j >= len(runes) || !cased(runes[j])
}

// cased is the Unicode Cased property: upper, lower and title case letters.
func cased(r rune) bool {
	return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) ||
		unicode.Is(unicode.Other_Lowercase, r) || unicode.Is(unicode.Other_Uppercase, r)
}

// caseIgnorable is the Unicode Case_Ignorable property: marks, format
// characters, modifier letters and symbols, and the punctuation that can sit
// inside a word (apostrophes, full stops, colons, middle dots).
func caseIgnorable(r rune) bool {
	switch r {
	case '\'', '.', ':', 0xb7, 0x387, 0x55f, 0x2018, 0x2019, 0x2024, 0x2027, 0xfe13, 0xfe52, 0xfe55, 0xff07, 0xff0e, 0xff1a:
		return true
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk)
}
