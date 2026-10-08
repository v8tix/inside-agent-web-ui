package web

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Helpers for asserting on rendered HTML structurally rather than by substring.

func parseHTML(t *testing.T, doc string) *html.Node {
	t.Helper()
	nodes, err := html.ParseFragment(strings.NewReader(doc), &html.Node{Type: html.ElementNode, Data: "body", DataAtom: atom.Body})
	require.NoError(t, err)
	root := &html.Node{Type: html.ElementNode, Data: "root"}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	return root
}

func walkHTML(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkHTML(c, visit)
	}
}

// byTestID returns every element carrying data-testid=id, in document order.
func byTestID(root *html.Node, id string) []*html.Node {
	var out []*html.Node
	walkHTML(root, func(n *html.Node) {
		if n.Type == html.ElementNode && attrOf(n, "data-testid") == id {
			out = append(out, n)
		}
	})
	return out
}

func elementsOf(root *html.Node, tag string) []*html.Node {
	var out []*html.Node
	walkHTML(root, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == tag {
			out = append(out, n)
		}
	})
	return out
}

func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

func textContent(n *html.Node) string {
	var sb strings.Builder
	walkHTML(n, func(c *html.Node) {
		if c.Type == html.TextNode {
			sb.WriteString(c.Data)
		}
	})
	return sb.String()
}
