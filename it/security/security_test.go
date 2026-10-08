// What an attacker, a curious neighbour or a careless page could do to the Web
// UI, tried against the application as the program runs it: a real PostgreSQL,
// the scripted agent, real HTTP. The routes are taken from the code of the web
// package, whichever of its files names them, so a route added later is covered
// or fails a test.
//
// Group A: needs Docker.
package security_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/v8tix/inside-agent-web-ui/it/internal/leak"
	"github.com/v8tix/inside-agent-web-ui/it/internal/stack"
)

const (
	bobEmail    = "bob@example.com"
	bobPassword = "bobs-long-enough-password"
	// secret marks text that only its owner may ever see.
	secret = "private-words-of-"
)

// within bounds what a case waits for.
func within(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return ctx
}

// finished starts a conversation as c and waits until its reply is saved.
func finished(t *testing.T, c *stack.Client, text string) ids {
	t.Helper()
	conv, reply := c.Start(text)
	c.OpenStream(within(t, 20*time.Second), conv, reply).Drain()
	return ids{conv, reply}
}

// hardened checks the headers every answer carries, whatever its status.
func hardened(t *testing.T, res *stack.Response, who string) {
	t.Helper()
	h := res.Header
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'", "base-uri 'self'", "form-action 'self'"} {
		assert.Contains(t, csp, want, who)
	}
	for _, never := range []string{"unsafe-inline", "unsafe-eval", "*", "http:", "https:"} {
		assert.NotContains(t, csp, never, "%s: the policy allows nothing from elsewhere", who)
	}
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"), who)
	assert.Equal(t, "DENY", h.Get("X-Frame-Options"), who)
	assert.Equal(t, "same-origin", h.Get("Referrer-Policy"), who)
	assert.Equal(t, "same-origin", h.Get("Cross-Origin-Opener-Policy"), who)
	assert.Equal(t, "same-origin", h.Get("Cross-Origin-Resource-Policy"), who)
	assert.Contains(t, h.Get("Permissions-Policy"), "camera=()", who)
	assert.Empty(t, h.Get("Strict-Transport-Security"), "%s: HSTS only goes with https", who)
}

// kept checks what a route lets the browser keep: nothing, except the files of
// the page, which are kept and checked again, and the event stream, which says
// it is live. Pages, fragments, redirects and errors hold one person's
// conversations, and the next person at the same computer must not get them
// back with the Back button.
func kept(t *testing.T, res *stack.Response, pattern, who string) {
	t.Helper()
	got := res.Header.Get("Cache-Control")
	switch {
	case pattern == "GET /static/{path...}" || pattern == "GET /favicon.ico":
		assert.NotEmpty(t, got, "%s: %s says how long it is good for", who, pattern)
		assert.NotContains(t, got, "no-store", "%s: %s is kept and checked again", who, pattern)
	case strings.HasSuffix(pattern, "/stream"):
		assert.Contains(t, []string{"no-cache", "no-store"}, got, "%s: %s", who, pattern)
	default:
		assert.Equal(t, "no-store", got, "%s: %s must not be kept", who, pattern)
	}
}

// snapshot is everything the Web UI keeps about conversations and sessions, as
// one string, so that "nothing changed" can be asserted.
func snapshot(t *testing.T, s *stack.Stack) string {
	t.Helper()
	var b strings.Builder
	for _, query := range []string{
		`SELECT id::text || ' ' || title || ' ' || updated_at::text FROM conversations ORDER BY id`,
		`SELECT id::text || ' ' || seq || ' ' || status || ' ' || content || ' ' || updated_at::text FROM messages ORDER BY id`,
		`SELECT id FROM sessions ORDER BY id`,
	} {
		rows, err := s.Pool.Query(t.Context(), query)
		require.NoError(t, err)
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			b.WriteString(line + "\n")
		}
		require.NoError(t, rows.Err())
		rows.Close()
		b.WriteString("--\n")
	}
	return b.String()
}

// normalize hides the ids a body was asked about, so the answers about two
// different ids can be compared.
func normalize(body string, x ids) string {
	return strings.NewReplacer(x.conv, "CONV", x.reply, "REPLY").Replace(body)
}

