package devtools_test

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
)

// The scene as a browser sees it: the scripts and tools the page answers, and the
// probes set to work in it. The scene itself, and what the page does when it is
// clicked and typed at, is in scene_test.go.

// --- the scripts -----------------------------------------------------------------------

// scriptArg is the value a script was given: what stands where the script has
// its placeholder.
func scriptArg(name, function string, out any) error {
	before, after, _ := strings.Cut(devtools.ScriptForTest(name), "__ARG__")
	raw, ok := strings.CutPrefix(function, before)
	if !ok {
		return fmt.Errorf("the function is not the %s script", name)
	}
	return json.Unmarshal([]byte(strings.TrimSuffix(raw, after)), out)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *scene) conversationID() *string { return nullable(s.conv) }

func (s *scene) lastReply() any {
	m := s.newest()
	if m == nil {
		return map[string]any{"found": false, "conversation": s.conversationID()}
	}
	m.look()
	r := map[string]any{
		"found": true, "conversation": s.conversationID(), "id": m.id, "status": m.status, "chars": m.chars,
	}
	if m.status == "failed" && m.failure != "" {
		r["error"] = m.failure
	}
	if m.terminal() && m.chars > 0 {
		r["rawUrl"] = fmt.Sprintf("/c/%s/m/%s/raw", s.conv, m.id)
	}
	return r
}

func (s *scene) replies() any {
	if m := s.newest(); m != nil {
		m.look()
	}
	type shown struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Chars  int    `json:"chars"`
		Text   string `json:"text"`
	}
	assistants := []shown{}
	for _, m := range s.assistants() {
		assistants = append(assistants, shown{m.id, m.status, m.chars, m.visible()})
	}
	users := 0
	if c := s.open(); c != nil {
		for _, m := range c.messages {
			if m.role == "user" {
				users++
			}
		}
	}
	regenerate, retry := s.offers()
	return map[string]any{"assistants": assistants, "users": users, "regenerate": regenerate, "retry": retry}
}

func (s *scene) fetchRaw(function string) any {
	var address string
	if err := scriptArg("fetchRaw", function, &address); err != nil {
		return map[string]any{"status": 400, "text": err.Error()}
	}
	for _, c := range s.saved {
		for _, m := range c.messages {
			if address == fmt.Sprintf("/c/%s/m/%s/raw", c.id, m.id) {
				return map[string]any{"status": 200, "text": m.raw()}
			}
		}
	}
	return map[string]any{"status": 404, "text": "not found"}
}

// rowButton says where a row's button is, as the script of that name does. A row
// whose title is being typed has neither of its buttons.
func (s *scene) rowButton(function string) any {
	var arg struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	if err := scriptArg("rowButton", function, &arg); err != nil || arg.ID == s.renaming {
		return nil
	}
	verb := map[string]string{"rename": "Rename", "delete": "Delete"}[arg.Kind]
	var name string
	before := 0
	for _, r := range s.rows {
		if r.id == arg.ID {
			name = verb + " conversation " + r.title
			break
		}
	}
	if name == "" {
		return nil
	}
	for _, r := range s.rows {
		if r.id == arg.ID {
			break
		}
		if verb+" conversation "+r.title == name {
			before++
		}
	}
	return map[string]any{"name": name, "before": before}
}

func (s *scene) rowFor(function, script string) (sceneRow, bool) {
	var id string
	if err := scriptArg(script, function, &id); err != nil {
		return sceneRow{}, false
	}
	for _, r := range s.rows {
		if r.id == id {
			return r, true
		}
	}
	return sceneRow{}, false
}

func (s *scene) layout() any {
	width, overflow := 1280, 0.0
	if s.phone() {
		width, overflow = 375, s.flaws.overflowPx
	}
	return map[string]any{
		"overflow": overflow, "width": width, "composerVisible": !s.flaws.composerOffScreen,
		"toggle": s.phone() && !s.flaws.noMenuButton,
	}
}

func (s *scene) focusNow() any {
	f := s.focused()
	return map[string]any{"testid": nullable(f.testid), "name": f.testid, "href": nullable(f.href)}
}

// --- the page ---------------------------------------------------------------------------

const quietNetwork = "## Network requests\nShowing 1-1 of 1 (Page 1 of 1).\nreqid=1 GET http://web.test:8300/ [200]"

