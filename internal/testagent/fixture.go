package testagent

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// FixtureVersion is the version of the fixture file format.
const FixtureVersion = 1

// lookupTool is the name of the one tool the agent has: BuildAgent gives the agent
// a tool of this name, the #tool script of the model calls it, and a fixture may
// name no other.
const lookupTool = "lookup"

// Fixture scripts the agent for the evaluator's self-test: what it answers to
// each problem. A problem is recognized by an id written in its question, so
// the same fixture works with whatever prompt is wrapped around the question.
// No two ids may contain one another, so that an id in a question means one
// problem.
type Fixture struct {
	Version int                     `json:"version"`
	Replies map[string]FixtureReply `json:"replies"`

	mu     sync.Mutex
	failed map[string]int // failures given so far, by id and question
}

// FixtureReply is how the agent answers one problem.
type FixtureReply struct {
	// Text is the reply, streamed word by word.
	Text string `json:"text,omitempty"`
	// Error fails the run with this message instead of replying.
	Error string `json:"error,omitempty"`
	// WordDelayMs is the pause between words; absent means the model's own pace.
	WordDelayMs *int `json:"word_delay_ms,omitempty"`
	// FirstDelayMs is the pause before the first word, for a reply that takes
	// its time to start.
	FirstDelayMs int `json:"first_delay_ms,omitempty"`
	// Tool names a tool to call before answering, so that the reply carries
	// tool-call telemetry.
	Tool string `json:"tool,omitempty"`
	// FailFirst makes the first that many runs fail with Error (a default
	// message when it is empty) before Text is given: an agent that is down for
	// a moment, for testing Retry. The runs are counted for each distinct
	// question, so a question with something new in it (a number made up for the
	// occasion) fails afresh, however long the agent has been up.
	FailFirst int `json:"fail_first,omitempty"`
}

// LoadFixture reads a fixture file.
func LoadFixture(path string) (*Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the fixture: %w", err)
	}
	f, err := ParseFixture(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// ParseFixture reads and checks a fixture. A member it does not know is an
// error, so that a misspelled setting is not quietly ignored.
func ParseFixture(data []byte) (*Fixture, error) {
	var f Fixture
	if err := json.Unmarshal(data, &f, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("reading the fixture: %w", err)
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

func (f *Fixture) validate() error {
	if f.Version != FixtureVersion {
		return fmt.Errorf("fixture version %d is not supported (this agent reads version %d)", f.Version, FixtureVersion)
	}
	if len(f.Replies) == 0 {
		return errors.New("the fixture has no replies")
	}
	var problems []error
	for id, r := range f.Replies {
		if err := r.validate(id); err != nil {
			problems = append(problems, err)
		}
		if id == "" || strings.ContainsAny(id, " \t\r\n") {
			problems = append(problems, fmt.Errorf("fixture id %q must not be empty or hold whitespace", id))
		}
		for other := range f.Replies {
			if other != id && strings.Contains(other, id) {
				problems = append(problems, fmt.Errorf("fixture id %q is part of id %q, so a question could not name just one", id, other))
			}
		}
	}
	return errors.Join(problems...)
}

func (r FixtureReply) validate(id string) error {
	switch {
	case r.FailFirst < 0 || r.FirstDelayMs < 0 || (r.WordDelayMs != nil && *r.WordDelayMs < 0):
		return fmt.Errorf("fixture %q: delays and fail_first cannot be negative", id)
	case r.Tool != "" && r.Tool != lookupTool:
		return fmt.Errorf("fixture %q: the agent has no tool %q (it has %q)", id, r.Tool, lookupTool)
	case r.FailFirst > 0 && r.Text == "":
		return fmt.Errorf("fixture %q: fail_first needs a text to answer with once the failures are over", id)
	case r.FailFirst == 0 && (r.Text == "") == (r.Error == ""):
		return fmt.Errorf("fixture %q: give either text or error", id)
	}
	return nil
}

// match finds the problem named in text: the longest id it contains, and of
// several of that length the one that comes first in text. The order of the map
// has no say, so a question gets the same reply every time.
func (f *Fixture) match(text string) (id string, r FixtureReply, ok bool) {
	at := -1
	for candidate, reply := range f.Replies {
		i := strings.Index(text, candidate)
		if i < 0 {
			continue
		}
		if len(candidate) > len(id) || (len(candidate) == len(id) && i < at) {
			id, r, ok, at = candidate, reply, true, i
		}
	}
	return id, r, ok
}

// failNow counts a run of the problem, asked in these words, and reports whether
// it is one of the first that are to fail.
func (f *Fixture) failNow(id, question string, r FixtureReply) bool {
	if r.FailFirst == 0 {
		return false
	}
	key := id + "\x00" + question
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed == nil {
		f.failed = map[string]int{}
	}
	if f.failed[key] >= r.FailFirst {
		return false
	}
	f.failed[key]++
	return true
}
