package devtools_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
)

func TestParsePages(t *testing.T) {
	t.Run("the recorded lists: the one list_pages prints, and the one new_page answers with", func(t *testing.T) {
		listed, _ := fixture(t, "list_pages")
		opened, _ := fixture(t, "new_page")
		for tool, text := range map[string]string{"list_pages": listed, "new_page": opened} {
			pages := devtools.ParsePages(text)

			assert.Equal(t, []devtools.Page{
				{ID: 1, URL: "about:blank"},
				{ID: 2, Title: "Fake chat", URL: "http://127.0.0.1:51008/", Selected: true},
			}, pages, tool)
			selected, ok := devtools.SelectedPage(pages)
			require.True(t, ok, tool)
			assert.Equal(t, 2, selected.ID, "%s: the selected page is the tab a driver that opened it works in", tool)
		}
	})
	t.Run("pages listed after another tool's message", func(t *testing.T) {
		text, _ := fixture(t, "navigate_reload")

		pages := devtools.ParsePages(text)

		assert.Len(t, pages, 2, "the line about the reload is not a page")
	})
	t.Run("a title with parentheses", func(t *testing.T) {
		pages := devtools.ParsePages("3: Docs (beta) (http://x/y) [selected]\n4: Other (http://z/)")

		assert.Equal(t, []devtools.Page{
			{ID: 3, Title: "Docs (beta)", URL: "http://x/y", Selected: true},
			{ID: 4, Title: "Other", URL: "http://z/"},
		}, pages)
	})
	t.Run("none selected", func(t *testing.T) {
		_, ok := devtools.SelectedPage(devtools.ParsePages("1: about:blank"))

		assert.False(t, ok)
	})
	t.Run("nothing", func(t *testing.T) {
		assert.Empty(t, devtools.ParsePages("## Pages\n"))
	})
}

func TestParseScriptResult(t *testing.T) {
	t.Run("a recorded result", func(t *testing.T) {
		text, _ := fixture(t, "evaluate_script")

		got, err := devtools.ParseScriptResult(text)

		require.NoError(t, err)
		assert.JSONEq(t, `{"contract":"1","msgs":[
			{"role":"user","status":"complete","text":"What is the answer?"},
			{"role":"assistant","status":"complete","ttft":"63","dur":"722","text":"Thinking about your question. The answer is forty two. FINAL ANSWER: 42"}],"keydown":393}`, string(got))
	})
	t.Run("a result that holds code fences of its own", func(t *testing.T) {
		text := "Script ran on page and returned:\n```json\n{\"text\":\"Here:\\n```python\\nprint(1)\\n```\\ndone\"}\n```\n"

		got, err := devtools.ParseScriptResult(text)

		require.NoError(t, err)
		assert.JSONEq(t, "{\"text\":\"Here:\\n```python\\nprint(1)\\n```\\ndone\"}", string(got))
	})
	t.Run("a result of several lines", func(t *testing.T) {
		got, err := devtools.ParseScriptResult("Script ran on page and returned:\n```json\n{\n  \"a\": [1,\n 2]\n}\n```")

		require.NoError(t, err)
		assert.JSONEq(t, `{"a":[1,2]}`, string(got))
	})
	t.Run("a function that returns nothing", func(t *testing.T) {
		got, err := devtools.ParseScriptResult("Script ran on page and returned:\n```json\n\n```")

		require.NoError(t, err)
		assert.Equal(t, "null", string(got))
	})
	t.Run("plain values", func(t *testing.T) {
		for in, want := range map[string]string{"42": "42", `"text"`: `"text"`, "true": "true", "null": "null"} {
			got, err := devtools.ParseScriptResult("Script ran on page and returned:\n```json\n" + in + "\n```")

			require.NoError(t, err)
			assert.Equal(t, want, string(got))
		}
	})
	t.Run("no result in the text", func(t *testing.T) {
		for _, text := range []string{"", "Script ran on page and returned nothing", "```json", "```json\n{}"} {
			_, err := devtools.ParseScriptResult(text)

			require.ErrorIs(t, err, devtools.ErrNoScriptResult, text)
		}
	})
}

func TestParseConsole(t *testing.T) {
	text, _ := fixture(t, "list_console_messages")

	got := devtools.ParseConsole(text)

	assert.Equal(t, []devtools.ConsoleMessage{
		{ID: 1, Type: "issue", Text: "A form field element should have an id or name attribute (count: 1)"},
		{ID: 2, Type: "error", Text: "Failed to load resource: the server responded with a status of 404 (Not Found) (0 args)"},
		{ID: 3, Type: "error", Text: "spike: simulated console error (1 args)"},
		{ID: 4, Type: "error", Text: "Failed to load resource: the server responded with a status of 404 (Not Found) (0 args)"},
	}, got)
	assert.Empty(t, devtools.ParseConsole("## Console messages\nNo messages\n"))
}

func TestParseRequests(t *testing.T) {
	t.Run("recorded", func(t *testing.T) {
		text, _ := fixture(t, "list_network_requests")

		got := devtools.ParseRequests(text)

		assert.Equal(t, []devtools.NetworkRequest{
			{ID: 1, Method: "GET", URL: "http://127.0.0.1:51008/", Status: 200},
			{ID: 2, Method: "GET", URL: "http://127.0.0.1:51008/favicon.ico", Status: 404},
			{ID: 3, Method: "GET", URL: "http://127.0.0.1:51008/missing", Status: 404},
		}, got)
	})
	t.Run("a request without a status, and one that failed", func(t *testing.T) {
		got := devtools.ParseRequests("reqid=4 POST http://x/c\nreqid=5 GET http://x/s [failed - net::ERR_ABORTED]\nsomething else")

		assert.Equal(t, []devtools.NetworkRequest{
			{ID: 4, Method: "POST", URL: "http://x/c"},
			{ID: 5, Method: "GET", URL: "http://x/s", Failure: "failed - net::ERR_ABORTED"},
		}, got)
	})
}
