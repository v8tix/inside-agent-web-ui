package devtools_test

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
)

// A scene is a pretend Web UI behind the tools of a browser: the server's
// conversations, the page that shows one of them, the sidebar, the theme and the
// keyboard focus. A probe acts on it with clicks and keys, and is judged by what
// it makes of what it sees, so the scene has to behave as the page does and, in
// the tests of a failure, as the page does not (see flaws).
//
// Time does not pass in a scene; looking does. Each look at the newest reply
// moves it on by one stage, the last stage staying for ever, which is what a
// reply being written looks like to something that keeps looking. The tests run
// in a bubble of virtual time, so the waits between looks cost nothing.

// The prompts the probes are given, and what the pretend agent does with each.
const (
	longPrompt  = "write a long essay"
	shortPrompt = "say hello"
	flakyPrompt = "answer on the second try"
)

// sceneStage is what a reply looks like at one look.
type sceneStage struct {
	status string
	chars  int
}

var (
	// quickly is a reply that is over in three looks.
	quickly = []sceneStage{{"pending", 0}, {"streaming", 6}, {"complete", 12}}
	// slowly is a reply that takes longer than any probe needs to press Stop or
	// to reload the page.
	slowly = []sceneStage{{"pending", 0}, {"streaming", 40}, {"streaming", 90}, {"complete", 230}}
	// failing is a reply the agent could not give.
	failing = []sceneStage{{"pending", 0}, {"failed", 0}}
	// endless is a reply that is never over.
	endless = []sceneStage{{"pending", 0}, {"streaming", 40}, {"streaming", 90}}
)

const failureText = "The agent could not answer."

// sceneReply is what the pretend agent gives to a prompt.
type sceneReply struct {
	stages  []sceneStage
	failure string // what a failed reply tells the reader
}

// defaultAgent answers the long prompt slowly, fails the flaky one the first
// time it is asked, and answers everything else quickly.
func defaultAgent(prompt string, attempt int) sceneReply {
	switch {
	case strings.HasPrefix(prompt, longPrompt):
		return sceneReply{stages: slowly}
	case strings.HasPrefix(prompt, flakyPrompt) && attempt == 1:
		return sceneReply{stages: failing, failure: failureText}
	}
	return sceneReply{stages: quickly}
}

// flaws are the ways a page can be wrong; the zero value is a page that is right.
type flaws struct {
	// Stop (P-01, P-08)
	stopHidden       bool // there is no Stop button
	stopNotFocusable bool // the Stop button cannot be reached with Tab
	stopIgnored      bool // pressing Stop changes nothing

	// Regenerate (P-02)
	regenerateAdds       bool // a new reply is added instead of replacing the last
	regenerateEverywhere bool // every reply offers Regenerate, not only the newest
	noRegenerate         bool // no reply offers it
	regenerateIgnored    bool // pressing it changes nothing
	forgetsEarlier       bool // a message drops the replies before it

	// Rename and delete (P-05)
	renameNotShown bool // the sidebar keeps the old title (the server saves the new one)
	renameLost     bool // the sidebar shows the new title (the server keeps the old one)
	deleteNotShown bool // the row stays in the sidebar (the server deletes the conversation)
	deleteLost     bool // the row goes (the server keeps the conversation)

	// Theme (P-06)
	themeStuck     bool // the switch does nothing
	themeForgotten bool // the choice is not kept across a load

	// Phone (P-04)
	overflowPx        float64 // how far the page spills sideways on a phone
	noMenuButton      bool    // nothing opens the conversations on a phone
	menuStaysClosed   bool    // the button does not open them
	composerOffScreen bool    // the message field is out of the window

	// Keyboard (P-08)
	composerNotFocused bool // the focus is on the theme switch when the page opens
	linkInert          bool // Enter on a conversation does not open it
	linkWithoutHref    bool // the conversations are not links

	// Failed replies (P-10)
	noRetry bool // a failed reply offers no Retry
}

type sceneMessage struct {
	id      string
	role    string // "user" or "assistant"
	text    string // what the user said, for a user message
	status  string
	chars   int
	failure string
	stages  []sceneStage
	stored  *string // what the server keeps, when it is not what is shown
}

// look shows the stage the reply is at and moves it on.
func (m *sceneMessage) look() {
	st := m.stages[0]
	m.status, m.chars = st.status, st.chars
	if len(m.stages) > 1 {
		m.stages = m.stages[1:]
	}
}

func (m *sceneMessage) terminal() bool { return domain.ReplyStatus(m.status).Terminal() }

func (m *sceneMessage) visible() string { return strings.Repeat("x", m.chars) }

// raw is the text the server keeps, which the Copy button's address gives.
func (m *sceneMessage) raw() string {
	if m.stored != nil {
		return *m.stored
	}
	return m.visible()
}

