package run_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
)

// browser is a pretend Web UI shared by the drivers of one test. It knows what
// the agent answers to each question, how long it takes, and when a driver dies.
type browser struct {
	mu sync.Mutex

	answers map[string]string // question -> the raw reply
	latency time.Duration     // how long a reply takes
	// dieOn makes a driver die when it is given the question, that many times.
	dieOn map[string]int
	// onAwait runs when a reply is being waited for; tests use it to cancel.
	onAwait func(ctx context.Context, question string)

	loginErr, newErr, closeErr error
	// failNewAfter makes NewDriver fail from that many drivers on (0: never).
	failNewAfter int

	// alwaysFailConsole makes every page show a console error.
	alwaysFailConsole bool

	asked   []string
	created int
	closed  int
	deleted int
	shots   int
	logins  int
}

func newBrowser(answers map[string]string) *browser {
	return &browser{answers: answers, dieOn: map[string]int{}}
}

func (b *browser) NewDriver(context.Context) (resource.UIDriver, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.created++
	if b.newErr != nil || (b.failNewAfter > 0 && b.created > b.failNewAfter) {
		b.created--
		if b.newErr != nil {
			return nil, b.newErr
		}
		return nil, fmt.Errorf("no more browsers")
	}
	return &fakeDriver{b: b}, nil
}

func (b *browser) questions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.asked)
}

func (b *browser) counts() (created, closed, logins int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.created, b.closed, b.logins
}

// fakeDriver is one browser session of the pretend UI.
type fakeDriver struct {
	b        *browser
	question string
	dead     bool
}

func (d *fakeDriver) alive() error {
	if d.dead {
		return fmt.Errorf("mcp: %w", domain.ErrDriverDied)
	}
	return nil
}

func (d *fakeDriver) Login(context.Context, string, string) error {
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	d.b.logins++
	return d.b.loginErr
}

func (d *fakeDriver) StartChat(context.Context) error { return d.alive() }

func (d *fakeDriver) Submit(_ context.Context, text string) error {
	if err := d.alive(); err != nil {
		return err
	}
	_, question, _ := strings.Cut(text, "Question: ")
	d.question = question
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	d.b.asked = append(d.b.asked, question)
	if d.b.dieOn[question] > 0 {
		d.b.dieOn[question]--
		d.dead = true
		return fmt.Errorf("mcp: %w", domain.ErrDriverDied)
	}
	return nil
}

func (d *fakeDriver) AwaitReply(ctx context.Context) (domain.Reply, error) {
	if hook := d.b.onAwait; hook != nil {
		hook(ctx, d.question)
	}
	if ctx.Err() != nil { // a real driver notices a cancelled context before it looks at the page
		return domain.Reply{}, fmt.Errorf("waiting: %w", domain.ErrReplyTimeout)
	}
	select {
	case <-time.After(d.b.latency):
	case <-ctx.Done():
		return domain.Reply{}, fmt.Errorf("waiting: %w", domain.ErrReplyTimeout)
	}
	return domain.Reply{ConversationID: "c-" + d.question, MessageID: "m1", Status: domain.ReplyComplete}, nil
}

func (d *fakeDriver) Stop(context.Context) error { return nil }

func (d *fakeDriver) ReadRaw(context.Context, domain.Reply) (string, error) {
	return d.b.answers[d.question], nil
}

func (d *fakeDriver) Collect(context.Context) (domain.UIObservations, error) {
	obs := healthyObs()
	obs.VisibleText = d.b.answers[d.question]
	if d.b.alwaysFailConsole {
		obs.Console = []domain.ConsoleMessage{{Type: "error", Text: "boom"}}
	}
	return obs, nil
}

func (d *fakeDriver) Screenshot(context.Context, string) error {
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	d.b.shots++
	return nil
}
func (d *fakeDriver) DeleteChat(context.Context) error {
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	d.b.deleted++
	return nil
}

func (d *fakeDriver) Close(context.Context) error {
	d.b.mu.Lock()
	defer d.b.mu.Unlock()
	d.b.closed++
	return d.b.closeErr
}

// memStore keeps a run's files in memory.
type memStore struct {
	mu        sync.Mutex
	rows      map[string][]domain.Result
	metas     map[string]domain.RunMeta
	probes    map[string][]domain.ProbeOutcome
	began     []domain.RunMeta
	finished  []domain.RunMeta
	summaries []domain.Summary
	// summarized is the meta each summary was written with.
	summarized []domain.RunMeta
	appendErr  error
	// fail makes the named method return the error: Begin, Index, Load, LoadMeta, LoadProbes, WriteSummary, Finish.
	fail map[string]error
}

