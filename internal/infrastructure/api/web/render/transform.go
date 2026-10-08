package render

import (
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

// The transformers below run on the parsed document, before it is rendered.
// Together they make agent output safe and fit the page; the sanitizer in
// markdown.go is a second, independent layer behind them.

// rawHTMLAsText turns raw HTML, inline and block, into plain text nodes, which
// the renderer escapes. The reader sees the characters the agent wrote (for
// example "use the <div> tag") instead of losing them, and nothing can run.
//
// Pattern: Transform Before Render
type rawHTMLAsText struct{}

func (rawHTMLAsText) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	source := reader.Source()
	var inlines []*ast.RawHTML
	var blocks []*ast.HTMLBlock
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.RawHTML:
			inlines = append(inlines, v)
		case *ast.HTMLBlock:
			blocks = append(blocks, v)
		}
		return ast.WalkContinue, nil
	})

	for _, n := range inlines {
		parent := n.Parent()
		for i := range n.Segments.Len() {
			parent.InsertBefore(parent, n, ast.NewTextSegment(n.Segments.At(i)))
		}
		parent.RemoveChild(parent, n)
	}

	for _, n := range blocks {
		para := ast.NewParagraph()
		lines := n.Lines()
		for i := range lines.Len() {
			seg := lines.At(i)
			line := ast.NewTextSegment(seg.TrimRightSpace(source))
			line.SetHardLineBreak(i < lines.Len()-1 || n.HasClosure())
			para.AppendChild(para, line)
		}
		if n.HasClosure() {
			para.AppendChild(para, ast.NewTextSegment(n.ClosureLine.TrimRightSpace(source)))
		}
		parent := n.Parent()
		parent.InsertBefore(parent, n, para)
		parent.RemoveChild(parent, n)
	}
}

// headingsDown pushes every heading two levels down (h1 becomes h3, anything
// past h6 stays h6), so a reply never competes with the page's own h1/h2.
type headingsDown struct{}

func (headingsDown) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if h, ok := n.(*ast.Heading); ok && entering {
			h.Level = min(h.Level+2, 6)
		}
		return ast.WalkContinue, nil
	})
}

// imagesAsLinks replaces every image with a link to it. A remote image would
// load as soon as the reply renders, which lets a prompt-injected reply leak
// data through the URL; a link only does anything when the user clicks it.
type imagesAsLinks struct{}

func (imagesAsLinks) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	var images []*ast.Image
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if img, ok := n.(*ast.Image); ok && entering {
			images = append(images, img)
		}
		return ast.WalkContinue, nil
	})

	for _, img := range images {
		link := ast.NewLink()
		link.Destination = img.Destination
		link.Title = img.Title
		for c := img.FirstChild(); c != nil; {
			next := c.NextSibling()
			link.AppendChild(link, c)
			c = next
		}
		if link.FirstChild() == nil {
			link.AppendChild(link, ast.NewString([]byte("image")))
		}
		parent := img.Parent()
		parent.InsertBefore(parent, img, link)
		parent.RemoveChild(parent, img)
	}
}

// indentedAsFenced turns a code block made by indenting into a fenced one that
// names no language. The highlighter puts its container around fenced blocks
// only; as one of them, such a block gets the container, the label "text" and,
// in the page, the copy button, like every other code block.
type indentedAsFenced struct{}

func (indentedAsFenced) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	var blocks []*ast.CodeBlock
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if block, ok := n.(*ast.CodeBlock); ok && entering {
			blocks = append(blocks, block)
		}
		return ast.WalkContinue, nil
	})

	for _, block := range blocks {
		fenced := ast.NewFencedCodeBlock(nil)
		fenced.SetLines(block.Lines())
		fenced.SetBlankPreviousLines(block.HasBlankPreviousLines())
		parent := block.Parent()
		parent.ReplaceChild(parent, block, fenced)
	}
}