type sceneConversation struct {
	id       string
	title    string
	rank     int // the sidebar lists the highest first
	messages []*sceneMessage
}

type sceneRow struct{ id, title string }

type scene struct {
	mu sync.Mutex

	agent    func(prompt string, attempt int) sceneReply
	attempts map[string]int // how many times each prompt was asked, retries and regenerations included
	flaws    flaws

	// Hooks for what only one test needs.
	afterReload     func(*scene)               // runs when the page is loaded again
	afterRegenerate func(*scene)               // runs when a new reply was asked for
	beforeSnapshot  func(*scene)               // runs when the page is about to be looked at for its elements
	keepOnStop      func(shown int) string     // what the server keeps of a reply when it is stopped
	observed        any                        // what the observer says it recorded
	opened          func(*scene, string) error // runs when a path is opened, to fail it

	// The server.
	saved    []*sceneConversation
	theme    string // the choice kept in the browser's storage
	nextConv int
	nextMsg  int
	nextRank int

	// The page.
	path       string
	conv       string // the open conversation; "" on the start page
	rows       []sceneRow
	composer   string
	focus      string // the key of the element that has the keyboard focus
	renaming   string // the conversation whose title is being typed
	titleInput string
	deleting   string // the conversation whose deletion waits for a yes
	mode       string // data-theme-mode
	menuOpen   bool   // the conversation list is slid open
	viewport   string

	viewports []string // every window size set, in order
}

func newScene(mutate ...func(*scene)) *scene {
	s := &scene{agent: defaultAgent, attempts: map[string]int{}, theme: "auto", viewport: devtools.DefaultViewport}
	for _, f := range mutate {
		f(s)
	}
	s.load("/")
	return s
}

// withFlaws is a mutation for newScene.
func withFlaws(f flaws) func(*scene) { return func(s *scene) { s.flaws = f } }

// seed adds conversations the server already had, which show above any the
// probes start: other people's work that was touched more recently.
func (s *scene) seed(title string, n int) {
	for range n {
		s.nextConv++
		s.saved = append(s.saved, &sceneConversation{
			id: fmt.Sprintf("seed-%d", s.nextConv), title: title, rank: 1000 + s.nextConv,
			messages: []*sceneMessage{{id: fmt.Sprintf("seed-m-%d", s.nextConv), role: "assistant", status: "complete", chars: 5, stages: []sceneStage{{"complete", 5}}}},
		})
	}
	s.load(s.path)
}

func (s *scene) conversation(id string) *sceneConversation {
	for _, c := range s.saved {
		if c.id == id {
			return c
		}
	}
	return nil
}

// titles lists what the server has, by conversation id.
func (s *scene) titles() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, c := range s.saved {
		out[c.id] = c.title
	}
	return out
}

func (s *scene) phone() bool { return strings.HasPrefix(s.viewport, "375") }

// load opens a path, as a navigation or a reload does: the page starts again
// from what the server has.
func (s *scene) load(path string) {
	s.path, s.conv = path, ""
	if id, ok := strings.CutPrefix(path, "/c/"); ok {
		s.conv = id
	}
	s.rows = s.rowsFromServer()
	s.composer, s.titleInput, s.renaming, s.deleting = "", "", "", ""
	s.menuOpen = false
	s.mode = s.theme
	s.focus = "composer-input"
	if s.flaws.composerNotFocused {
		s.focus = "theme" // the first thing on the page, as when nothing asks for the focus
	}
}

func (s *scene) rowsFromServer() []sceneRow {
	byRank := slices.Clone(s.saved)
	sort.SliceStable(byRank, func(i, j int) bool { return byRank[i].rank > byRank[j].rank })
	rows := make([]sceneRow, 0, len(byRank))
	for _, c := range byRank {
		rows = append(rows, sceneRow{id: c.id, title: c.title})
	}
	return rows
}

func (s *scene) open() *sceneConversation { return s.conversation(s.conv) }

func (s *scene) assistants() []*sceneMessage {
	c := s.open()
	if c == nil {
		return nil
	}
	var out []*sceneMessage
	for _, m := range c.messages {
		if m.role == "assistant" {
			out = append(out, m)
		}
	}
	return out
}

func (s *scene) newest() *sceneMessage {
	if a := s.assistants(); len(a) > 0 {
		return a[len(a)-1]
	}
	return nil
}

// replyTo makes the assistant message that answers a prompt.
func (s *scene) replyTo(prompt string) *sceneMessage {
	s.attempts[prompt]++
	r := s.agent(prompt, s.attempts[prompt])
	s.nextMsg++
	m := &sceneMessage{id: fmt.Sprintf("m-%d", s.nextMsg), role: "assistant", failure: r.failure, stages: slices.Clone(r.stages)}
	m.status, m.chars = m.stages[0].status, m.stages[0].chars
	return m
}