// page connects the scene to the tools of a browser. Tools that are not about
// the page itself (the audits, the trace) are left to the test.
func (s *scene) page() *page {
	p := newPage()
	locked := func(f func(map[string]any) any) func(map[string]any) (any, error) {
		return func(args map[string]any) (any, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			return f(args), nil
		}
	}
	function := func(args map[string]any) string { fn, _ := args["function"].(string); return fn }
	tool := func(f func(map[string]any) error) func(map[string]any) (*mcp.CallToolResult, error) {
		return func(args map[string]any) (*mcp.CallToolResult, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if err := f(args); err != nil {
				return errorResult("Error: " + err.Error()), nil
			}
			return textResult("Done"), nil
		}
	}
	text := func(args map[string]any, key string) string { v, _ := args[key].(string); return v }

	p.scripts["pageState"] = locked(func(map[string]any) any {
		return state("1", func(m map[string]any) { m["path"], m["login"], m["composer"] = s.path, false, true })
	})
	p.scripts["lastReply"] = locked(func(map[string]any) any { return s.lastReply() })
	p.scripts["replies"] = locked(func(map[string]any) any { return s.replies() })
	p.scripts["collect"] = locked(func(map[string]any) any { return s.observed })
	p.scripts["layout"] = locked(func(map[string]any) any { return s.layout() })
	p.scripts["theme"] = locked(func(map[string]any) any { return map[string]any{"mode": s.mode, "applied": s.mode} })
	p.scripts["focus"] = locked(func(map[string]any) any { return s.focusNow() })
	p.scripts["sidebarShown"] = locked(func(map[string]any) any { return s.menuOpen || !s.phone() })
	p.scripts["fetchRaw"] = locked(func(args map[string]any) any { return s.fetchRaw(function(args)) })
	p.scripts["rowButton"] = locked(func(args map[string]any) any { return s.rowButton(function(args)) })
	p.scripts["rowExists"] = locked(func(args map[string]any) any { _, ok := s.rowFor(function(args), "rowExists"); return ok })
	p.scripts["rowTitle"] = locked(func(args map[string]any) any {
		if r, ok := s.rowFor(function(args), "rowTitle"); ok && r.id != s.renaming {
			return r.title
		}
		return nil
	})

	p.tools["navigate_page"] = tool(func(args map[string]any) error {
		path := s.path
		if text(args, "type") == "url" {
			path = strings.TrimPrefix(text(args, "url"), baseURL)
		}
		if s.opened != nil {
			if err := s.opened(s, path); err != nil {
				return err
			}
		}
		s.load(path)
		if text(args, "type") == "reload" && s.afterReload != nil {
			s.afterReload(s)
		}
		return nil
	})
	p.tools["take_snapshot"] = func(map[string]any) (*mcp.CallToolResult, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.beforeSnapshot != nil {
			s.beforeSnapshot(s)
		}
		return textResult(s.snapshot()), nil
	}
	p.tools["click"] = tool(func(args map[string]any) error { return s.click(text(args, "uid")) })
	p.tools["fill"] = tool(func(args map[string]any) error { return s.fill(text(args, "uid"), text(args, "value")) })
	p.tools["press_key"] = tool(func(args map[string]any) error { s.press(text(args, "key")); return nil })
	p.tools["type_text"] = tool(func(args map[string]any) error { s.typeText(text(args, "text"), text(args, "submitKey")); return nil })
	p.tools["emulate"] = tool(func(args map[string]any) error {
		s.viewport = text(args, "viewport")
		s.viewports = append(s.viewports, s.viewport)
		return nil
	})
	p.tools["list_console_messages"] = ok(emptyConsole)
	p.tools["list_network_requests"] = ok(quietNetwork)
	return p
}

// --- the rig -----------------------------------------------------------------------------

// rig is probes working in a scene.
type rig struct {
	scene  *scene
	page   *page
	probes *devtools.Probes
}

// newRig puts probes in a tab of the scene. They leave the conversations they
// start, as a person looking at the result would want, unless a test says not.
func newRig(t *testing.T, s *scene) *rig {
	t.Helper()
	p := s.page()
	probes := devtools.NewProbesForTest(p.driver(t, devtools.DriverConfig{}))
	probes.Prompts = devtools.ProbePrompts{Long: longPrompt, Short: shortPrompt, Flaky: flakyPrompt}
	probes.Thresholds = domain.DefaultThresholds()
	probes.KeepChats = true
	return &rig{scene: s, page: p, probes: probes}
}

// lighthouseText is what the audit tool prints for a page.
func lighthouseText(accessibility, bestPractices int) string {
	return fmt.Sprintf("## Lighthouse Audit Results\nMode: navigation\n### Category Scores\n- Accessibility: %d (accessibility)\n- Best Practices: %d (best-practices)\n- SEO: 90 (seo)\n### Audit Summary\nPassed: 40\nFailed: 1\n", accessibility, bestPractices)
}

// traceText is what stopping a performance trace prints.
func traceText(cls string) string {
	return "The performance trace has been stopped.\n## insight set id: NAVIGATION_0\nMetrics (lab / observed):\n  - LCP: 120 ms\n  - CLS: " + cls + "\nMetrics (field / real users): n/a – no data\n"
}

var errAudit = errors.New("the audit failed")
