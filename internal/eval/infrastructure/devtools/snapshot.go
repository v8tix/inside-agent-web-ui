// Package devtools drives the Web UI through Chrome DevTools MCP: it starts the
// server, calls its tools, and reads what they print.
package devtools

import (
	"encoding/json/v2"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Node is one line of a page snapshot: an element of the accessibility tree.
type Node struct {
	// UID is what the tools take to say which element to act on. It is only good
	// for the snapshot it came from.
	UID  string
	Role string
	// Name is the accessible name; empty when the element has none.
	Name string
	// Attrs are the key="value" parts after the name (url, level, value...).
	Attrs map[string]string
	// Flags are the bare words after the name (focusable, focused, disabled...).
	Flags []string
}

// Has reports whether the node carries the flag.
func (n Node) Has(flag string) bool { return slices.Contains(n.Flags, flag) }

func (n Node) String() string {
	if n.Name == "" {
		return n.Role
	}
	return fmt.Sprintf("%s %q", n.Role, n.Name)
}

// Snapshot is the accessibility tree of a page, in document order.
type Snapshot struct {
	Nodes []Node
}

// Query picks nodes. Every field that is set must match.
type Query struct {
	Role string
	// Name must equal the accessible name.
	Name string
	// NameContains must be part of the accessible name.
	NameContains string
	// NameMatches is a regular expression the accessible name must match.
	NameMatches *regexp.Regexp
	// Flag must be one of the node's flags.
	Flag string
}

func (q Query) matches(n Node) bool {
	switch {
	case q.Role != "" && n.Role != q.Role:
		return false
	case q.Name != "" && n.Name != q.Name:
		return false
	case q.NameContains != "" && !strings.Contains(n.Name, q.NameContains):
		return false
	case q.NameMatches != nil && !q.NameMatches.MatchString(n.Name):
		return false
	case q.Flag != "" && !n.Has(q.Flag):
		return false
	}
	return true
}

func (q Query) String() string {
	var parts []string
	if q.Role != "" {
		parts = append(parts, q.Role)
	}
	if q.Name != "" {
		parts = append(parts, fmt.Sprintf("named %q", q.Name))
	}
	if q.NameContains != "" {
		parts = append(parts, fmt.Sprintf("with %q in its name", q.NameContains))
	}
	if q.NameMatches != nil {
		parts = append(parts, fmt.Sprintf("with a name matching %s", q.NameMatches))
	}
	if q.Flag != "" {
		parts = append(parts, "that is "+q.Flag)
	}
	if len(parts) == 0 {
		return "any element"
	}
	return strings.Join(parts, " ")
}

// Find returns the first node that matches.
func (s Snapshot) Find(q Query) (Node, bool) {
	for _, n := range s.Nodes {
		if q.matches(n) {
			return n, true
		}
	}
	return Node{}, false
}

// FindAll returns every node that matches, in document order.
func (s Snapshot) FindAll(q Query) []Node {
	var out []Node
	for _, n := range s.Nodes {
		if q.matches(n) {
			out = append(out, n)
		}
	}
	return out
}

// Require is Find, with an error that says what the page did have.
func (s Snapshot) Require(q Query) (Node, error) {
	if n, ok := s.Find(q); ok {
		return n, nil
	}
	return Node{}, &NotFoundError{Query: q, Roles: s.namesOfRole(q.Role)}
}

// RequireNth is Require for the node that comes after skip others that match,
// for pages whose elements share a name.
func (s Snapshot) RequireNth(q Query, skip int) (Node, error) {
	if all := s.FindAll(q); skip >= 0 && skip < len(all) {
		return all[skip], nil
	}
	return Node{}, &NotFoundError{Query: q, Roles: s.namesOfRole(q.Role)}
}

// namesOfRole lists the nodes of a role (or every named node), to help whoever
// reads the error.
func (s Snapshot) namesOfRole(role string) []string {
	var out []string
	for _, n := range s.Nodes {
		if (role == "" || n.Role == role) && n.Name != "" && len(out) < 12 {
			out = append(out, n.String())
		}
	}
	return out
}

// NotFoundError says that the snapshot has no element like the one asked for.
type NotFoundError struct {
	Query Query
	// Roles lists a few elements the page does have, for the message.
	Roles []string
}

func (e *NotFoundError) Error() string {
	msg := "the page has no " + e.Query.String()
	if len(e.Roles) > 0 {
		msg += " (it has: " + strings.Join(e.Roles, ", ") + ")"
	}
	return msg
}

// nodeLine matches the start of a snapshot line: indentation, uid and role.
var nodeLine = regexp.MustCompile(`^ *uid=(\S+) (\S+)(.*)$`)

// ParseSnapshot reads the text take_snapshot returns. Lines that are not
// elements (the heading, a note about truncation) are ignored. The result has no
// nodes when the text holds none; the caller decides whether that is an error.
func ParseSnapshot(text string) Snapshot {
	var s Snapshot
	for line := range strings.SplitSeq(text, "\n") {
		m := nodeLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		n := Node{UID: m[1], Role: m[2]}
		n.Name, n.Attrs, n.Flags = parseTail(m[3])
		s.Nodes = append(s.Nodes, n)
	}
	return s
}

// parseTail reads what follows the role: an optional quoted name, then
// key="value" pairs and bare flags.
func parseTail(rest string) (name string, attrs map[string]string, flags []string) {
	rest = strings.TrimLeft(rest, " ")
	if strings.HasPrefix(rest, `"`) {
		var quoted string
		quoted, rest = cutQuoted(rest)
		name = unquote(quoted)
	}
	for {
		rest = strings.TrimLeft(rest, " ")
		if rest == "" {
			return name, attrs, flags
		}
		word, after, _ := strings.Cut(rest, " ")
		key, value, hasValue := strings.Cut(word, "=")
		if !hasValue {
			flags = append(flags, word)
			rest = after
			continue
		}
		if attrs == nil {
			attrs = map[string]string{}
		}
		if strings.HasPrefix(value, `"`) {
			var quoted string
			quoted, rest = cutQuoted(rest[len(key)+1:])
			attrs[key] = unquote(quoted)
			continue
		}
		attrs[key] = value
		rest = after
	}
}

// cutQuoted splits a string that starts with a double quote into the quoted
// part, quotes included, and what follows it. A string that never closes is
// taken whole.
func cutQuoted(s string) (quoted, rest string) {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return s[:i+1], s[i+1:]
		}
	}
	return s, ""
}

// unquote reads a JSON string, which is how the tool writes names; anything it
// cannot read is returned without its quotes.
func unquote(quoted string) string {
	var s string
	if err := json.Unmarshal([]byte(quoted), &s); err == nil {
		return s
	}
	return strings.Trim(quoted, `"`)
}
