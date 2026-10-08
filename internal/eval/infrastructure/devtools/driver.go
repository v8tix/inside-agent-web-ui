package devtools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/redact"
)

// DefaultViewport is the window the driver works in unless told otherwise.
const DefaultViewport = "1280x800x1"

// SupportedContract is the Web UI contract version this driver was written for
// (internal/contract). A page that says another version is refused. A test keeps
// it equal to the contract package's, so that raising one makes you look at the
// other.
const SupportedContract = "1"

var _ resource.UIDriver = (*Driver)(nil)

// DriverConfig says how a driver behaves.
type DriverConfig struct {
	// BaseURL is the Web UI, such as http://localhost:8300. It can carry
	// credentials: the browser is given it as it is, and an error names it as
	// redact.URL shows it.
	BaseURL string
	// HumanTyping types the prompt as keystrokes, a word at a time with a pause
	// between words, instead of putting it in the field at once.
	HumanTyping bool
	// TypingDelay is the pause per character when HumanTyping is on.
	TypingDelay time.Duration
	// Viewport is the size of the browser window as the emulate tool spells it
	// (width x height x pixel ratio); zero means a laptop, 1280x800x1. The default
	// window of a headless Chrome is taller than most replies, which would leave
	// the scroll check with nothing to do, and differs from machine to machine.
	Viewport string
	// PollInterval is how often AwaitReply looks at the reply; zero means 250 ms.
	PollInterval time.Duration
	// SettleTimeout is how long to wait for a page to show what it should after
	// a navigation or a click; zero means 15 seconds.
	SettleTimeout time.Duration
	Logger        *slog.Logger
}

// Driver uses the Web UI the way a person does, through Chrome DevTools MCP: it
// finds elements by their accessible names in the page's accessibility tree,
// clicks and types, and reads the page's state through the data attributes of
// the contract. One driver is one browser tab and is not safe for concurrent use.
type Driver struct {
	cfg DriverConfig
	s   *Session
	tab tab
	log *slog.Logger

	conversation string // the id of the conversation in the tab, once it has one
	// prevReply is the newest reply the page had when the last message was sent.
	// It is over, and AwaitReply must not mistake it for the one being written.
	prevReply string

	// report, when set, is told the version of a tool as it becomes known.
	report func(name, version string)
}

// NewDriver wraps a session. The tab is opened by Login.
func NewDriver(s *Session, cfg DriverConfig) *Driver {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Driver{cfg: cfg, s: s, tab: tab{s: s}, log: log}
}

// Click clicks the element a query finds in a fresh snapshot.
func (d *Driver) Click(ctx context.Context, q Query) error { return d.tab.clickNamed(ctx, q) }

// Fill puts text in the element a query finds.
func (d *Driver) Fill(ctx context.Context, q Query, value string) error {
	snap, err := d.tab.snapshot(ctx)
	if err != nil {
		return err
	}
	node, err := snap.Require(q)
	if err != nil {
		return err
	}
	return d.tab.fill(ctx, node.UID, value)
}

// Reload loads the page again, with the observer in it, and waits for the page
// to show its composer.
func (d *Driver) Reload(ctx context.Context) error {
	if _, err := d.tab.call(ctx, "navigate_page", map[string]any{"type": "reload", "initScript": observerScript()}); err != nil {
		return fmt.Errorf("reloading the page: %w", err)
	}
	_, err := d.waitFor(ctx, "the composer after a reload", func(st pageState) bool { return st.Composer })
	return err
}

// Viewport sets the size of the window as the emulate tool spells it, such as
// "375x667x2,mobile,touch" or "1280x800x1".
func (d *Driver) Viewport(ctx context.Context, spec string) error {
	if _, err := d.tab.call(ctx, "emulate", map[string]any{"viewport": spec}); err != nil {
		return fmt.Errorf("setting the size of the browser window: %w", err)
	}
	return nil
}

// ExpectNewReply notes the newest reply on the page, so that AwaitReply waits for
// the one that the next step brings (Regenerate, Retry), not for this one.
func (d *Driver) ExpectNewReply(ctx context.Context) { d.markCurrentReply(ctx) }

