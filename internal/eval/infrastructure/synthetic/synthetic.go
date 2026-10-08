// Package synthetic holds the evaluator's self-test set: thirteen invented
// problems (none of it is GAIA content), the answers the scripted agent gives to
// them, and the results a correct evaluator must find. The files are in data/
// and are embedded, so the evaluator carries its own self-test.
//
// The answers are in the format of the scripted agent's fixture (see
// testagent.Fixture); to run the agent with them, point TEST_AGENT_FIXTURE at
// data/synthetic_replies.json.
package synthetic

import (
	_ "embed"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/run"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/gaia"
)

//go:embed data/synthetic_gaia.jsonl
var problems []byte

//go:embed data/synthetic_replies.json
var replies []byte

//go:embed data/selftest_expected.json
var expected []byte

// Problems returns the invented problems.
func Problems() resource.ProblemSource {
	return gaia.NewSyntheticBytes("synthetic_gaia.jsonl", problems)
}

// Replies returns the file that scripts the agent's answers to them.
func Replies() []byte { return append([]byte(nil), replies...) }

// Expected returns what a run over them must find.
func Expected() (run.Expected, error) { return run.ParseExpected(expected) }