func titleFrom(prompt string) string {
	r := []rune(prompt)
	return string(r[:min(len(r), 30)])
}

// send sends the text of the composer; the first message of a conversation
// starts it.
func (s *scene) send(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	s.composer = ""
	c := s.open()
	if c == nil {
		s.nextConv++
		s.nextRank++
		c = &sceneConversation{id: fmt.Sprintf("c-%d", s.nextConv), title: titleFrom(text), rank: s.nextRank}
		s.saved = append(s.saved, c)
		s.rows = append([]sceneRow{{id: c.id, title: c.title}}, s.rows...)
		s.conv, s.path = c.id, "/c/"+c.id
	}
	if s.flaws.forgetsEarlier {
		c.messages = nil
	}
	s.nextMsg++
	c.messages = append(c.messages, &sceneMessage{id: fmt.Sprintf("u-%d", s.nextMsg), role: "user", text: text, status: "complete", chars: len(text)})
	c.messages = append(c.messages, s.replyTo(text))
}

// askAgain asks again for the newest reply, as Regenerate and Retry do.
func (s *scene) askAgain() {
	c := s.open()
	if c == nil || len(c.messages) < 2 {
		return
	}
	prompt := c.messages[len(c.messages)-2].text
	fresh := s.replyTo(prompt)
	if s.flaws.regenerateAdds {
		c.messages = append(c.messages, fresh)
	} else {
		c.messages[len(c.messages)-1] = fresh
	}
	if s.afterRegenerate != nil {
		s.afterRegenerate(s)
	}
}

func (s *scene) stop() {
	m := s.newest()
	if m == nil || m.terminal() || s.flaws.stopIgnored {
		return
	}
	m.status, m.stages = "stopped", []sceneStage{{"stopped", m.chars}}
	if s.keepOnStop != nil {
		kept := s.keepOnStop(m.chars)
		m.stored = &kept
	}
}

// offers counts the Regenerate and Retry buttons a reader can see.
func (s *scene) offers() (regenerate, retry int) {
	all := s.assistants()
	for i, m := range all {
		if !m.terminal() {
			continue
		}
		latest := i == len(all)-1
		switch {
		case m.status == "failed":
			if latest && !s.flaws.noRetry {
				retry++
			}
		case (latest || s.flaws.regenerateEverywhere) && !s.flaws.noRegenerate:
			regenerate++
		}
	}
	return regenerate, retry
}

// --- the keyboard ---------------------------------------------------------------------

type focusable struct{ key, testid, href string }

// ring is what Tab goes through, in the order of the page.
func (s *scene) ring() []focusable {
	ring := []focusable{{key: "new-chat", testid: "new-chat", href: "/"}, {key: "theme", testid: "theme-toggle"}}
	if s.phone() && !s.flaws.noMenuButton {
		ring = append([]focusable{{key: "menu", testid: "sidebar-toggle"}}, ring...)
	}
	for _, r := range s.rows {
		if r.id == s.renaming {
			continue // the row is a title field for now, without its link and buttons
		}
		link := focusable{key: "link:" + r.id, testid: "conversation-link", href: "/c/" + r.id}
		if s.flaws.linkWithoutHref {
			link.href = ""
		}
		ring = append(ring, link, focusable{key: "rename:" + r.id, testid: "rename"}, focusable{key: "delete:" + r.id, testid: "delete"})
	}
	ring = append(ring, focusable{key: "composer-input", testid: "composer-input"}, focusable{key: "send", testid: "send"})
	if m := s.newest(); m != nil && !m.terminal() && !s.flaws.stopHidden && !s.flaws.stopNotFocusable {
		ring = append(ring, focusable{key: "stop", testid: "stop"}) // after Send, as in the page
	}
	return ring
}

// move puts the focus on the next element, or the previous one. An element that
// has gone leaves the focus nowhere, and Tab then starts from the top of the
// page, Shift+Tab from the bottom.
func (s *scene) move(by int) {
	ring := s.ring()
	i := slices.IndexFunc(ring, func(f focusable) bool { return f.key == s.focus })
	switch {
	case i < 0 && by > 0:
		i = 0
	case i < 0:
		i = len(ring) - 1
	default:
		i = (i + by + len(ring)) % len(ring)
	}
	s.focus = ring[i].key
}

func (s *scene) focused() focusable {
	for _, f := range s.ring() {
		if f.key == s.focus {
			return f
		}
	}
	return focusable{}
}

// enter does what Enter does to the element that has the focus.
func (s *scene) enter() {
	switch f := s.focused(); f.testid {
	case "composer-input", "send":
		s.send(s.composer)
	case "stop":
		s.stop()
	case "conversation-link":
		if !s.flaws.linkInert {
			s.load(f.href)
		}
	case "theme-toggle":
		s.cycleTheme()
	}
}

