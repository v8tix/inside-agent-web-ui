package devtools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/probe"
)

// auditWait is how long a Lighthouse audit may take.
const auditWait = 3 * time.Minute

// lighthouseScore matches a line of the category scores the tool prints:
// "- Accessibility: 95 (accessibility)".
var lighthouseScore = regexp.MustCompile(`(?m)^- [^:\n]+: (\d+) \(([a-z-]+)\)\s*$`)

// ErrNoLighthouseScores is returned when the audit's output holds no scores.
var ErrNoLighthouseScores = errors.New("the Lighthouse audit printed no category scores")

// ParseLighthouse reads the category scores of a Lighthouse audit, by category id
// ("accessibility", "best-practices", "seo"...).
func ParseLighthouse(text string) (map[string]float64, error) {
	scores := map[string]float64{}
	for _, m := range lighthouseScore.FindAllStringSubmatch(text, -1) {
		score, _ := strconv.ParseFloat(m[1], 64) // the pattern only lets digits through
		scores[m[2]] = score
	}
	if len(scores) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrNoLighthouseScores, shorten(text, 120))
	}
	return scores, nil
}

// labCLS matches the cumulative layout shift a trace summary reports.
var labCLS = regexp.MustCompile(`(?m)^\s*- CLS: ([0-9.]+)`)

// ErrNoCLS is returned when the trace summary holds no layout shift figure.
var ErrNoCLS = errors.New("the performance trace reported no layout shift")

// ParseCLS reads the lab cumulative layout shift from the summary of a stopped
// performance trace.
func ParseCLS(text string) (float64, error) {
	m := labCLS.FindStringSubmatch(text)
	if m == nil {
		return 0, fmt.Errorf("%w: %q", ErrNoCLS, shorten(text, 120))
	}
	return strconv.ParseFloat(m[1], 64)
}

// --- P-03 -------------------------------------------------------------------------------

// Lighthouse audits the page, on a desktop and on a phone, and reads the scores
// for accessibility and best practices.
func (p *Probes) Lighthouse(ctx context.Context) (domain.ProbeOutcome, error) {
	d, err := p.driver(ctx)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	defer p.tidy(d)

	// A page with a conversation on it is the more telling one to audit.
	if _, err := p.ask(ctx, d, p.Prompts.Short); err != nil {
		return domain.ProbeOutcome{}, err
	}

	scores := map[string]float64{}
	for _, device := range []string{"desktop", "mobile"} {
		res, err := d.Tool(ctx, "lighthouse_audit", map[string]any{"device": device, "mode": "navigation"}, WithTimeout(auditWait))
		if err != nil {
			return domain.ProbeOutcome{}, fmt.Errorf("auditing for %s: %w", device, err)
		}
		got, err := ParseLighthouse(res.Text)
		if err != nil {
			return domain.ProbeOutcome{}, err
		}
		if v, ok := got["accessibility"]; ok {
			scores[device+"_accessibility"] = v
		}
		if v, ok := got["best-practices"]; ok {
			scores[device+"_best_practices"] = v
		}
	}
	// The audit reloaded the page, and with it ended the observer.
	if err := d.Viewport(ctx, DefaultViewport); err != nil {
		p.log().Warn("could not restore the window size after the audit", "error", err)
	}
	return probe.JudgeLighthouse(scores, p.Thresholds), nil
}

// --- P-04 -------------------------------------------------------------------------------

// layoutOf is what layout.js returns.
type layoutOf struct {
	Overflow        float64 `json:"overflow"`
	Width           float64 `json:"width"`
	ComposerVisible bool    `json:"composerVisible"`
	Toggle          bool    `json:"toggle"`
}

const phoneViewport = "375x667x2,mobile,touch"

// Mobile looks at the page in a window the size of a phone's.
func (p *Probes) Mobile(ctx context.Context) (domain.ProbeOutcome, error) {
	d, err := p.driver(ctx)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	if err := d.Viewport(ctx, phoneViewport); err != nil {
		return domain.ProbeOutcome{}, err
	}
	defer func() {
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tidyWait)
		defer cancel()
		if err := d.Viewport(restoreCtx, DefaultViewport); err != nil {
			p.log().Warn("could not restore the window size", "error", err)
		}
	}()
	if err := d.StartChat(ctx); err != nil {
		return domain.ProbeOutcome{}, err
	}

	var l layoutOf
	if err := d.Eval(ctx, script("layout"), &l); err != nil {
		return domain.ProbeOutcome{}, err
	}
	m := probe.MobileMeasures{OverflowPx: l.Overflow, ComposerVisible: l.ComposerVisible}
	if l.Toggle {
		if err := d.Click(ctx, Query{Role: "button", Name: "Open conversations"}); err == nil {
			m.SidebarReachable = d.waitSidebar(ctx)
		} else {
			p.log().Warn("could not press the button that opens the conversations", "error", err)
		}
	}
	return probe.JudgeMobile(m), nil
}

