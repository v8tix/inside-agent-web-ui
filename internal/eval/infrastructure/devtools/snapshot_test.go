package devtools_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
)

// recordings is where the recorded tool outputs are: what the tools of Chrome
// DevTools MCP printed for a small page, one file for each.
var recordings = filepath.Join("..", "..", "testdata", "mcp")

// fixture returns a recorded tool output and whether the tool reported an error.
func fixture(t *testing.T, name string) (text string, isError bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(recordings, name+".txt"))
	require.NoError(t, err)
	text = string(data)
	if body, ok := strings.CutPrefix(text, "[isError]\n"); ok {
		return body, true
	}
	return text, false
}

// A recording is kept because a rule of the driver rests on what a tool prints,
// and a test of that rule reads it. One that no test reads backs nothing, and is
// deleted. Which are read is taken from the calls of fixture in this package's
// tests, so every call writes the name out.
func TestEveryRecordingIsRead(t *testing.T) {
	read := map[string]bool{}
	sources, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	for _, source := range sources {
		files := token.NewFileSet()
		parsed, err := parser.ParseFile(files, source, nil, 0)
		require.NoError(t, err)
		for n := range ast.Preorder(parsed) {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				continue
			}
			if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "fixture" {
				continue
			}
			name, ok := call.Args[1].(*ast.BasicLit)
			if !assert.True(t, ok && name.Kind == token.STRING, "%s: fixture is given a name that is not written out, so the recording it reads cannot be told", files.Position(call.Pos())) {
				continue
			}
			written, err := strconv.Unquote(name.Value)
			require.NoError(t, err)
			read[written+".txt"] = true
		}
	}
	require.Contains(t, read, "take_snapshot_chat.txt", "the calls of fixture were not found: the scan is looking in the wrong place")

	kept, err := os.ReadDir(recordings)
	require.NoError(t, err)
	require.NotEmpty(t, kept)
	for _, recording := range kept {
		assert.True(t, read[recording.Name()], "no test reads %s: test the rule that rests on it, or delete it", recording.Name())
	}
}

func chatSnapshot(t *testing.T) devtools.Snapshot {
	t.Helper()
	text, isErr := fixture(t, "take_snapshot_chat")
	require.False(t, isErr)
	return devtools.ParseSnapshot(text)
}

func TestParseSnapshot_RecordedPage(t *testing.T) {
	snap := chatSnapshot(t)

	require.Len(t, snap.Nodes, 11)
	assert.Equal(t, devtools.Node{
		UID: "1_0", Role: "RootWebArea", Name: "Fake chat", Attrs: map[string]string{"url": "http://127.0.0.1:51008/"},
	}, snap.Nodes[0])

	roles := make([]string, len(snap.Nodes))
	for i, n := range snap.Nodes {
		roles[i] = n.Role
	}
	assert.Equal(t, []string{"RootWebArea", "banner", "link", "StaticText", "main", "region", "StaticText", "StaticText", "form", "textbox", "button"}, roles)

	textbox := snap.Nodes[9]
	assert.Equal(t, "1_7", textbox.UID)
	assert.Equal(t, "Message", textbox.Name)
	assert.Equal(t, []string{"focusable", "focused", "multiline"}, textbox.Flags)
	assert.True(t, textbox.Has("focused"))
	assert.False(t, textbox.Has("disabled"))
	assert.Empty(t, snap.Nodes[1].Name, "a banner has no name")
	assert.Equal(t, "http://127.0.0.1:51008/#", snap.Nodes[2].Attrs["url"])
}