// Progress says how far the newest reply has got: its status and how many
// characters of text it shows. found is false when the page has no reply yet.
// A reply that is there tells which conversation it is in, so that a probe that
// goes no further than this can still tidy up after itself.
func (d *Driver) Progress(ctx context.Context) (status domain.ReplyStatus, chars int, found bool, err error) {
	r, err := d.readReply(ctx)
	if err != nil || !r.Found || (d.prevReply != "" && r.ID != nil && *r.ID == d.prevReply) {
		return "", 0, false, err
	}
	if r.Conversation != nil {
		d.conversation = *r.Conversation
	}
	return r.reply().Status, r.Chars, true, nil
}

// Conversation is the id of the conversation in the tab, once it has one.
func (d *Driver) Conversation() string { return d.conversation }

// Eval runs a function in the page and decodes the JSON it returns into out. It
// is for the probes and the tests, which do things to the page that an attempt
// never does (scroll away, resize, reload).
func (d *Driver) Eval(ctx context.Context, function string, out any) error {
	return d.tab.eval(ctx, function, out)
}

// Tool calls a Chrome DevTools MCP tool on the driver's own tab.
func (d *Driver) Tool(ctx context.Context, name string, args map[string]any, opts ...CallOption) (Result, error) {
	return d.tab.call(ctx, name, args, opts...)
}

func (d *Driver) interval() time.Duration { return cmp.Or(d.cfg.PollInterval, 250*time.Millisecond) }
func (d *Driver) settle() time.Duration   { return cmp.Or(d.cfg.SettleTimeout, 15*time.Second) }

// pageState is what pageState.js returns.
type pageState struct {
	Contract *string `json:"contract"`
	Path     string  `json:"path"`
	Composer bool    `json:"composer"`
	Login    bool    `json:"login"`
	Alert    *string `json:"alert"`
}

func (d *Driver) pageState(ctx context.Context) (pageState, error) {
	var st pageState
	err := d.tab.eval(ctx, script("pageState"), &st)
	return st, err
}

// checkContract refuses a page that does not speak the contract version this
// driver knows, before anything is typed into it.
func (d *Driver) checkContract(ctx context.Context) error {
	st, err := d.pageState(ctx)
	if err != nil {
		return err
	}
	switch {
	case st.Contract == nil:
		return fmt.Errorf("%w: the page at %s declares no UI contract version, so it is not the Web UI this evaluator drives", domain.ErrContractVersion, redact.URL(d.cfg.BaseURL))
	case *st.Contract != SupportedContract:
		return fmt.Errorf("%w: the page says data-ui-contract=%q, this evaluator was built for %q", domain.ErrContractVersion, *st.Contract, SupportedContract)
	}
	return nil
}

// poll runs look every interval until it says it is done, for at most within. A
// look that fails because the page is changing under the script, such as a
// navigation in progress, is tried again; one that fails because the browser is
// gone, or because the time is up, ends the wait with its error.
func (d *Driver) poll(ctx context.Context, within time.Duration, look func(ctx context.Context) (done bool, err error)) error {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	for {
		done, err := look(ctx)
		switch {
		case err == nil && done:
			return nil
		case err != nil && (ctx.Err() != nil || errors.Is(err, domain.ErrDriverDied)):
			return err
		}
		if err := sleep(ctx, d.interval()); err != nil {
			return err
		}
	}
}