func newMemStore() *memStore {
	return &memStore{rows: map[string][]domain.Result{}, metas: map[string]domain.RunMeta{}, probes: map[string][]domain.ProbeOutcome{}, fail: map[string]error{}}
}

func (m *memStore) Begin(_ context.Context, meta domain.RunMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail["Begin"]; err != nil {
		return err
	}
	// Beginning a run that exists resumes it, and it keeps the time it first started.
	if previous, ok := m.metas[meta.RunID]; ok && !previous.StartedAt.IsZero() {
		meta.StartedAt = previous.StartedAt
	}
	m.began = append(m.began, meta)
	m.metas[meta.RunID] = meta
	return nil
}

func (m *memStore) Append(_ context.Context, runID string, row domain.Result) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appendErr != nil {
		return m.appendErr
	}
	m.rows[runID] = append(m.rows[runID], row)
	return nil
}

func (m *memStore) Load(_ context.Context, runID string) ([]domain.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail["Load"]; err != nil {
		return nil, err
	}
	return slices.Clone(m.rows[runID]), nil
}

func (m *memStore) Index(_ context.Context, runID string) (map[domain.AttemptKey]struct{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail["Index"]; err != nil {
		return nil, err
	}
	// As the real store does: an attempt the page broke is not finished, and the
	// last row for a slot decides.
	finished := map[domain.AttemptKey]bool{}
	for _, r := range m.rows[runID] {
		finished[r.Key()] = r.Status != domain.StatusUIError
	}
	index := map[domain.AttemptKey]struct{}{}
	for key, done := range finished {
		if done {
			index[key] = struct{}{}
		}
	}
	return index, nil
}

func (m *memStore) Finish(_ context.Context, meta domain.RunMeta) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail["Finish"]; err != nil {
		return err
	}
	m.finished = append(m.finished, meta)
	return nil
}

func (m *memStore) LoadMeta(_ context.Context, runID string) (domain.RunMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail["LoadMeta"]; err != nil {
		return domain.RunMeta{}, err
	}
	return m.metas[runID], nil
}

func (m *memStore) SaveProbes(_ context.Context, runID string, probes []domain.ProbeOutcome) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.probes[runID] = probes
	return nil
}

func (m *memStore) LoadProbes(_ context.Context, runID string) ([]domain.ProbeOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail["LoadProbes"]; err != nil {
		return nil, err
	}
	return m.probes[runID], nil
}

func (m *memStore) WriteSummary(_ context.Context, meta domain.RunMeta, s domain.Summary) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail["WriteSummary"]; err != nil {
		return err
	}
	m.summarized = append(m.summarized, meta)
	m.summaries = append(m.summaries, s)
	return nil
}

func (m *memStore) EvidencePath(_ context.Context, _, name string) (string, error) {
	return "/evidence/" + name, nil
}

// hangingStore is a store that has stopped answering: the call it is told to
// hang on waits until its context ends, and says so. If nothing ends it, an hour
// of the test's clock does, so that a missing limit fails a test with a message
// and does not stop the program of the test.
type hangingStore struct {
	*memStore
	on string // WriteSummary or Finish
}

func hang(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Hour):
		return errors.New("nothing ended the wait")
	}
}

func (h hangingStore) WriteSummary(ctx context.Context, meta domain.RunMeta, s domain.Summary) error {
	if h.on == "WriteSummary" {
		return hang(ctx)
	}
	return h.memStore.WriteSummary(ctx, meta, s)
}

func (h hangingStore) Finish(ctx context.Context, meta domain.RunMeta) error {
	if h.on == "Finish" {
		return hang(ctx)
	}
	return h.memStore.Finish(ctx, meta)
}

// results returns the rows of a run, ordered by task and attempt.
func (m *memStore) results(runID string) []domain.Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := slices.Clone(m.rows[runID])
	slices.SortFunc(rows, func(a, b domain.Result) int {
		if c := strings.Compare(a.TaskID, b.TaskID); c != 0 {
			return c
		}
		return a.Attempt - b.Attempt
	})
	return rows
}

func statuses(rows []domain.Result) map[string]domain.AttemptStatus {
	out := map[string]domain.AttemptStatus{}
	for _, r := range rows {
		out[fmt.Sprintf("%s#%d", r.TaskID, r.Attempt)] = r.Status
	}
	return out
}