// waitSidebar waits for the conversation list to slide into view.
func (d *Driver) waitSidebar(ctx context.Context) bool {
	return d.poll(ctx, d.settle(), func(ctx context.Context) (bool, error) {
		var shown bool
		err := d.tab.eval(ctx, script("sidebarShown"), &shown)
		return err == nil && shown, err
	}) == nil
}

// --- P-06 -------------------------------------------------------------------------------

type themeOf struct {
	Mode    *string `json:"mode"`
	Applied *string `json:"applied"`
}

func (t themeOf) mode() string {
	if t.Mode == nil {
		return ""
	}
	return *t.Mode
}

// Theme presses the theme switch and reloads the page to see whether the choice
// was kept.
func (p *Probes) Theme(ctx context.Context) (domain.ProbeOutcome, error) {
	d, err := p.driver(ctx)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	if err := d.StartChat(ctx); err != nil {
		return domain.ProbeOutcome{}, err
	}
	var before, after, reloaded themeOf
	if err := d.Eval(ctx, script("theme"), &before); err != nil {
		return domain.ProbeOutcome{}, err
	}
	switchTheme := Query{Role: "button", NameContains: "Theme"}
	if err := d.Click(ctx, switchTheme); err != nil {
		return domain.ProbeOutcome{}, err
	}
	if err := d.Eval(ctx, script("theme"), &after); err != nil {
		return domain.ProbeOutcome{}, err
	}
	if err := d.Reload(ctx); err != nil {
		return domain.ProbeOutcome{}, err
	}
	if err := d.Eval(ctx, script("theme"), &reloaded); err != nil {
		return domain.ProbeOutcome{}, err
	}
	m := probe.ThemeMeasures{
		Toggled:              after.mode() != "" && after.mode() != before.mode(),
		PersistedAfterReload: reloaded.mode() == after.mode() && after.mode() != "",
	}
	// Put it back the way it was found: the switch goes round auto, light, dark.
	for range 2 {
		if err := d.Click(ctx, switchTheme); err != nil {
			break
		}
	}
	return probe.JudgeTheme(m), nil
}

// --- P-08 -------------------------------------------------------------------------------

type focusOf struct {
	TestID *string `json:"testid"`
	Name   string  `json:"name"`
	Href   *string `json:"href"`
}

func (f focusOf) testID() string {
	if f.TestID == nil {
		return ""
	}
	return *f.TestID
}

// maxTabs bounds the walk to a control with the keyboard.
const maxTabs = 40

// tabTo presses key (Tab or Shift+Tab) until the focus is on an element with the
// test id, up to maxTabs times, and says whether it got there.
func (d *Driver) tabTo(ctx context.Context, key, testid string) (focusOf, bool) {
	for range maxTabs {
		var f focusOf
		if err := d.tab.eval(ctx, script("focus"), &f); err != nil {
			return f, false
		}
		if f.testID() == testid {
			return f, true
		}
		if d.tab.press(ctx, key) != nil {
			return f, false
		}
	}
	return focusOf{}, false
}

// KeyboardOnly sends a message, stops a reply and opens a conversation with the
// keyboard alone.
func (p *Probes) KeyboardOnly(ctx context.Context) (domain.ProbeOutcome, error) {
	d, err := p.driver(ctx)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	var m probe.KeyboardMeasures
	// The probe starts two conversations, and the driver shows one at a time.
	var started []string
	defer func() {
		for _, conv := range started {
			p.tidyChat(d, conv)
		}
	}()

	// 1. Send. The composer has the focus when the page opens; typing and Enter do the rest.
	if err := d.StartChat(ctx); err != nil {
		return domain.ProbeOutcome{}, err
	}
	d.ExpectNewReply(ctx)
	m.Sent = p.sendByKeyboard(ctx, d)
	started = append(started, d.Conversation())

	// 2. Stop: start a long reply the same way, then walk to the Stop button.
	if err := d.StartChat(ctx); err != nil {
		return domain.ProbeOutcome{}, err
	}
	m.Stopped = p.stopByKeyboard(ctx, d)
	started = append(started, d.Conversation())

	// 3. Navigate: walk back to a conversation in the list and open it.
	m.Navigated = d.openByKeyboard(ctx)
	return probe.JudgeKeyboard(m), nil
}

