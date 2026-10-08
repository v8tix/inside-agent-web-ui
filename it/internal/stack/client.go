package stack

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Response is a finished HTTP exchange.
type Response struct {
	Status int
	Header http.Header
	Body   string
}

// Client talks to the Web UI the way a browser does: it keeps cookies, and it
// does not follow redirects, so that tests see them.
type Client struct {
	t    testing.TB
	base string
	http *http.Client
}

// Anonymous returns a client that has not signed in.
func (s *Stack) Anonymous() *Client {
	s.t.Helper()
	c := NewClient(s.t, s.URL)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients = append(s.clients, c)
	return c
}

// NewClient returns a client of the Web UI at base, such as http://127.0.0.1:8300.
func NewClient(t testing.TB, base string) *Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	c := &Client{t: t, base: base, http: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	t.Cleanup(c.http.CloseIdleConnections)
	return c
}

// SignIn signs the client in and fails the test if the Web UI refuses.
func (c *Client) SignIn(email, password string) *Client {
	c.t.Helper()
	res := c.Do(http.MethodPost, "/login", url.Values{"email": {email}, "password": {password}})
	require.Equal(c.t, http.StatusSeeOther, res.Status, "signing in as %s: %s", email, res.Body)
	return c
}

// SignedIn returns a client signed in as the first user.
func (s *Stack) SignedIn() *Client { return s.SignedInAs(Email, Password) }

// SignedInAs returns a client signed in as the user.
func (s *Stack) SignedInAs(email, password string) *Client {
	s.t.Helper()
	return s.Anonymous().SignIn(email, password)
}

// Do sends a request. form, when not nil, is the urlencoded body. headers are
// pairs of a name and a value.
func (c *Client) Do(method, path string, form url.Values, headers ...string) *Response {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(c.t.Context(), 30*time.Second)
	defer cancel()
	res, err := c.http.Do(c.request(ctx, method, path, form, headers))
	require.NoError(c.t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(c.t, err)
	return &Response{Status: res.StatusCode, Header: res.Header, Body: string(body)}
}

// Get is Do for a GET.
func (c *Client) Get(path string, headers ...string) *Response {
	c.t.Helper()
	return c.Do(http.MethodGet, path, nil, headers...)
}

// Post is Do for a POST of a form.
func (c *Client) Post(path string, form url.Values, headers ...string) *Response {
	c.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	return c.Do(http.MethodPost, path, form, headers...)
}

func (c *Client) request(ctx context.Context, method, path string, form url.Values, headers []string) *http.Request {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	require.NoError(c.t, err)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	require.Zero(c.t, len(headers)%2, "headers come in pairs")
	for i := 0; i < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return req
}

// Cookie returns the value of a cookie the client holds, or "".
func (c *Client) Cookie(name string) string {
	u, err := url.Parse(c.base)
	require.NoError(c.t, err)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

// Event is one server-sent event.
type Event struct {
	// Name is the event's name, "message" when it has none.
	Name string
	Data string
	// At is when the last line of the event arrived.
	At time.Time
}

// Stream is an open event stream, read one event at a time.
type Stream struct {
	Status int
	Header http.Header
	// Retry is the last retry: value the server sent, 0 if none.
	Retry time.Duration

	body   io.ReadCloser
	reader *bufio.Reader
	cancel context.CancelFunc
}

// OpenStream asks for a reply's event stream, as the browser's EventSource
// does. The stream is closed when the test ends or ctx is done.
func (c *Client) OpenStream(ctx context.Context, conv, reply string) *Stream {
	c.t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	req := c.request(ctx, http.MethodGet, "/c/"+conv+"/m/"+reply+"/stream", nil, []string{"Accept", "text/event-stream"})
	res, err := c.http.Do(req)
	if err != nil {
		cancel()
		require.NoError(c.t, err)
	}
	s := &Stream{Status: res.StatusCode, Header: res.Header, body: res.Body, reader: bufio.NewReader(res.Body), cancel: cancel}
	c.t.Cleanup(s.Close)
	return s
}

// Close ends the stream, as a browser does when the tab goes away.
func (s *Stream) Close() {
	s.cancel()
	_ = s.body.Close()
}

// Next returns the next event, or false when the stream has ended.
func (s *Stream) Next() (Event, bool) {
	var ev Event
	var data []string
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return Event{}, false
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if ev.Name == "" && len(data) == 0 {
				continue // a comment, or a retry: line, ended
			}
			ev.Name = defaultName(ev.Name)
			ev.Data = strings.Join(data, "\n")
			ev.At = time.Now()
			return ev, true
		case strings.HasPrefix(line, ":"):
			// a comment: the keep-alive, or the first line that opens the stream
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				ev.Name = value
			case "data":
				data = append(data, value)
			case "retry":
				if ms, err := strconv.Atoi(value); err == nil {
					s.Retry = time.Duration(ms) * time.Millisecond
				}
			}
		}
	}
}

func defaultName(name string) string {
	if name == "" {
		return "message"
	}
	return name
}

// Drain reads the stream to its end.
func (s *Stream) Drain() []Event {
	var events []Event
	for ev, ok := s.Next(); ok; ev, ok = s.Next() {
		events = append(events, ev)
	}
	return events
}

// Named returns the events of a stream that have the name.
func Named(events []Event, name string) []Event {
	var out []Event
	for _, ev := range events {
		if ev.Name == name {
			out = append(out, ev)
		}
	}
	return out
}