// waitFor polls the page state until ok says it is what is wanted, the context
// ends, or the settle timeout is up.
func (d *Driver) waitFor(ctx context.Context, what string, ok func(pageState) bool) (pageState, error) {
	var last pageState
	err := d.poll(ctx, d.settle(), func(ctx context.Context) (bool, error) {
		st, err := d.pageState(ctx)
		if err != nil {
			return false, err
		}
		last = st
		return ok(st), nil
	})
	if err != nil {
		return last, fmt.Errorf("waiting for %s (the page is at %s): %w", what, last.Path, err)
	}
	return last, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Login opens a tab on the sign-in page, checks the page speaks the contract
// this driver knows, and signs in.
func (d *Driver) Login(ctx context.Context, email, password string) error {
	res, err := d.s.Call(ctx, "new_page", map[string]any{"url": d.cfg.BaseURL + "/login"})
	if err != nil {
		return fmt.Errorf("opening the Web UI at %s: %w", redact.URL(d.cfg.BaseURL), err)
	}
	page, ok := SelectedPage(ParsePages(res.Text))
	if !ok {
		return fmt.Errorf("opening the Web UI: the browser reported no page: %q", shorten(res.Text, 120))
	}
	d.tab.id = page.ID
	if _, err := d.tab.call(ctx, "emulate", map[string]any{"viewport": cmp.Or(d.cfg.Viewport, DefaultViewport)}); err != nil {
		return fmt.Errorf("setting the size of the browser window: %w", err)
	}

	if err := d.checkContract(ctx); err != nil {
		return err
	}
	snap, err := d.tab.snapshot(ctx)
	if err != nil {
		return err
	}
	emailBox, err := snap.Require(Query{Role: "textbox", Name: "Email"})
	if err != nil {
		return fmt.Errorf("the sign-in page: %w", err)
	}
	passwordBox, err := snap.Require(Query{Role: "textbox", Name: "Password"})
	if err != nil {
		return fmt.Errorf("the sign-in page: %w", err)
	}
	if err := d.tab.fill(ctx, emailBox.UID, email); err != nil {
		return fmt.Errorf("typing the email: %w", err)
	}
	if err := d.tab.fill(ctx, passwordBox.UID, password); err != nil {
		return fmt.Errorf("typing the password: %w", err)
	}
	if err := d.tab.clickNamed(ctx, Query{Role: "button", Name: "Sign in"}); err != nil {
		return fmt.Errorf("pressing Sign in: %w", err)
	}

	st, err := d.waitFor(ctx, "sign-in to finish", func(st pageState) bool { return !st.Login || st.Alert != nil })
	if err != nil {
		return err
	}
	if st.Login {
		msg := "the sign-in page is still showing"
		if st.Alert != nil {
			msg = fmt.Sprintf("the page said: %s", shorten(*st.Alert, 120))
		}
		return fmt.Errorf("%w: %s", domain.ErrLoginFailed, msg)
	}
	if err := d.checkContract(ctx); err != nil {
		return err
	}
	d.noteBrowserVersion(ctx)
	return nil
}

// chromePattern finds the version in a user agent such as
// "Mozilla/5.0 ... HeadlessChrome/143.0.7499.0 Safari/537.36".
var chromePattern = regexp.MustCompile(`Chrome/(\d+(?:\.\d+)*)`)

// noteBrowserVersion tells the report hook which Chrome this is. It is for the
// record only, so a page that will not say is not a reason to stop.
func (d *Driver) noteBrowserVersion(ctx context.Context) {
	if d.report == nil {
		return
	}
	var ua string
	if err := d.tab.eval(ctx, script("userAgent"), &ua); err != nil {
		d.log.Debug("could not read the browser's version", "error", err)
		return
	}
	if m := chromePattern.FindStringSubmatch(ua); m != nil {
		d.report("chrome", m[1])
	}
}

// StartChat opens the start page, where a message starts a new conversation.
func (d *Driver) StartChat(ctx context.Context) error {
	if err := d.tab.navigate(ctx, d.cfg.BaseURL+"/", observerScript()); err != nil {
		return fmt.Errorf("opening the start page: %w", err)
	}
	st, err := d.waitFor(ctx, "the composer", func(st pageState) bool { return st.Composer || st.Login })
	if err != nil {
		return err
	}
	if st.Login {
		// The web session ended (it has a lifetime). A new browser session signs in
		// again, which is what ErrDriverDied makes the runner do.
		return fmt.Errorf("%w: the session ended, the page asks to sign in again", domain.ErrDriverDied)
	}
	d.conversation, d.prevReply = "", ""
	return nil
}

// markCurrentReply remembers the newest reply on the page. Looking is for the
// record only: on an empty page there is nothing to remember, and a page that
// will not say is dealt with by the steps that follow.
func (d *Driver) markCurrentReply(ctx context.Context) {
	d.prevReply = ""
	if r, err := d.readReply(ctx); err == nil && r.Found && r.ID != nil {
		d.prevReply = *r.ID
	}
}

// Submit puts the text in the composer and presses Enter.
func (d *Driver) Submit(ctx context.Context, text string) error {
	d.markCurrentReply(ctx)
	snap, err := d.tab.snapshot(ctx)
	if err != nil {
		return err
	}
	box, err := snap.Require(Query{Role: "textbox", Name: "Message"})
	if err != nil {
		return fmt.Errorf("the composer: %w", err)
	}
	// A tab typed on a keyboard moves the focus out of the field, so text with one
	// is put in whole, to reach the agent as it is.
	if d.cfg.HumanTyping && !strings.ContainsRune(text, '\t') {
		if err := d.tab.click(ctx, box.UID); err != nil {
			return fmt.Errorf("focusing the composer: %w", err)
		}
		if err := d.typeLikeAPerson(ctx, text); err != nil {
			return err
		}
		return wrap("pressing Enter", d.tab.press(ctx, "Enter"))
	}
	if err := d.tab.fill(ctx, box.UID, text); err != nil {
		return fmt.Errorf("filling the composer: %w", err)
	}
	return wrap("pressing Enter", d.tab.press(ctx, "Enter"))
}

func wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}

