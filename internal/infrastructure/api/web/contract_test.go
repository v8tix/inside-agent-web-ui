package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/v8tix/inside-agent-web-ui/internal/contract"
	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service/mocks"
)

const contractTitle = "Quarterly plan"

// document parses a whole page, keeping <html>, <head> and <body> (parseHTML
// parses a fragment and drops them).
func document(t *testing.T, page string) *html.Node {
	t.Helper()
	root, err := html.Parse(strings.NewReader(page))
	require.NoError(t, err)
	return root
}

// pageFor renders one of the contract's page states through the real routes
// and templates, and returns the page.
func pageFor(t *testing.T, state string) *html.Node {
	t.Helper()
	switch state {
	case contract.Login:
		rec := serve(t, mocks.NewAuth(t), Options{}, httptest.NewRequest(http.MethodGet, "/login", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		return document(t, rec.Body.String())
	case contract.RenameForm, contract.DeleteDialog:
		h := newChatHarness(t)
		h.convs.On("Find", mock.Anything, alice.ID, convID).Return(titled(convID, contractTitle), nil)
		path := "/c/" + convID.String() + "/rename"
		if state == contract.DeleteDialog {
			path = "/c/" + convID.String() + "/delete"
		}
		rec := h.do(http.MethodGet, path, nil, htmx)
		require.Equal(t, http.StatusOK, rec.Code)
		return parseHTML(t, rec.Body.String())
	}

	other := uuid.NewV7()
	h := newChatHarness(t)
	h.expectSidebar(titled(convID, contractTitle), titled(other, "An older chat"))
	if state == contract.Empty {
		rec := h.do(http.MethodGet, "/", nil, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		return document(t, rec.Body.String())
	}

	known := domain.Meta{TTFTMs: 120, DurationMs: 3400, Usage: domain.Usage{Prompt: 10, Completion: 25}, ToolCalls: []string{"search", "calc"}}
	user := func(seq int) domain.Message { return msg(seq, domain.RoleUser, domain.StatusComplete, "a question") }
	reply := func(seq int, status domain.Status, content string) domain.Message {
		return msg(seq, domain.RoleAssistant, status, content)
	}
	var messages []domain.Message
	switch state {
	case contract.Finished:
		done := reply(2, domain.StatusComplete, "It is **4**.")
		done.Meta = known
		messages = []domain.Message{user(1), done}
	case contract.Failed:
		failed := reply(2, domain.StatusFailed, "par")
		failed.Error = "The agent took too long to answer."
		messages = []domain.Message{user(1), failed}
	case contract.Statuses:
		done := reply(2, domain.StatusComplete, "complete")
		done.Meta = known
		failed := reply(6, domain.StatusFailed, "")
		failed.Error = "The agent can't be reached right now. Try again in a moment."
		messages = []domain.Message{
			user(1), done,
			user(3), reply(4, domain.StatusStopped, "half an answer"),
			user(5), failed,
			user(7), reply(8, domain.StatusStreaming, "so far"),
			user(9), reply(10, domain.StatusPending, ""),
		}
	default:
		t.Fatalf("unknown page state %q", state)
	}
	h.convs.On("Get", mock.Anything, alice.ID, convID).Return(service.ConversationView{
		Conversation: conv(convID, contractTitle), Messages: messages,
	}, nil)
	rec := h.do(http.MethodGet, "/c/"+convID.String(), nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	return document(t, rec.Body.String())
}

// --- what the accessibility tree would call things ---------------------------

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// visibleText is the text of n, leaving out what is hidden from assistive
// technology (aria-hidden, hidden), such as decorative icons.
func visibleText(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.ElementNode && (attrOf(c, "aria-hidden") == "true" || hasAttr(c, "hidden")) {
			return
		}
		if c.Type == html.TextNode {
			sb.WriteString(c.Data)
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	return sb.String()
}

func findByID(root *html.Node, id string) *html.Node {
	var found *html.Node
	walkHTML(root, func(n *html.Node) {
		if found == nil && n.Type == html.ElementNode && attrOf(n, "id") == id {
			found = n
		}
	})
	return found
}

// accessibleName follows the parts of the accessible name computation this
// page uses: aria-labelledby, aria-label, an associated <label>, then content.
func accessibleName(root, n *html.Node) string {
	if ids := attrOf(n, "aria-labelledby"); ids != "" {
		var parts []string
		for id := range strings.FieldsSeq(ids) {
			if ref := findByID(root, id); ref != nil {
				parts = append(parts, collapse(visibleText(ref)))
			}
		}
		return strings.Join(parts, " ")
	}
	if v := attrOf(n, "aria-label"); v != "" {
		return collapse(v)
	}
	switch n.Data {
	case "input", "textarea", "select":
		var name string
		walkHTML(root, func(l *html.Node) {
			if l.Type == html.ElementNode && l.Data == "label" && attrOf(l, "for") == attrOf(n, "id") && attrOf(n, "id") != "" {
				name = collapse(visibleText(l))
			}
		})
		return name // a placeholder is not a name
	}
	return collapse(visibleText(n))
}

// rowTitle is the title of the sidebar row n belongs to.
func rowTitle(n *html.Node) string {
	for row := n; row != nil; row = row.Parent {
		if row.Type == html.ElementNode && attrOf(row, "data-testid") == "conversation" {
			if link := byTestID(row, "conversation-link"); len(link) > 0 {
				return collapse(textContent(link[0]))
			}
		}
	}
	return ""
}

// checkContract returns what is wrong with a rendered page: every hook the
// contract requires in this state that is missing or does not look as promised.
//
// Pattern: Checker That Checks Itself
func checkContract(root *html.Node, state string) []string {
	var problems []string
	complain := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if state != contract.RenameForm && state != contract.DeleteDialog { // fragments have no <body>
		bodies := elementsOf(root, "body")
		switch {
		case len(bodies) != 1:
			complain("%s: expected one <body>, found %d", state, len(bodies))
		case attrOf(bodies[0], "data-ui-contract") == "":
			complain("%s: <body> has no data-ui-contract", state)
		case attrOf(bodies[0], "data-ui-contract") != contract.Version:
			complain("%s: data-ui-contract is %q, the contract is version %q", state, attrOf(bodies[0], "data-ui-contract"), contract.Version)
		}
	}

	for _, hook := range contract.Hooks {
		if !slices.Contains(hook.States, state) {
			continue
		}
		elements := byTestID(root, hook.TestID)
		if len(elements) == 0 {
			complain("%s: %s is missing", state, hook.TestID)
			continue
		}
		for _, el := range elements {
			if hook.Tag != "" && el.Data != hook.Tag {
				complain("%s: %s is a <%s>, the contract says <%s>", state, hook.TestID, el.Data, hook.Tag)
			}
			for _, attr := range hook.Attrs {
				if !hasAttr(el, attr) {
					complain("%s: %s has no %s", state, hook.TestID, attr)
				}
			}
			if hook.Name == "" {
				continue
			}
			want := strings.ReplaceAll(hook.Name, "{title}", rowTitle(el))
			if strings.Contains(hook.Name, "{title}") && hook.TestID == "conversation-link" {
				want = collapse(textContent(el))
			}
			got := accessibleName(root, el)
			if hook.NamePrefix && !strings.HasPrefix(got, want) || !hook.NamePrefix && got != want {
				complain("%s: %s is named %q, the contract says %q", state, hook.TestID, got, want)
			}
		}
	}
	return problems
}

// --- the tests ---------------------------------------------------------------

// renderedStates are the page states that TestUIContract renders and checks. A
// hook is checked in these and nowhere else, so TestUIContract_TableIsSound
// requires every state that a hook names to be one of them.
var renderedStates = []string{
	contract.Login, contract.Empty, contract.Finished, contract.Failed, contract.Statuses,
	contract.RenameForm, contract.DeleteDialog,
}

func TestUIContract(t *testing.T) {
	for _, state := range renderedStates {
		t.Run(state, func(t *testing.T) {
			assert.Empty(t, checkContract(pageFor(t, state), state))
		})
	}
}

func TestUIContract_MessagesCarryTheirState(t *testing.T) {
	root := pageFor(t, contract.Statuses)

	t.Run("every status appears, spelled as the contract says", func(t *testing.T) {
		var seen []string
		for _, m := range byTestID(root, "message") {
			status := attrOf(m, "data-status")
			require.Contains(t, contract.MessageStatuses, status)
			if attrOf(m, "data-role") == "assistant" {
				seen = append(seen, status)
			}
		}
		assert.ElementsMatch(t, contract.MessageStatuses, seen)
	})

	t.Run("a message says who wrote it and what it is", func(t *testing.T) {
		for _, m := range byTestID(root, "message") {
			assert.Contains(t, []string{"user", "assistant"}, attrOf(m, "data-role"))
			_, err := uuid.Parse(attrOf(m, "data-id"))
			assert.NoError(t, err, "data-id is the message's id")
			assert.NotEmpty(t, byTestID(m, "message-body"), "where the text lands")
		}
	})

	t.Run("the telemetry is there once a reply is finished and known, and only then", func(t *testing.T) {
		for _, m := range byTestID(root, "message") {
			var have []string
			for _, attr := range contract.Telemetry {
				if hasAttr(m, attr) {
					have = append(have, attr)
				}
			}
			switch {
			case attrOf(m, "data-role") == "assistant" && attrOf(m, "data-status") == "complete":
				assert.ElementsMatch(t, contract.Telemetry, have, "a finished reply with known values has them all")
				assert.Equal(t, "120", attrOf(m, "data-ttft-ms"))
				assert.Equal(t, "3400", attrOf(m, "data-duration-ms"))
				assert.Equal(t, "10", attrOf(m, "data-prompt-tokens"))
				assert.Equal(t, "25", attrOf(m, "data-completion-tokens"))
				assert.Equal(t, "search,calc", attrOf(m, "data-tool-calls"))
			default:
				assert.Empty(t, have, "unknown means absent, not zero or empty (%s %s)", attrOf(m, "data-role"), attrOf(m, "data-status"))
			}
		}
	})

	t.Run("an error bubble is an alert with text meant for people", func(t *testing.T) {
		errs := byTestID(root, "error")
		require.NotEmpty(t, errs)
		for _, e := range errs {
			assert.Equal(t, "alert", attrOf(e, "role"))
			text := textContent(e)
			assert.NotContains(t, text, "Error:", "no stack or error type")
			assert.NotContains(t, text, "ECONN")
			assert.NotEmpty(t, strings.TrimSpace(text))
		}
	})
}

func TestUIContract_ComposerAtRest(t *testing.T) {
	for _, state := range []string{contract.Empty, contract.Finished} {
		t.Run(state, func(t *testing.T) {
			root := pageFor(t, state)

			assert.False(t, hasAttr(byTestID(root, "send")[0], "disabled"), "Send is on when nothing is being written")
			assert.True(t, hasAttr(byTestID(root, "stop")[0], "hidden"), "Stop is only there while a reply is being written")
		})
	}
}

func TestUIContract_Actions(t *testing.T) {
	t.Run("regenerate on a finished reply, retry on a failed one, never both", func(t *testing.T) {
		finished, failed := pageFor(t, contract.Finished), pageFor(t, contract.Failed)

		assert.Len(t, byTestID(finished, "regenerate"), 1)
		assert.Empty(t, byTestID(finished, "retry"))
		assert.Len(t, byTestID(failed, "retry"), 1)
		assert.Empty(t, byTestID(failed, "regenerate"))
	})
}

// --- the check checks itself -------------------------------------------------

func removeAttr(n *html.Node, key string) {
	n.Attr = slices.DeleteFunc(n.Attr, func(a html.Attribute) bool { return a.Key == key })
}

func setAttr(n *html.Node, key, value string) {
	removeAttr(n, key)
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: value})
}

func TestUIContract_NoticesDamage(t *testing.T) {
	tests := []struct {
		name        string
		state       string
		damage      func(t *testing.T, root *html.Node)
		wantProblem string
	}{
		{"a hook is removed", contract.Finished, func(t *testing.T, root *html.Node) {
			removeAttr(byTestID(root, "send")[0], "data-testid")
		}, "send is missing"},
		{"a button is renamed", contract.Finished, func(t *testing.T, root *html.Node) {
			setAttr(byTestID(root, "stop")[0], "aria-label", "Halt")
		}, `stop is named "Halt"`},
		{"the composer loses its label", contract.Finished, func(t *testing.T, root *html.Node) {
			walkHTML(root, func(n *html.Node) {
				if n.Type == html.ElementNode && n.Data == "label" && attrOf(n, "for") == "composer-input" {
					setAttr(n, "for", "somewhere-else")
				}
			})
		}, "composer-input is named"},
		{"a message loses its id", contract.Finished, func(t *testing.T, root *html.Node) {
			removeAttr(byTestID(root, "message")[0], "data-id")
		}, "message has no data-id"},
		{"a row loses its id", contract.Empty, func(t *testing.T, root *html.Node) {
			removeAttr(byTestID(root, "conversation")[0], "data-id")
		}, "conversation has no data-id"},
		{"the contract version moves on", contract.Finished, func(t *testing.T, root *html.Node) {
			setAttr(elementsOf(root, "body")[0], "data-ui-contract", "2")
		}, `data-ui-contract is "2"`},
		{"the version marker is dropped", contract.Login, func(t *testing.T, root *html.Node) {
			removeAttr(elementsOf(root, "body")[0], "data-ui-contract")
		}, "<body> has no data-ui-contract"},
		{"a button becomes something else", contract.Finished, func(t *testing.T, root *html.Node) {
			byTestID(root, "regenerate")[0].Data = "div"
		}, "regenerate is a <div>"},
		{"a rename button forgets whose it is", contract.Empty, func(t *testing.T, root *html.Node) {
			setAttr(byTestID(root, "rename")[0], "aria-label", "Rename conversation")
		}, "rename is named"},
		{"the dialog loses its name", contract.DeleteDialog, func(t *testing.T, root *html.Node) {
			removeAttr(byTestID(root, "delete-dialog")[0], "aria-labelledby")
		}, "delete-dialog has no aria-labelledby"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := pageFor(t, tc.state)
			require.Empty(t, checkContract(root, tc.state), "the page is fine before the damage")

			tc.damage(t, root)

			problems := checkContract(root, tc.state)
			require.NotEmpty(t, problems, "the damage went unnoticed")
			assert.Contains(t, strings.Join(problems, "\n"), tc.wantProblem)
		})
	}
}

func TestUIContract_TableIsSound(t *testing.T) {
	seen := map[string]bool{}
	for _, hook := range contract.Hooks {
		assert.NotEmpty(t, hook.TestID)
		assert.NotEmpty(t, hook.States, "%s: a hook nobody renders is not tested", hook.TestID)
		for _, state := range hook.States {
			assert.Contains(t, renderedStates, state, "%s is required in a state that no test renders", hook.TestID)
		}
		assert.False(t, seen[hook.TestID], "%s is listed twice", hook.TestID)
		seen[hook.TestID] = true
	}
	_, err := strconv.Atoi(contract.Version)
	assert.NoError(t, err, "the version is a number")
}