// inert asserts that nothing in a message body could run, load or leave.
func inert(t *testing.T, body *html.Node, where string) {
	t.Helper()
	forbidden := []string{"script", "iframe", "object", "embed", "style", "link", "meta", "base", "form", "img", "svg", "math", "video", "audio", "source", "input", "textarea", "select", "button"}
	for _, n := range stack.FindAll(body, func(n *html.Node) bool { return n.Type == html.ElementNode }) {
		assert.NotContains(t, forbidden, n.Data, "%s: <%s> in a message", where, n.Data)
		for _, a := range n.Attr {
			assert.False(t, strings.HasPrefix(a.Key, "on"), "%s: event handler %s on <%s>", where, a.Key, n.Data)
			assert.NotContains(t, []string{"style", "srcdoc"}, a.Key, "%s: %s on <%s>", where, a.Key, n.Data)
			if slices.Contains([]string{"href", "src", "action", "formaction", "data", "poster", "xlink:href"}, a.Key) {
				v := strings.ToLower(strings.TrimSpace(a.Val))
				assert.False(t, strings.HasPrefix(v, "javascript:") || strings.HasPrefix(v, "data:") || strings.HasPrefix(v, "vbscript:"),
					"%s: %s=%q on <%s>", where, a.Key, a.Val, n.Data)
			}
		}
	}
}

// bodyOf is the message body of a message.
func bodyOf(t *testing.T, m stack.Message) *html.Node {
	t.Helper()
	body := stack.Find(m.Node, stack.TestID("message-body"))
	require.NotNil(t, body)
	return body
}

// pageIsCSPClean asserts what the content security policy relies on: scripts and
// styles come from files of this site, nothing is inline, nothing is fetched
// from another host.
func pageIsCSPClean(t *testing.T, page string, where string) {
	t.Helper()
	doc := stack.Doc(t, page)
	for _, n := range stack.FindAll(doc, func(n *html.Node) bool { return n.Type == html.ElementNode }) {
		for _, a := range n.Attr {
			assert.False(t, strings.HasPrefix(a.Key, "on"), "%s: inline handler %s on <%s>", where, a.Key, n.Data)
			assert.NotEqual(t, "style", a.Key, "%s: inline style on <%s>", where, n.Data)
		}
		switch n.Data {
		case "style":
			t.Errorf("%s: an inline <style>", where)
		case "script":
			src := stack.Attr(n, "src")
			assert.True(t, strings.HasPrefix(src, "/static/"), "%s: a script that is not a file of this site: %q", where, src)
		case "link":
			if href := stack.Attr(n, "href"); href != "" {
				assert.False(t, strings.HasPrefix(href, "http") || strings.HasPrefix(href, "//"), "%s: <link> to %s", where, href)
			}
		}
	}
}