func TestParseSnapshot_Reading(t *testing.T) {
	tests := []struct {
		name string
		line string
		want devtools.Node
	}{
		{"escaped quotes and unicode", `uid=1_1 button "Say \"hi\" é\\"`, devtools.Node{UID: "1_1", Role: "button", Name: `Say "hi" é\`}},
		{"a value with spaces", `uid=3 link "x" url="http://a b/c d" level="2"`, devtools.Node{UID: "3", Role: "link", Name: "x", Attrs: map[string]string{"url": "http://a b/c d", "level": "2"}}},
		{"an attribute without quotes", `uid=4 heading "T" level=2 focusable`, devtools.Node{UID: "4", Role: "heading", Name: "T", Attrs: map[string]string{"level": "2"}, Flags: []string{"focusable"}}},
		{"no name, only flags", `uid=5 textbox focusable focused`, devtools.Node{UID: "5", Role: "textbox", Flags: []string{"focusable", "focused"}}},
		{"no name, no flags", `uid=6 main`, devtools.Node{UID: "6", Role: "main"}},
		{"a value attribute holding quotes", `uid=7 textbox "Message" value="He said \"no\""`, devtools.Node{UID: "7", Role: "textbox", Name: "Message", Attrs: map[string]string{"value": `He said "no"`}}},
		{"a quote that never closes", `uid=8 button "oops`, devtools.Node{UID: "8", Role: "button", Name: "oops"}},
		{"an empty name", `uid=9 button ""`, devtools.Node{UID: "9", Role: "button"}},
		{"windows line ends", "uid=10 button \"Go\"\r", devtools.Node{UID: "10", Role: "button", Name: "Go"}},
		{"indentation is ignored", `      uid=11 button "Deep"`, devtools.Node{UID: "11", Role: "button", Name: "Deep"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap := devtools.ParseSnapshot("## Latest page snapshot\n" + tc.line + "\n")

			require.Len(t, snap.Nodes, 1)
			assert.Equal(t, tc.want, snap.Nodes[0])
		})
	}
	t.Run("lines that are not elements are ignored", func(t *testing.T) {
		snap := devtools.ParseSnapshot("## Latest page snapshot\n\nsomething else\nuid=1 main\n...[truncated]\n")

		assert.Len(t, snap.Nodes, 1)
	})
	t.Run("an error message has no nodes", func(t *testing.T) {
		text, isErr := fixture(t, "take_snapshot_bad_page")
		require.True(t, isErr)

		assert.Empty(t, devtools.ParseSnapshot(text).Nodes)
	})
	t.Run("nothing at all", func(t *testing.T) {
		assert.Empty(t, devtools.ParseSnapshot("").Nodes)
	})
}

// A page that has loaded has at least a root. A snapshot with no element in it
// is not a page to look for anything in, and what is asked of it fails for that
// reason, not for want of the one element.
func TestSnapshot_APageWithoutElementsIsAnError(t *testing.T) {
	p := chatPage()
	p.tools["take_snapshot"] = ok("## Latest page snapshot\n")
	d := p.driver(t, devtools.DriverConfig{})
	tests := []struct {
		name string
		do   func(t *testing.T) error
	}{
		{"clicking", func(t *testing.T) error {
			return d.Click(t.Context(), devtools.Query{Role: "button", Name: "Send message"})
		}},
		{"filling a field", func(t *testing.T) error {
			return d.Fill(t.Context(), devtools.Query{Role: "textbox", Name: "Message"}, "hello")
		}},
		{"sending a message", func(t *testing.T) error { return d.Submit(t.Context(), "hello") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.do(t)

			require.ErrorContains(t, err, "the page snapshot has no elements")
			_, missing := errors.AsType[*devtools.NotFoundError](err)
			assert.False(t, missing, "it is not that this element is missing")
		})
	}
	assert.Empty(t, p.called("click"))
	assert.Empty(t, p.called("fill"))
}

// The start page as it was recorded, before a message: the composer does not
// have the focus, and no reply is being written.
func TestDriver_OnTheRecordedStartPage(t *testing.T) {
	start, isErr := fixture(t, "take_snapshot_empty")
	require.False(t, isErr)
	p := chatPage()
	p.tools["take_snapshot"] = ok(start)
	d := p.driver(t, devtools.DriverConfig{})

	t.Run("a message goes into the composer, which is found by its role and name", func(t *testing.T) {
		require.NoError(t, d.Submit(t.Context(), "hello"))

		assert.Equal(t, "1_7", p.called("fill")[0].args["uid"])
	})
	t.Run("there is no Stop to press", func(t *testing.T) {
		err := d.Stop(t.Context())

		require.ErrorIs(t, err, devtools.ErrNoStopButton)
		assert.ErrorContains(t, err, `it has: button "Send message"`)
		assert.Empty(t, p.called("click"))
	})
}

// A uid is only good for the snapshot it came from. When the page has changed
// by the time of the click, the browser says that the element is not found, in
// the words of the recording, and the driver looks the element up again.
func TestClick_AnElementThatIsGoneIsLookedUpAgain(t *testing.T) {
	chat, _ := fixture(t, "take_snapshot_chat")
	gone, isErr := fixture(t, "click_bad_uid")
	require.True(t, isErr)
	p := newPage()
	p.tools["take_snapshot"] = ok(chat)
	clicks := 0
	p.tools["click"] = func(map[string]any) (*mcp.CallToolResult, error) {
		if clicks++; clicks == 1 {
			return errorResult(gone), nil
		}
		return textResult("Successfully clicked on the element"), nil
	}
	d := p.driver(t, devtools.DriverConfig{})

	require.NoError(t, d.Click(t.Context(), devtools.Query{Role: "button", Name: "Send message"}))

	assert.Equal(t, []string{"take_snapshot", "click", "take_snapshot", "click"}, p.sequence())
}

func TestSnapshot_Find(t *testing.T) {
	snap := chatSnapshot(t)

	tests := []struct {
		name    string
		query   devtools.Query
		wantUID string
	}{
		{"by role and name", devtools.Query{Role: "textbox", Name: "Message"}, "1_7"},
		{"by role only", devtools.Query{Role: "button"}, "1_8"},
		{"by name only", devtools.Query{Name: "New chat"}, "1_2"},
		{"by part of the name", devtools.Query{Role: "button", NameContains: "Send"}, "1_8"},
		{"by pattern", devtools.Query{NameMatches: regexp.MustCompile(`^FINAL|FINAL ANSWER: \d+$`), Role: "StaticText"}, "2_1"},
		{"by flag", devtools.Query{Flag: "focused"}, "1_7"},
		{"the first of several", devtools.Query{Role: "StaticText"}, "1_3"},
		{"everything must match", devtools.Query{Role: "button", Name: "Send message", Flag: "focusable"}, ""},
		{"a name that is not exact", devtools.Query{Role: "button", Name: "Send"}, ""},
		{"a role that is not there", devtools.Query{Role: "dialog"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := snap.Find(tc.query)

			assert.Equal(t, tc.wantUID != "", ok)
			assert.Equal(t, tc.wantUID, got.UID)
		})
	}
	t.Run("all of them, in order", func(t *testing.T) {
		got := snap.FindAll(devtools.Query{Role: "StaticText"})

		require.Len(t, got, 3)
		assert.Equal(t, []string{"1_3", "2_0", "2_1"}, []string{got[0].UID, got[1].UID, got[2].UID})
		assert.Empty(t, snap.FindAll(devtools.Query{Role: "nothing"}))
	})
}

func TestSnapshot_Require(t *testing.T) {
	snap := chatSnapshot(t)

	t.Run("found", func(t *testing.T) {
		n, err := snap.Require(devtools.Query{Role: "button", Name: "Send message"})

		require.NoError(t, err)
		assert.Equal(t, "1_8", n.UID)
	})
	t.Run("not found says what the page has", func(t *testing.T) {
		_, err := snap.Require(devtools.Query{Role: "button", Name: "Stop"})

		var nf *devtools.NotFoundError
		require.ErrorAs(t, err, &nf)
		assert.EqualError(t, err, `the page has no button named "Stop" (it has: button "Send message")`)
	})
	t.Run("and when nothing of the role is named", func(t *testing.T) {
		_, err := snap.Require(devtools.Query{Role: "dialog"})

		assert.EqualError(t, err, "the page has no dialog")
	})
	t.Run("a long page lists only a few names", func(t *testing.T) {
		var b strings.Builder
		for i := range 40 {
			b.WriteString(strings.Repeat(" ", 2))
			b.WriteString("uid=1_" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ` link "Item ` + string(rune('A'+i%26)) + `"` + "\n")
		}

		_, err := devtools.ParseSnapshot(b.String()).Require(devtools.Query{Role: "link", Name: "Nope"})

		var nf *devtools.NotFoundError
		require.ErrorAs(t, err, &nf)
		assert.Len(t, nf.Roles, 12)
	})
}

func TestQueryAndNodeDescriptions(t *testing.T) {
	assert.Equal(t, "any element", devtools.Query{}.String())
	assert.Equal(t, `button named "Go" that is focused`, devtools.Query{Role: "button", Name: "Go", Flag: "focused"}.String())
	assert.Equal(t, `link with "chat" in its name`, devtools.Query{Role: "link", NameContains: "chat"}.String())
	assert.Equal(t, "textbox with a name matching ^M", devtools.Query{Role: "textbox", NameMatches: regexp.MustCompile("^M")}.String())
	assert.Equal(t, "main", devtools.Node{Role: "main"}.String())
	assert.Equal(t, `button "Go"`, devtools.Node{Role: "button", Name: "Go"}.String())
}
