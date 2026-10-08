package score

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuestion(t *testing.T) {
	tests := []struct {
		name, truth, got string
		want             bool
	}{
		// numbers
		{"number ignores $ and commas", "1000", "$1,000", true},
		{"a percent sign is dropped", "50", "50%", true},
		{"scientific notation", "1000", "1e3", true},
		{"a different number", "1000", "999", false},
		{"words are not numbers", "1000", "one thousand", false},
		{"no answer", "5", NoAnswer, false},
		{"a separator around a number", "7", "\x1f7\x1c", false},
		{"a truth with a separator around it is compared as a string", "\x1f7\x1c", "7.0", false},
		// lists
		{"list compares element-wise", "3, red", "3;Red", true},
		{"list length mismatch", "a, b", "a", false},
		{"order matters", "a, b", "b, a", false},
		{"numbers in a list are numbers", "1, 2, 3", "1;2.0;3", true},
		{"punctuation is kept inside list elements", "a.b, c", "ab, c", false},
		{"a comma in the truth makes a list: 1,000 is 1 and 000", "1,000", "1000", false},
		{"which is also how it matches", "1,000", "1,000", true},
		// strings
		{"string ignores case, spaces, punctuation", "Saint Petersburg", "saint-petersburg.", true},
		{"abbreviations are different", "Saint Petersburg", "St. Petersburg", false},
		{"seagull vs. sea gull", "sea gull", "seagull", true},
		// the official quirks
		{"an unreadable answer is infinity, which equals a truth of inf", "inf", "garbage", true},
		{"nan is never equal", "nan", "nan", false},
		{"no answer matches a truth of None", "None", NoAnswer, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, Question(tc.got, tc.truth))
		})
	}
}

// vectors are cases scored by the official scorer.py (see
// testdata/gen_scorer_vectors.py). The Go port must agree with all of them.
type vectors struct {
	Source string `json:"source"`
	SHA256 string `json:"source_sha256"`
	Python string `json:"python"`
	Cases  []struct {
		Answer       string `json:"answer"`
		AnswerIsNone bool   `json:"answer_is_none"`
		Truth        string `json:"truth"`
		Want         bool   `json:"want"`
	} `json:"cases"`
}

func TestAgreesWithTheOfficialScorer(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/scorer_vectors.json")
	require.NoError(t, err)
	var v vectors
	require.NoError(t, json.Unmarshal(raw, &v))

	require.GreaterOrEqual(t, len(v.Cases), 100, "EC-2 asks for at least 100 vectors")
	assert.NotEmpty(t, v.SHA256, "the vectors say which scorer.py they came from")
	assert.Contains(t, v.Source, "gaia-benchmark/leaderboard")

	var trues, falses, disagreements int
	for _, c := range v.Cases {
		answer := c.Answer
		if c.AnswerIsNone {
			answer = NoAnswer
		}
		if c.Want {
			trues++
		} else {
			falses++
		}
		if got := Question(answer, c.Truth); got != c.Want {
			disagreements++
			t.Errorf("answer %q, truth %q: official scorer says %v, the port says %v", answer, c.Truth, c.Want, got)
		}
	}
	assert.Zero(t, disagreements, "100%% agreement is the requirement")
	assert.Greater(t, trues, 20, "the vectors must hold both outcomes")
	assert.Greater(t, falses, 20, "the vectors must hold both outcomes")
	agreement := "all agree"
	if disagreements > 0 {
		agreement = fmt.Sprintf("%d disagree", disagreements)
	}
	t.Logf("%d vectors (%d true, %d false) from %s, Python %s: %s", len(v.Cases), trues, falses, v.SHA256[:12], v.Python, agreement)
}