func TestSecuritySuite(t *testing.T) {
	leak.Check(t)
	s := stack.Start(t,
		stack.WithSetting("RATE_LIMIT_RPS", "1000"), stack.WithSetting("RATE_LIMIT_BURST", "1000"))
	s.AddUser(bobEmail, bobPassword)
	alice := s.SignedIn()
	bob := s.SignedInAs(bobEmail, bobPassword)

	t.Run("every route carries the security headers, whoever asks and whatever the answer (SC-8)", func(t *testing.T) {
		for _, r := range routes {
			t.Run(r.pattern, func(t *testing.T) {
				x := finished(t, alice, "headers")
				for who, c := range map[string]*stack.Client{
					"the owner":            s.SignedIn(),
					"another user":         s.SignedInAs(bobEmail, bobPassword),
					"an anonymous visitor": s.Anonymous(),
				} {
					res := r.call(c, x)
					hardened(t, res, who)
					kept(t, res, r.pattern, who)
				}
			})
		}
		t.Run("a page that is not there", func(t *testing.T) {
			res := alice.Get("/no/such/page")
			assert.Equal(t, http.StatusNotFound, res.Status)
			hardened(t, res, "a 404")
			kept(t, res, "GET /no/such/page", "a 404")
		})
		t.Run("a method the route doesn't take", func(t *testing.T) {
			res := alice.Do(http.MethodPut, "/c/"+uuid.New().String(), url.Values{})
			assert.Equal(t, http.StatusMethodNotAllowed, res.Status)
			hardened(t, res, "a 405")
		})
		t.Run("signing out needs a POST, so a link or an image can't do it", func(t *testing.T) {
			c := s.SignedIn()
			res := c.Get("/logout")
			assert.Equal(t, http.StatusMethodNotAllowed, res.Status)
			assert.Equal(t, http.StatusOK, c.Get("/").Status, "and the session is still good")
		})
	})

	t.Run("an anonymous visitor reaches only the public routes", func(t *testing.T) {
		for _, r := range routes {
			t.Run(r.pattern, func(t *testing.T) {
				x := finished(t, alice, secret+"alice")

				res := r.call(s.Anonymous(), x)

				assert.NotContains(t, res.Body, secret, "nothing private is given away")
				location := res.Header.Get("Location")
				switch {
				case r.public:
					assert.Less(t, res.Status, 500)
					assert.False(t, strings.HasPrefix(location, "/login?next="), "a public route does not ask to sign in")
				case strings.HasSuffix(r.pattern, "/stream"):
					// A redirect would make the browser's event-stream client retry
					// for ever, so this one says so inside the stream.
					assert.Equal(t, http.StatusOK, res.Status)
					assert.Contains(t, res.Body, "Your session has ended")
				default:
					assert.Equal(t, http.StatusSeeOther, res.Status)
					assert.True(t, strings.HasPrefix(location, "/login"), "sent to sign in, not to %q", location)
				}
			})
		}
	})

	t.Run("a request from another origin that would change something is refused (SC-9)", func(t *testing.T) {
		for _, r := range routes {
			if !r.changes {
				continue
			}
			t.Run(r.pattern, func(t *testing.T) {
				x := finished(t, alice, "cross-origin")
				c := s.SignedIn()
				before := snapshot(t, s)

				for _, from := range [][]string{
					{"Origin", "https://evil.example"},
					{"Origin", "http://127.0.0.1:1"}, // another port is another origin
					{"Sec-Fetch-Site", "cross-site"},
					{"Sec-Fetch-Site", "same-site"},
				} {
					res := r.call(c, x, from...)

					assert.Equal(t, http.StatusForbidden, res.Status, "%s %v", r.pattern, from)
					hardened(t, res, "a refused request")
				}

				assert.Equal(t, before, snapshot(t, s), "nothing was changed")
				for _, from := range [][]string{{"Origin", s.URL}, {"Sec-Fetch-Site", "same-origin"}, {}} {
					res := r.call(s.SignedIn(), finished(t, alice, "same origin"), from...)
					assert.NotEqual(t, http.StatusForbidden, res.Status, "the site's own pages are not refused: %v", from)
				}
			})
		}
		t.Run("reading is allowed from anywhere, as following a link is", func(t *testing.T) {
			res := alice.Get("/", "Sec-Fetch-Site", "cross-site")
			assert.Equal(t, http.StatusOK, res.Status)
		})
	})

	t.Run("another user's conversations can't be reached, seen or changed (SC-10)", func(t *testing.T) {
		bobs := finished(t, bob, secret+"bob")
		missing := ids{uuid.New().String(), uuid.New().String()}
		before := snapshot(t, s)
		runs := s.Model.Probe().Runs()

		for _, r := range routes {
			if !strings.Contains(r.pattern, "{id}") {
				continue
			}
			t.Run(r.pattern, func(t *testing.T) {
				foreign := r.call(alice, bobs)
				absent := r.call(alice, missing)

				assert.Equal(t, absent.Status, foreign.Status, "a conversation of someone else is answered as one that doesn't exist")
				assert.Equal(t, normalize(absent.Body, missing), normalize(foreign.Body, bobs))
				assert.NotContains(t, foreign.Body, secret)
				if strings.HasSuffix(r.pattern, "/stream") {
					// Streams always answer 200 and say inside them what went wrong, because
					// the browser's event-stream client retries any HTTP error for ever.
					assert.Equal(t, http.StatusOK, foreign.Status)
					assert.Contains(t, foreign.Body, "isn&#39;t available")
				} else {
					assert.Equal(t, http.StatusNotFound, foreign.Status)
				}
			})
		}

		assert.Equal(t, before, snapshot(t, s), "the other user's data is as it was")
		assert.Equal(t, runs, s.Model.Probe().Runs(), "and the agent was not asked anything")
		assert.NotContains(t, alice.Get("/").Body, secret+"bob", "it is not listed either")
		kept := bob.Messages(bobs.conv)
		require.Len(t, kept, 2)
		assert.Equal(t, secret+"bob", kept[0].Text)
	})

	t.Run("the raw text is what was stored, for its owner only, and never cached (SC-21)", func(t *testing.T) {
		x := finished(t, alice, "show it #markdown")

		res := alice.Raw(x.conv, x.reply)

		require.Equal(t, http.StatusOK, res.Status)
		assert.Equal(t, "text/plain; charset=utf-8", res.Header.Get("Content-Type"))
		assert.Equal(t, "no-store", res.Header.Get("Cache-Control"))
		assert.Equal(t, "nosniff", res.Header.Get("X-Content-Type-Options"))
		var stored string
		require.NoError(t, s.Pool.QueryRow(t.Context(), `SELECT content FROM messages WHERE id = $1`, x.reply).Scan(&stored))
		assert.Equal(t, stored, res.Body, "the bytes in the database")
		assert.Contains(t, res.Body, "# Report", "Markdown as written, not rendered")
		assert.Contains(t, res.Body, "```go")
		assert.Equal(t, http.StatusNotFound, bob.Raw(x.conv, x.reply).Status)

		conv, reply := alice.Start("not read yet")
		assert.Equal(t, http.StatusConflict, alice.Raw(conv, reply).Status, "a reply that isn't finished has no text yet")
	})

	t.Run("hostile replies are shown as inert text, streamed or reloaded (SC-8)", func(t *testing.T) {
		conv, reply := alice.Start("show me #html")

		events := alice.OpenStream(within(t, 20*time.Second), conv, reply).Drain()

		shown := stack.Named(events, "delta")
		require.NotEmpty(t, shown)
		for i, ev := range shown {
			messages := stack.MessagesIn(t, ev.Data)
			require.Len(t, messages, 1)
			inert(t, bodyOf(t, messages[0]), fmt.Sprintf("update %d", i+1))
		}
		page := alice.Get("/c/" + conv)
		for _, m := range stack.MessagesIn(t, page.Body) {
			inert(t, bodyOf(t, m), "the reloaded page")
		}
		pageIsCSPClean(t, page.Body, "the chat page")
		last := stack.MessagesIn(t, shown[len(shown)-1].Data)[0]
		assert.Contains(t, last.Text, "alert(1)", "the hostile text is there to read, not dropped")
		assert.Contains(t, stack.Text(stack.Find(last.Node, stack.TestID("message-body"))), "<script>", "as text")
	})

	t.Run("what a user types is shown as text (SC-8)", func(t *testing.T) {
		hostile := `<img src=x onerror=alert(1)> <script>alert(2)</script> [x](javascript:alert(3))`
		conv, reply := alice.Start(hostile)
		alice.OpenStream(within(t, 20*time.Second), conv, reply).Drain()

		page := alice.Get("/c/" + conv)

		messages := stack.MessagesIn(t, page.Body)
		require.Len(t, messages, 2)
		assert.Equal(t, hostile, messages[0].Text, "the words as typed")
		for _, m := range messages {
			inert(t, bodyOf(t, m), m.Role)
		}
		pageIsCSPClean(t, page.Body, "the chat page")
	})

	t.Run("the pages load nothing from other hosts, and nothing inline (SC-8)", func(t *testing.T) {
		for _, p := range []struct{ name, path string }{{"the new chat page", "/"}, {"the sign-in page", "/login"}} {
			c := alice
			if p.path == "/login" {
				c = s.Anonymous()
			}
			pageIsCSPClean(t, c.Get(p.path).Body, p.name)
		}
	})

	t.Run("passwords are hashed with argon2id and sessions are stored as digests (SC-11)", func(t *testing.T) {
		rows, err := s.Pool.Query(t.Context(), `SELECT email, password_hash FROM users ORDER BY email`)
		require.NoError(t, err)
		passwords := map[string]string{stack.Email: stack.Password, bobEmail: bobPassword}
		var seen int
		for rows.Next() {
			var email, hash string
			require.NoError(t, rows.Scan(&email, &hash))
			assert.True(t, strings.HasPrefix(hash, "$argon2id$"), "%s: %.12s...", email, hash)
			assert.NotContains(t, hash, passwords[email])
			seen++
		}
		require.NoError(t, rows.Err())
		rows.Close()
		assert.Equal(t, 2, seen)

		token := alice.Cookie("awu_session")
		require.NotEmpty(t, token)
		var withToken, all int
		require.NoError(t, s.Pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions WHERE id = $1`, token).Scan(&withToken))
		require.NoError(t, s.Pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions`).Scan(&all))
		assert.Zero(t, withToken, "the table can't be replayed as live sessions")
		assert.Positive(t, all)
	})

	t.Run("the session cookie is closed to scripts and to other sites (SC-11)", func(t *testing.T) {
		c := s.Anonymous()

		res := c.Post("/login", url.Values{"email": {stack.Email}, "password": {stack.Password}})

		cookie := sessionCookie(t, res)
		assert.True(t, cookie.HttpOnly, "scripts can't read it")
		assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite, "other sites can't make the browser send it with a POST")
		assert.Equal(t, "/", cookie.Path)
		assert.False(t, cookie.Secure, "over plain http it can't be Secure; see the https test")
		assert.Positive(t, cookie.MaxAge)

		out := c.Post("/logout", nil)
		assert.Negative(t, sessionCookie(t, out).MaxAge, "signing out expires it")
	})

	t.Run("input over the limits is refused and nothing is saved (SC-11)", func(t *testing.T) {
		x := finished(t, alice, "limits")
		limit := s.Config.Chat.MaxMessageChars
		bodyLimit := int(s.Config.HTTP.MaxBodyBytes)
		before := snapshot(t, s)

		tooLong := strings.Repeat("a", limit+1)
		inConversation := alice.Post("/c/"+x.conv+"/messages", url.Values{"content": {tooLong}}, "HX-Request", "true")
		firstMessage := alice.Post("/c", url.Values{"content": {tooLong}})
		hugeBody := alice.Post("/c/"+x.conv+"/messages", url.Values{"content": {strings.Repeat("a", bodyLimit+1)}})
		hugeLogin := s.Anonymous().Post("/login", url.Values{"email": {strings.Repeat("a", bodyLimit+1)}})

		assert.Equal(t, http.StatusUnprocessableEntity, inConversation.Status)
		assert.Contains(t, inConversation.Body, strconv.Itoa(limit), "it says what the limit is")
		assert.Equal(t, http.StatusUnprocessableEntity, firstMessage.Status)
		assert.Equal(t, http.StatusRequestEntityTooLarge, hugeBody.Status)
		assert.Equal(t, http.StatusRequestEntityTooLarge, hugeLogin.Status)
		assert.Equal(t, before, snapshot(t, s), "none of it was saved")

		exactly := alice.Post("/c/"+x.conv+"/messages", url.Values{"content": {strings.Repeat("a", limit)}}, "HX-Request", "true")
		assert.Equal(t, http.StatusOK, exactly.Status, "a message of exactly the limit is fine")
	})

	t.Run("text the database can't store is refused politely, not as a server error (SC-11)", func(t *testing.T) {
		x := finished(t, alice, "unstorable")
		before := snapshot(t, s)

		for name, text := range map[string]string{
			"a NUL character":          "hello\x00world",
			"bytes that are not UTF-8": "hello \xff\xfe",
		} {
			inConversation := alice.Post("/c/"+x.conv+"/messages", url.Values{"content": {text}}, "HX-Request", "true")
			firstMessage := alice.Post("/c", url.Values{"content": {text}})
			renamed := alice.Do(http.MethodPatch, "/c/"+x.conv, url.Values{"title": {text}}, "HX-Request", "true")

			assert.Equal(t, http.StatusUnprocessableEntity, inConversation.Status, "%s in a message", name)
			assert.Contains(t, inConversation.Body, "can&#39;t be saved", "%s: it says what is wrong", name)
			assert.Equal(t, http.StatusUnprocessableEntity, firstMessage.Status, "%s in a first message", name)
			assert.Equal(t, http.StatusUnprocessableEntity, renamed.Status, "%s in a title", name)
		}
		assert.Equal(t, before, snapshot(t, s), "none of it was saved")
	})
}

