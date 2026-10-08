// Package render turns an agent's Markdown into safe HTML.
//
// Agent output is untrusted: it can be steered by prompt injection. It passes
// two independent layers before reaching a page. First goldmark with
// transformers that neutralize raw HTML and images; then a strict bluemonday
// allow-list. The HTML it returns contains no scripts, styles, event handlers,
// forms or images, and only http, https and mailto links.
package render

//go:generate go run ./cmd/chromacss -o ../../../../../assets/static/css/chroma.css

import (
	"html/template"
	"regexp"

	"bytes"
	"fmt"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/util"
)

// maxRenderBytes is the largest reply that is interpreted as Markdown. Anything
// bigger is shown as escaped plain text, so one huge reply cannot burn CPU on
// every update of a stream.
const maxRenderBytes = 1 << 20

var (
	classPattern = regexp.MustCompile(`^[a-z][a-z0-9 _-]{0,80}$`)
	langPattern  = regexp.MustCompile(`^[a-z0-9+#._-]{1,30}$`)
)

// finishedCacheBytes is how much HTML of finished replies is kept for the next
// view of a conversation.
const finishedCacheBytes = 32 << 20

// Markdown renders Markdown to sanitized HTML. It is safe for concurrent use.
type Markdown struct {
	md       goldmark.Markdown
	policy   *bluemonday.Policy
	finished *cache
}

// NewMarkdown builds the renderer.
func NewMarkdown() *Markdown {
	md := goldmark.New(
		goldmark.WithExtensions(
			// align="left" rather than style="text-align:left": inline styles are
			// forbidden by the CSP and removed by the sanitizer.
			extension.NewTable(extension.WithTableCellAlignMethod(extension.TableCellAlignAttribute)),
			extension.Strikethrough,
			extension.Linkify,
			highlighting.NewHighlighting(
				highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
				highlighting.WithWrapperRenderer(wrapCodeBlock),
			),
		),
		goldmark.WithParserOptions(parser.WithASTTransformers(
			util.Prioritized(rawHTMLAsText{}, 100),
			util.Prioritized(headingsDown{}, 200),
			util.Prioritized(imagesAsLinks{}, 300),
			util.Prioritized(indentedAsFenced{}, 400),
		)),
		// html.WithUnsafe is deliberately not set.
	)
	return &Markdown{md: md, policy: newPolicy(), finished: newCache(finishedCacheBytes)}
}

func newPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements(
		"p", "br", "hr", "strong", "em", "del", "blockquote", "ul", "ol", "li",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"table", "thead", "tbody", "tr", "th", "td",
		"pre", "code", "span", "div", "a",
	)
	p.AllowAttrs("align").Matching(bluemonday.CellAlign).OnElements("th", "td")
	p.AllowAttrs("start").Matching(bluemonday.Integer).OnElements("ol")

	// Links: absolute http, https and mailto only. A relative one would be
	// followed on our own origin, with the reader's session. A link keeps its
	// address and nothing else, so the title that the converter writes for a
	// link or an image ends here. Every link gets rel nofollow and noreferrer.
	// One with a host, a web address, also opens in a new tab with noopener; a
	// mail link has no host and gets neither.
	p.AllowStandardURLs()
	p.AllowRelativeURLs(false)
	p.AllowAttrs("href").OnElements("a")
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)

	// Highlighted code uses CSS classes, never inline styles.
	p.AllowAttrs("class").Matching(classPattern).OnElements("pre", "code", "span", "div")
	p.AllowAttrs("data-lang").Matching(langPattern).OnElements("div")
	return p
}

// Render converts src to sanitized HTML.
//
// Pattern: Two Independent Filters
func (m *Markdown) Render(src string) template.HTML {
	if len(src) > maxRenderBytes {
		return plain(src)
	}
	var buf bytes.Buffer
	if err := m.md.Convert([]byte(src), &buf); err != nil {
		return plain(src)
	}
	return template.HTML(m.policy.SanitizeBytes(buf.Bytes())) //nolint:gosec // sanitized by bluemonday above
}

// RenderFinal is Render for text that will not change again, a finished reply:
// the result is kept, so the next view of the conversation does not work it out
// again. Text that is still being written goes through Render, which keeps
// nothing: every snapshot of it is new.
func (m *Markdown) RenderFinal(src string) template.HTML {
	return m.finished.get(src, m.Render)
}

// Cached is how many finished replies are kept.
func (m *Markdown) Cached() int { return m.finished.len() }

// plain shows text as it is, escaped, for input that is not interpreted.
func plain(src string) template.HTML {
	return template.HTML(`<pre class="plain">` + template.HTMLEscapeString(src) + `</pre>`) //nolint:gosec // escaped
}

// wrapCodeBlock puts every code block in a labelled container. The highlighter
// calls it for fenced blocks, and a block made by indenting reaches it as one
// (see indentedAsFenced). The label is the fence's language in lower case: the
// highlighter finds a language whatever its case, so a block highlighted as Go
// is labelled "go" however the fence spells it. A language outside a safe
// alphabet, or none, is labelled "text". A copy button is added by the page's
// JavaScript, which keeps interactive elements out of the sanitizer's
// allow-list.
func wrapCodeBlock(w util.BufWriter, c highlighting.CodeBlockContext, entering bool) {
	if entering {
		label := "text"
		if lang, ok := c.Language(); ok {
			if lower := bytes.ToLower(lang); langPattern.Match(lower) {
				label = string(lower)
			}
		}
		_, _ = fmt.Fprintf(w, `<div class="code-block" data-lang="%s">`, label)
		if !c.Highlighted() {
			_, _ = w.WriteString("<pre><code>")
		}
		return
	}
	if !c.Highlighted() {
		_, _ = w.WriteString("</code></pre>")
	}
	_, _ = w.WriteString("</div>\n")
}