func TestPyFloat(t *testing.T) {
	tests := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"1000", 1000, true},
		{"  12  ", 12, true},
		{"\t-3.5\n", -3.5, true},
		{"+.5", 0.5, true},
		{"5.", 5, true},
		{"5.e2", 500, true},
		{"1e3", 1000, true},
		{"1E-2", 0.01, true},
		{"1_000", 1000, true},
		{"1_0.0_1", 10.01, true},
		{"1e1_0", 1e10, true},
		{"٣", 3, true},   // Arabic-Indic three
		{"１２", 12, true}, // full-width
		{"١٢.٥", 12.5, true},
		{" 7　", 7, true},   // no-break space and ideographic space
		{"\v7\f", 7, true}, // a vertical tab and a form feed: float() skips the six ASCII spaces
		{"inf", math.Inf(1), true},
		{"-Infinity", math.Inf(-1), true},
		{"+INF", math.Inf(1), true},
		{"1e400", math.Inf(1), true},
		{"1e-400", 0, true},
		{"", 0, false},
		{" ", 0, false},
		{".", 0, false},
		{"+", 0, false},
		{"e5", 0, false},
		{"1e", 0, false},
		{"1e+", 0, false},
		{"1__0", 0, false},
		{"_1", 0, false},
		{"1_", 0, false},
		{"1_.5", 0, false},
		{"1._5", 0, false},
		{"0x10", 0, false},
		{"0b1", 0, false},
		{"1,000", 0, false},
		{"1 000", 0, false},
		{"--3", 0, false},
		{"+-3", 0, false},
		{"−3", 0, false},        // a Unicode minus sign is not a minus sign
		{"3\u200b", 0, false},   // a zero-width space is not white space
		{"\x1f7\x1c", 0, false}, // float() does not skip the information separators, which str.isspace counts
		{"\x1d7\x1e", 0, false},
		{"İnf", 0, false},
		{"infinit", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := pyFloat(tc.in)

			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.want, got)
			}
		})
	}

	t.Run("nan", func(t *testing.T) {
		for _, in := range []string{"nan", "NaN", "-nan", " +NAN "} {
			got, ok := pyFloat(in)
			assert.True(t, ok, in)
			assert.True(t, math.IsNaN(got), in)
		}
	})
}

func TestPyLower(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ABC", "abc"},
		{"Zürich", "zürich"},
		{"ÀÉÎ", "àéî"},
		{"İstanbul", "i̇stanbul"}, // capital dotted I becomes two characters
		{"ΑΣ", "ας"},              // ΑΣ: final sigma
		{"ΣΑ", "σα"},              // ΣΑ: not final
		{"ΑΣ Β", "ας β"},          // the end of a word, then another
		{"Σ", "σ"},                // alone: not preceded by a letter
		{"ΑΣ.", "ας."},            // a full stop is ignored when looking ahead, and kept
		{"Α'Σ", "α'ς"},            // so is an apostrophe when looking back
		{"ΑΣΑ", "ασα"},            // a letter follows: not final
		{"ǅ", "ǆ"},                // titlecase letters
		{"ẞ", "ß"},
		{"日本", "日本"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, pyLower(tc.in))
		})
	}
}

func TestSplitList(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"a,b;c", []string{"a", "b", "c"}},
		{"a, b", []string{"a", " b"}},
		{"", []string{""}},
		{"a,", []string{"a", ""}},
		{",a", []string{"", "a"}},
		{"a,,b", []string{"a", "", "b"}},
		{";;", []string{"", "", ""}},
		{"日本,語", []string{"日本", "語"}},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, splitList(tc.in))
		})
	}
}

func TestNormalizeString(t *testing.T) {
	tests := []struct {
		in          string
		removePunct bool
		want        string
	}{
		{"Saint-Petersburg.", true, "saintpetersburg"},
		{"Saint-Petersburg.", false, "saint-petersburg."},
		{"New\tYork\n  City", true, "newyorkcity"},
		{"O'Brien", true, "obrien"},
		{"“quoted”", true, "“quoted”"},                   // curly quotes are not ASCII punctuation
		{"Paris\u200bFrance", true, "paris\u200bfrance"}, // zero-width space is not white space
		{"", true, ""},
		{"!?.", true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeString(tc.in, tc.removePunct))
		})
	}
}

func TestNormalizeNumber(t *testing.T) {
	assert.Equal(t, 1000.0, normalizeNumber("$1,000"))
	assert.Equal(t, 50.0, normalizeNumber("50%"))
	assert.Equal(t, 1.5, normalizeNumber(" 1.5 "))
	assert.True(t, math.IsInf(normalizeNumber("about ten"), 1), "unreadable means +Inf, as in the official scorer")
	assert.True(t, math.IsInf(normalizeNumber(""), 1))
}