func sessionCookie(t *testing.T, res *stack.Response) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: res.Header}).Cookies() {
		if c.Name == "awu_session" {
			return c
		}
	}
	require.Fail(t, "no session cookie in the answer", "%v", res.Header)
	return nil
}

func TestSecurity_SignInsAreLimited(t *testing.T) {
	leak.Check(t)
	s := stack.Start(t, stack.WithSetting("RATE_LIMIT_RPS", "0.5"), stack.WithSetting("RATE_LIMIT_BURST", "3"))
	c := s.Anonymous()
	wrong := url.Values{"email": {stack.Email}, "password": {"not-the-password-1"}}
	right := url.Values{"email": {stack.Email}, "password": {stack.Password}}
	for i := range 3 {
		assert.Equal(t, http.StatusUnprocessableEntity, c.Post("/login", wrong).Status, "attempt %d", i+1)
	}

	refused := c.Post("/login", wrong)

	assert.Equal(t, http.StatusTooManyRequests, refused.Status)
	wait, err := strconv.Atoi(refused.Header.Get("Retry-After"))
	require.NoError(t, err, "it says when to come back")
	assert.Positive(t, wait)
	assert.Contains(t, refused.Body, "Too many sign-in attempts")
	hardened(t, refused, "a refused sign-in")
	assert.Equal(t, http.StatusTooManyRequests, c.Post("/login", right).Status, "even the right password waits")
	assert.Empty(t, c.Cookie("awu_session"))

	time.Sleep(time.Duration(wait)*time.Second + 200*time.Millisecond)

	assert.Equal(t, http.StatusSeeOther, c.Post("/login", right).Status, "after the wait it works")
}