// typeLikeAPerson sends the text as keystrokes a word at a time, pausing as long
// as the typist would between words. A line break is Shift+Enter, as a person
// adds one: the composer sends on Enter, and the browser's keyboard would type
// "\n" as Enter, which would send the message at its first line.
func (d *Driver) typeLikeAPerson(ctx context.Context, text string) error {
	words := wordsWithSpaces(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text))
	for i, w := range words {
		if err := d.typeWord(ctx, w); err != nil {
			return fmt.Errorf("typing: %w", err)
		}
		if i < len(words)-1 {
			if err := sleep(ctx, d.cfg.TypingDelay*time.Duration(len([]rune(w)))); err != nil {
				return err
			}
		}
	}
	return nil
}

// typeWord types a piece of text: its lines as keystrokes, with Shift+Enter
// between them.
func (d *Driver) typeWord(ctx context.Context, word string) error {
	lines := strings.Split(word, "\n")
	for i, line := range lines {
		if line != "" {
			if err := d.tab.typeText(ctx, line, ""); err != nil {
				return err
			}
		}
		if i < len(lines)-1 {
			if err := d.tab.press(ctx, "Shift+Enter"); err != nil {
				return err
			}
		}
	}
	return nil
}

// wordsWithSpaces splits text into pieces that each end with the whitespace
// after a word, so that joining them gives the text back.
func wordsWithSpaces(text string) []string {
	var out []string
	start := 0
	for i, r := range text {
		if r == ' ' || r == '\n' || r == '\t' {
			// keep runs of whitespace with the word before them
			end := i + len(string(r))
			out = append(out, text[start:end])
			start = end
		}
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

// lastReply is what lastReply.js returns.
type lastReply struct {
	Found            bool    `json:"found"`
	Conversation     *string `json:"conversation"`
	ID               *string `json:"id"`
	Status           *string `json:"status"`
	TTFT             *string `json:"ttft"`
	Duration         *string `json:"duration"`
	PromptTokens     *string `json:"promptTokens"`
	CompletionTokens *string `json:"completionTokens"`
	ToolCalls        *string `json:"toolCalls"`
	Error            *string `json:"error"`
	RawURL           *string `json:"rawUrl"`
	Chars            int     `json:"chars"`
}

func (d *Driver) readReply(ctx context.Context) (lastReply, error) {
	var r lastReply
	err := d.tab.eval(ctx, script("lastReply"), &r)
	return r, err
}

func (r lastReply) reply() domain.Reply {
	rep := domain.Reply{Status: domain.ReplyPending}
	if r.Conversation != nil {
		rep.ConversationID = *r.Conversation
	}
	if r.ID != nil {
		rep.MessageID = *r.ID
	}
	if r.Status != nil {
		rep.Status = domain.ReplyStatus(*r.Status)
	}
	if r.Error != nil {
		rep.Error = *r.Error
	}
	return rep
}

// maxScriptErrors is how many times in a row reading the reply may fail before
// the page is given up on. A failure or two is normal while the page swaps its
// content.
const maxScriptErrors = 20

// AwaitReply waits until the assistant message has a final status. When ctx
// ends first, it returns what it knows and an error wrapping
// domain.ErrReplyTimeout.
func (d *Driver) AwaitReply(ctx context.Context) (domain.Reply, error) {
	var last domain.Reply
	failures := 0
	for {
		r, err := d.readReply(ctx)
		switch {
		case err == nil:
			failures = 0
			if r.Found && (d.prevReply == "" || r.ID == nil || *r.ID != d.prevReply) {
				last = r.reply()
				if r.Conversation != nil {
					d.conversation = *r.Conversation
				}
				if last.Status.Terminal() {
					return last, nil
				}
			}
		case errors.Is(err, domain.ErrDriverDied):
			return last, err
		case ctx.Err() != nil:
			return last, fmt.Errorf("waiting for the reply: %w", errors.Join(domain.ErrReplyTimeout, ctx.Err()))
		default:
			if failures++; failures >= maxScriptErrors {
				return last, fmt.Errorf("reading the reply failed %d times in a row: %w", failures, err)
			}
		}
		if err := sleep(ctx, d.interval()); err != nil {
			return last, fmt.Errorf("waiting for the reply: %w", errors.Join(domain.ErrReplyTimeout, err))
		}
	}
}

// ErrNoStopButton is returned by Stop when the page shows no Stop button: the
// reply has finished, or the page does not offer one.
var ErrNoStopButton = errors.New("there is no Stop button on the page")

// Stop presses the Stop button.
func (d *Driver) Stop(ctx context.Context) error {
	err := d.tab.clickNamed(ctx, Query{Role: "button", Name: "Stop generating"})
	if _, missing := errors.AsType[*NotFoundError](err); missing {
		return fmt.Errorf("%w: %v", ErrNoStopButton, err)
	}
	return wrap("pressing Stop", err)
}

// rawPath is the shape of the address behind the Copy button: /c/<id>/m/<id>/raw,
// with ids made of letters, digits, "-" and "_".
var rawPath = regexp.MustCompile(`^/c/[0-9A-Za-z_-]+/m/[0-9A-Za-z_-]+/raw$`)

// sameOriginPath reports whether an address is the raw-text path of a reply on
// the page's own server, and nothing else. The raw text is read with the page's
// session, so it is only ever asked of the Web UI. Checking the whole shape
// rather than "starts with a slash" matters: Chrome drops tabs and line breaks
// from an address before it reads it, so "/<tab>/evil.example/x" is a way to
// another host, and a prefix check lets it through.
func sameOriginPath(address string) bool { return rawPath.MatchString(address) }

// rawResponse is what fetchRaw.js returns.
type rawResponse struct {
	Status int    `json:"status"`
	Text   string `json:"text"`
}

// ReadRaw returns the reply as it is stored, the Markdown the agent wrote, from
// the endpoint behind the Copy button. It is the one place the evaluator asks
// the Web UI for data without going through what a person sees, and it does so
// from inside the page, with the page's own session.
func (d *Driver) ReadRaw(ctx context.Context, reply domain.Reply) (string, error) {
	r, err := d.readReply(ctx)
	if err != nil {
		return "", fmt.Errorf("finding the reply: %w", err)
	}
	if !r.Found || r.RawURL == nil || *r.RawURL == "" {
		return "", errors.New("the reply has no Copy button, so there is no address to read it from")
	}
	if !sameOriginPath(*r.RawURL) {
		return "", fmt.Errorf("the Copy button's address %q is not a path on the Web UI itself; it is not fetched", shorten(*r.RawURL, 80))
	}
	if reply.MessageID != "" && r.ID != nil && *r.ID != reply.MessageID {
		return "", fmt.Errorf("the newest reply on the page is not the one asked for (%s)", reply.MessageID)
	}
	var resp rawResponse
	if err := d.tab.eval(ctx, scriptWith("fetchRaw", *r.RawURL), &resp); err != nil {
		return "", err
	}
	if resp.Status != 200 {
		return "", fmt.Errorf("the reply's raw text answered HTTP %d", resp.Status)
	}
	return resp.Text, nil
}

// Screenshot saves the page as a PNG at path.
func (d *Driver) Screenshot(ctx context.Context, path string) error {
	res, err := d.tab.call(ctx, "take_screenshot", map[string]any{"format": "png"})
	if err != nil {
		return err
	}
	if len(res.Images) == 0 {
		return errors.New("the browser returned no picture")
	}
	return os.WriteFile(path, res.Images[0].Data, 0o600)
}

// RowAction is a button on a conversation's row in the sidebar.
type RowAction string

// The buttons of a row.
const (
	RowRename RowAction = "rename"
	RowDelete RowAction = "delete"
)

// rowButton is where the page says a row's button is: its accessible name, and
// how many buttons with that name come before it.
type rowButton struct {
	Name   string `json:"name"`
	Before int    `json:"before"`
}

// PressRowButton presses the rename or delete button of a conversation's row in
// the sidebar, however many other rows there are and whatever their titles.
// Every question of an evaluation opens with the same prompt, so every row has
// the same title and every button the same name; the page says how many come
// first, and that many are passed over. Pressing the first button with the name
// would delete another conversation, possibly one that is still being written.
func (d *Driver) PressRowButton(ctx context.Context, action RowAction, conversation string) error {
	var found *rowButton
	arg := map[string]string{"kind": string(action), "id": conversation}
	if err := d.tab.eval(ctx, scriptWith("rowButton", arg), &found); err != nil {
		return err
	}
	if found == nil {
		return fmt.Errorf("the sidebar has no row for conversation %s", conversation)
	}
	if err := d.tab.clickNth(ctx, Query{Role: "button", Name: found.Name}, found.Before); err != nil {
		return fmt.Errorf("pressing the %s button: %w", action, err)
	}
	return nil
}

// DeleteChat deletes the conversation in the tab through the sidebar: the
// delete button of its row, then Delete in the confirmation.
func (d *Driver) DeleteChat(ctx context.Context) error {
	if d.conversation == "" {
		return errors.New("there is no conversation to delete")
	}
	if err := d.PressRowButton(ctx, RowDelete, d.conversation); err != nil {
		return err
	}
	if err := d.waitDialog(ctx); err != nil {
		return err
	}
	if err := d.tab.clickNamed(ctx, Query{Role: "button", Name: "Delete"}); err != nil {
		return fmt.Errorf("confirming the deletion: %w", err)
	}

	err := d.poll(ctx, d.settle(), func(ctx context.Context) (bool, error) {
		var exists bool
		err := d.tab.eval(ctx, scriptWith("rowExists", d.conversation), &exists)
		return err == nil && !exists, err
	})
	if err != nil {
		return fmt.Errorf("waiting for the conversation to leave the sidebar: %w", err)
	}
	d.conversation = ""
	return nil
}

// waitDialog waits for the confirmation to be on the page.
func (d *Driver) waitDialog(ctx context.Context) error {
	err := d.poll(ctx, d.settle(), func(ctx context.Context) (bool, error) {
		snap, err := d.tab.snapshot(ctx)
		if err != nil {
			return false, err
		}
		_, found := snap.Find(Query{Role: "button", Name: "Delete"})
		return found, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for the delete confirmation: %w", err)
	}
	return nil
}

// Close ends the browser session.
func (d *Driver) Close(ctx context.Context) error { return d.s.Close(ctx) }

// Factory makes drivers, each with a browser of its own. Use it by pointer.
type Factory struct {
	Session Options
	Driver  DriverConfig

	mu       sync.Mutex
	versions map[string]string
}

var _ resource.DriverFactory = (*Factory)(nil)

// NewDriver starts a browser and returns a driver for it.
func (f *Factory) NewDriver(ctx context.Context) (resource.UIDriver, error) {
	return f.Open(ctx)
}

// Open is NewDriver, returning the driver itself, for the probes that use more
// of it than the runner does.
func (f *Factory) Open(ctx context.Context) (*Driver, error) {
	s, err := Open(ctx, f.Session)
	if err != nil {
		return nil, err
	}
	if v := s.Info().Version; v != "" {
		f.note("chrome-devtools-mcp", v)
	}
	d := NewDriver(s, f.Driver)
	d.report = f.note
	return d, nil
}

func (f *Factory) note(name, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.versions == nil {
		f.versions = map[string]string{}
	}
	f.versions[name] = version
}

// Versions are the tools the drivers made so far have reported: the MCP server
// and Chrome. Chrome's is known once a driver has signed in.
func (f *Factory) Versions() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.versions)
}
