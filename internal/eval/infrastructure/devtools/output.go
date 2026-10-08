package devtools

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Page is one entry of list_pages.
type Page struct {
	ID       int
	Title    string
	URL      string
	Selected bool
}

// pageLine matches "2: Fake chat (http://127.0.0.1:51008/) [selected]". The
// title may hold parentheses itself, so the URL is the last group.
var pageLine = regexp.MustCompile(`^(\d+): (.*?)\s*(?:\(([^()\s]*)\))?(\s+\[selected\])?$`)

// ParsePages reads the "## Pages" list that list_pages, new_page and several
// other tools print.
func ParsePages(text string) []Page {
	var pages []Page
	for line := range strings.SplitSeq(text, "\n") {
		m := pageLine.FindStringSubmatch(strings.TrimRight(line, "\r "))
		if m == nil {
			continue
		}
		id, _ := strconv.Atoi(m[1]) // the pattern only lets digits through
		p := Page{ID: id, Title: m[2], URL: m[3], Selected: m[4] != ""}
		if p.URL == "" && strings.Contains(p.Title, ":") && !strings.Contains(p.Title, " ") {
			p.URL, p.Title = p.Title, "" // "1: about:blank" has a URL and no title
		}
		pages = append(pages, p)
	}
	return pages
}

// SelectedPage is the page the tools act on by default.
func SelectedPage(pages []Page) (Page, bool) {
	for _, p := range pages {
		if p.Selected {
			return p, true
		}
	}
	return Page{}, false
}

// ErrNoScriptResult is returned when evaluate_script's output holds no result.
var ErrNoScriptResult = errors.New("evaluate_script printed no result")

const (
	scriptOpen  = "```json"
	scriptClose = "```"
)

// ParseScriptResult returns the JSON an evaluated function returned, which the
// tool prints between a "```json" line and a closing "```". The closing fence
// is the last one in the text, because the result may hold fences of its own: an
// answer with a code block in it, for one. A function that returns nothing
// prints an empty block, and the result is "null".
func ParseScriptResult(text string) ([]byte, error) {
	start := strings.Index(text, scriptOpen)
	end := strings.LastIndex(text, scriptClose)
	if start < 0 || end < start+len(scriptOpen) {
		return nil, fmt.Errorf("%w: %q", ErrNoScriptResult, shorten(text, 120))
	}
	if body := strings.TrimSpace(text[start+len(scriptOpen) : end]); body != "" {
		return []byte(body), nil
	}
	return []byte("null"), nil
}

// ConsoleMessage is one line of list_console_messages.
type ConsoleMessage struct {
	ID   int
	Type string // log, info, warn, error, issue, ...
	Text string
}

var consoleLine = regexp.MustCompile(`^msgid=(\d+) \[([^\]]+)\] (.*)$`)

// ParseConsole reads list_console_messages. The arguments count the tool adds
// ("(1 args)") and the repeat count of an issue ("(count: 2)") are left in the
// text, which is only ever shown, never compared.
func ParseConsole(text string) []ConsoleMessage {
	var out []ConsoleMessage
	for line := range strings.SplitSeq(text, "\n") {
		if m := consoleLine.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			id, _ := strconv.Atoi(m[1])
			out = append(out, ConsoleMessage{ID: id, Type: m[2], Text: m[3]})
		}
	}
	return out
}

// NetworkRequest is one line of list_network_requests.
type NetworkRequest struct {
	ID     int
	Method string
	URL    string
	// Status is the HTTP status; 0 when the line has none (still pending).
	Status int
	// Failure is the text in brackets when it is not a status, such as
	// "failed - net::ERR_CONNECTION_REFUSED".
	Failure string
}

var requestLine = regexp.MustCompile(`^reqid=(\d+) (\S+) (\S+)(?: \[([^\]]*)\])?`)

// ParseRequests reads list_network_requests.
func ParseRequests(text string) []NetworkRequest {
	var out []NetworkRequest
	for line := range strings.SplitSeq(text, "\n") {
		m := requestLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		id, _ := strconv.Atoi(m[1])
		r := NetworkRequest{ID: id, Method: m[2], URL: m[3]}
		if status, err := strconv.Atoi(m[4]); err == nil {
			r.Status = status
		} else {
			r.Failure = m[4]
		}
		out = append(out, r)
	}
	return out
}

func shorten(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > n {
		return string(r[:n]) + "…"
	}
	return text
}

// ending is shorten for a text whose end is what matters, such as the output of
// a program that failed: it keeps the last n characters, behind a mark that says
// the start was left out.
func ending(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > n {
		return "…" + string(r[len(r)-n:])
	}
	return text
}