func TestSecurity_SendingIsLimitedPerUser(t *testing.T) {
	leak.Check(t)
	s := stack.Start(t, stack.WithSetting("RATE_LIMIT_RPS", "0.5"), stack.WithSetting("RATE_LIMIT_BURST", "3"))
	s.AddUser(bobEmail, bobPassword)
	alice := s.SignedIn()
	conv, reply := alice.Start("one")
	alice.OpenStream(within(t, 20*time.Second), conv, reply).Drain()
	for _, text := range []string{"two", "three"} {
		reply = alice.Send(conv, text)
		alice.OpenStream(within(t, 20*time.Second), conv, reply).Drain()
	}
	before := snapshot(t, s)

	plain := alice.Post("/c/"+conv+"/messages", url.Values{"content": {"too many"}})
	htmx := alice.Post("/c/"+conv+"/messages", url.Values{"content": {"too many"}}, "HX-Request", "true")

	assert.Equal(t, http.StatusTooManyRequests, plain.Status)
	wait, err := strconv.Atoi(plain.Header.Get("Retry-After"))
	require.NoError(t, err)
	assert.Positive(t, wait)
	assert.Contains(t, plain.Body, "too quickly", "it says why")
	assert.Contains(t, plain.Body, "too many", "and keeps what was typed")
	assert.Equal(t, http.StatusTooManyRequests, htmx.Status)
	assert.Equal(t, "none", htmx.Header.Get("HX-Reswap"), "the page around the composer is left alone")
	hardened(t, htmx, "a refused send")
	assert.Equal(t, before, snapshot(t, s), "nothing was saved")
	for range 10 {
		assert.Equal(t, http.StatusOK, alice.Get("/c/"+conv).Status, "reading isn't limited")
	}
	bob := s.SignedInAs(bobEmail, bobPassword)
	bobConv, _ := bob.Start("bob is not slowed down by alice")
	assert.NotEmpty(t, bobConv, "the limit is per user")

	time.Sleep(time.Duration(wait)*time.Second + 200*time.Millisecond)

	assert.Equal(t, http.StatusOK, alice.Post("/c/"+conv+"/messages", url.Values{"content": {"later"}}, "HX-Request", "true").Status, "and it lifts")
}

func TestSecurity_OverHTTPS(t *testing.T) {
	leak.Check(t)
	s := stack.Start(t, stack.WithSetting("APP_BASE_URL", "https://chat.example.test"))
	c := s.Anonymous()

	res := c.Post("/login", url.Values{"email": {stack.Email}, "password": {stack.Password}})

	require.Equal(t, http.StatusSeeOther, res.Status)
	cookie := sessionCookie(t, res)
	assert.True(t, cookie.Secure, "the browser sends it over https only")
	assert.True(t, cookie.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	assert.Equal(t, "max-age=31536000; includeSubDomains", res.Header.Get("Strict-Transport-Security"), "and is told to stay on https")
	out := c.Post("/logout", nil)
	assert.True(t, sessionCookie(t, out).Secure, "the cookie that signs out is Secure too")
}
