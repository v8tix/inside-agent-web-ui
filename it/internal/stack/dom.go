package stack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

// Doc parses an HTML page or fragment, as a browser would.
func Doc(t testing.TB, markup string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(markup))
	require.NoError(t, err)
	return doc
}

// Find returns the first node under n (n included) that match accepts, or nil.
func Find(n *html.Node, match func(*html.Node) bool) *html.Node {
	if match(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := Find(c, match); found != nil {
			return found
		}
	}
	return nil
}

// FindAll returns every node under n (n included) that match accepts, in
// document order.
func FindAll(n *html.Node, match func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	if match(n) {
		out = append(out, n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, FindAll(c, match)...)
	}
	return out
}

// Attr is the value of an attribute, or "".
func Attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// TestID matches the element with the data-testid.
func TestID(id string) func(*html.Node) bool {
	return func(n *html.Node) bool { return n.Type == html.ElementNode && Attr(n, "data-testid") == id }
}

// Text is the text under n that a reader would see: not what is in scripts or
// styles. Runs of white space are kept as they are.
func Text(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// Message is a message as a page shows it.
type Message struct {
	ID     string
	Role   string // "user" or "assistant"
	Status string
	// Text is what the reader sees in the message body.
	Text string
	// Node is the whole article, for anything else a test wants to look at.
	Node *html.Node
}

// MessagesIn lists the messages in a page or fragment, in order.
func MessagesIn(t testing.TB, markup string) []Message {
	t.Helper()
	var out []Message
	for _, a := range FindAll(Doc(t, markup), TestID("message")) {
		m := Message{ID: Attr(a, "data-id"), Role: Attr(a, "data-role"), Status: Attr(a, "data-status"), Node: a}
		if body := Find(a, TestID("message-body")); body != nil {
			m.Text = strings.TrimSpace(Text(body))
		}
		out = append(out, m)
	}
	return out
}