// --- the tools -------------------------------------------------------------------------

func (s *scene) cycleTheme() {
	if s.flaws.themeStuck {
		return
	}
	next := map[string]string{"auto": "light", "light": "dark", "dark": "auto"}
	s.mode = next[s.mode]
	if !s.flaws.themeForgotten {
		s.theme = s.mode
	}
}

func (s *scene) saveTitle() {
	id, title := s.renaming, s.titleInput
	s.renaming = ""
	if !s.flaws.renameNotShown {
		for i := range s.rows {
			if s.rows[i].id == id {
				s.rows[i].title = title
			}
		}
	}
	if c := s.conversation(id); c != nil && !s.flaws.renameLost {
		c.title = title
	}
}

func (s *scene) confirmDelete() {
	id := s.deleting
	s.deleting = ""
	if !s.flaws.deleteNotShown {
		s.rows = slices.DeleteFunc(s.rows, func(r sceneRow) bool { return r.id == id })
	}
	if !s.flaws.deleteLost {
		s.saved = slices.DeleteFunc(s.saved, func(c *sceneConversation) bool { return c.id == id })
	}
	if s.conv == id {
		s.conv, s.path = "", "/"
	}
}

func (s *scene) click(uid string) error {
	kind, id, _ := strings.Cut(uid, ":")
	switch kind {
	case "stop":
		s.stop()
	case "regenerate", "retry":
		if kind != "regenerate" || !s.flaws.regenerateIgnored {
			s.askAgain()
		}
	case "theme":
		s.cycleTheme()
	case "menu":
		s.menuOpen = !s.flaws.menuStaysClosed
	case "rename":
		s.renaming, s.titleInput = id, ""
	case "delete":
		s.deleting = id
	case "save":
		s.saveTitle()
	case "cancel":
		s.renaming = ""
	case "confirm":
		s.confirmDelete()
	case "keep":
		s.deleting = ""
	case "composer":
		s.focus = "composer-input"
	case "send":
		s.send(s.composer)
	case "link":
		s.load("/c/" + id)
	case "new":
		s.load("/")
	default:
		return fmt.Errorf("element uid %q not found on page", uid)
	}
	return nil
}

func (s *scene) fill(uid, value string) error {
	switch uid {
	case "composer":
		s.composer, s.focus = value, "composer-input"
	case "title":
		s.titleInput = value
	default:
		return fmt.Errorf("element uid %q is not a field", uid)
	}
	return nil
}

func (s *scene) press(key string) {
	switch key {
	case "Tab":
		s.move(1)
	case "Shift+Tab":
		s.move(-1)
	case "Shift+Enter":
		if s.focus == "composer-input" {
			s.composer += "\n"
		}
	case "Enter":
		s.enter()
	}
}

func (s *scene) typeText(text, submit string) {
	if s.focus == "composer-input" {
		s.composer += text
	}
	if submit != "" {
		s.press(submit)
	}
}

func (s *scene) snapshot() string {
	var b strings.Builder
	node := func(depth int, uid, role, name string) {
		fmt.Fprintf(&b, "%suid=%s %s %q\n", strings.Repeat("  ", depth), uid, role, name)
	}
	b.WriteString("## Latest page snapshot\n")
	node(0, "root", "RootWebArea", "Agent Web UI")
	if s.phone() && !s.flaws.noMenuButton {
		node(1, "menu", "button", "Open conversations")
	}
	node(1, "theme", "button", "Theme: Auto (system). Switch to Light.")
	node(1, "new", "link", "New chat")
	for _, r := range s.rows {
		if r.id == s.renaming {
			continue // the row is a title field for now, without its link and buttons
		}
		node(2, "link:"+r.id, "link", r.title)
		node(2, "rename:"+r.id, "button", "Rename conversation "+r.title)
		node(2, "delete:"+r.id, "button", "Delete conversation "+r.title)
	}
	if s.renaming != "" {
		node(1, "title", "textbox", "Conversation title")
		node(1, "save", "button", "Save")
		node(1, "cancel", "button", "Cancel")
	}
	regenerate, retry := s.offers()
	for range regenerate {
		node(1, "regenerate", "button", "Regenerate response")
	}
	for range retry {
		node(1, "retry", "button", "Retry")
	}
	node(1, "composer", "textbox", "Message")
	node(1, "send", "button", "Send message")
	if m := s.newest(); m != nil && !m.terminal() && !s.flaws.stopHidden {
		node(1, "stop", "button", "Stop generating")
	}
	if s.deleting != "" {
		node(1, "dialog", "dialog", "Delete this conversation?")
		node(2, "keep", "button", "Cancel")
		node(2, "confirm", "button", "Delete")
	}
	return b.String()
}