// typeInComposer types the text and presses Enter, if the message field is what
// has the focus: typing at anything else would do something else.
func (d *Driver) typeInComposer(ctx context.Context, text string) bool {
	f, ok := d.focusNow(ctx)
	return ok && f.testID() == "composer-input" && d.tab.typeText(ctx, text, "Enter") == nil
}

// sendByKeyboard sends the short prompt and waits for the reply it gets.
func (p *Probes) sendByKeyboard(ctx context.Context, d *Driver) bool {
	if !d.typeInComposer(ctx, p.Prompts.Short) {
		return false
	}
	reply, err := awaitFor(ctx, d, replyEnds)
	return err == nil && reply.Status == domain.ReplyComplete
}

// stopByKeyboard sends the long prompt, walks to the Stop button while the reply
// is being written, presses it, and waits for the reply to show as stopped.
func (p *Probes) stopByKeyboard(ctx context.Context, d *Driver) bool {
	if !d.typeInComposer(ctx, p.Prompts.Long) {
		return false
	}
	if status, _, err := untilText(ctx, d, replyStarts); err != nil || status.Terminal() {
		return false
	}
	if _, found := d.tabTo(ctx, "Tab", "stop"); !found || d.tab.press(ctx, "Enter") != nil {
		return false
	}
	reply, err := awaitFor(ctx, d, stopEnds)
	return err == nil && reply.Status == domain.ReplyStopped
}

// openByKeyboard walks back through the page to a conversation in the list and
// opens it, and waits for the page to be at its address.
func (d *Driver) openByKeyboard(ctx context.Context) bool {
	f, found := d.tabTo(ctx, "Shift+Tab", "conversation-link")
	return found && f.Href != nil && d.tab.press(ctx, "Enter") == nil && d.waitPath(ctx, *f.Href)
}

func (d *Driver) focusNow(ctx context.Context) (focusOf, bool) {
	var f focusOf
	if err := d.tab.eval(ctx, script("focus"), &f); err != nil {
		return f, false
	}
	return f, true
}

// waitPath waits for the page to be at the path.
func (d *Driver) waitPath(ctx context.Context, path string) bool {
	return d.poll(ctx, d.settle(), func(ctx context.Context) (bool, error) {
		st, err := d.pageState(ctx)
		return err == nil && st.Path == path, err
	}) == nil
}

// --- P-09 -------------------------------------------------------------------------------

// LayoutShift records a performance trace while a long reply arrives and reads
// how much the layout shifted.
func (p *Probes) LayoutShift(ctx context.Context) (domain.ProbeOutcome, error) {
	d, err := p.driver(ctx)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	defer p.tidy(d)
	if err := d.StartChat(ctx); err != nil {
		return domain.ProbeOutcome{}, err
	}
	if _, err := d.Tool(ctx, "performance_start_trace", map[string]any{"reload": false, "autoStop": false}); err != nil {
		return domain.ProbeOutcome{}, fmt.Errorf("starting the trace: %w", err)
	}
	// Whatever happens, the trace is stopped, or the next probe would find one running.
	stopped := false
	stop := func(c context.Context) (Result, error) {
		stopped = true
		return d.Tool(c, "performance_stop_trace", nil, WithTimeout(auditWait))
	}
	defer func() {
		if !stopped {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tidyWait)
			defer cancel()
			_, _ = stop(stopCtx)
		}
	}()

	if err := d.Submit(ctx, p.Prompts.Long); err != nil {
		return domain.ProbeOutcome{}, err
	}
	if _, err := awaitFor(ctx, d, replyEnds); err != nil {
		return domain.ProbeOutcome{}, err
	}
	res, err := stop(ctx)
	if err != nil {
		return domain.ProbeOutcome{}, fmt.Errorf("stopping the trace: %w", err)
	}
	cls, err := ParseCLS(res.Text)
	if err != nil {
		return domain.ProbeOutcome{}, err
	}
	return probe.JudgeLayoutShift(cls, p.Thresholds), nil
}
