package evaldriver_test

import (
	"encoding/json/v2"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
	"github.com/v8tix/inside-agent-web-ui/internal/testagent"
	"github.com/v8tix/inside-agent-web-ui/it/internal/evalenv"
)

// longReply is a fixture with one reply that is taller than the window and takes
// several seconds to write, so a reader has time to scroll up: sixty short
// paragraphs, each a line of its own.
func longReply(t *testing.T, id string) *testagent.Fixture {
	t.Helper()
	var text strings.Builder
	for i := range 60 {
		fmt.Fprintf(&text, "Paragraph %d says one short thing.\n\n", i+1)
	}
	f, err := testagent.ParseFixture(fmt.Appendf(nil, `{"version":1,"replies":{%q:{"text":%q,"word_delay_ms":20}}}`, id, strings.TrimSpace(text.String())))
	require.NoError(t, err)
	return f
}

// view is where the reader is in the conversation.
type view struct {
	Top        float64 `json:"top"`
	Height     float64 `json:"height"`
	Client     float64 `json:"client"`
	JumpHidden bool    `json:"jumpHidden"`
	Status     string  `json:"status"`
	Chars      int     `json:"chars"`
}

func (v view) fromBottom() float64 { return v.Height - v.Top - v.Client }

const readView = `() => {
  const log = document.querySelector('[data-testid=messages]');
  const jump = document.querySelector('[data-testid=jump-latest]');
  const last = [...document.querySelectorAll('[data-testid=message][data-role=assistant]')].at(-1);
  return { top: log.scrollTop, height: log.scrollHeight, client: log.clientHeight, jumpHidden: jump.hidden,
           status: last ? last.dataset.status : '', chars: last ? last.innerText.length : 0 };
}`

// SC-12: a reader who scrolled up is left where they are while the reply grows,
// "Jump to latest" shows, and pressing it brings them back to the end.
func TestPage_ReadingPositionIsKept(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := evalenv.Start(t, testagent.WithFixture(longReply(t, "long-reply")), testagent.WithWordDelay(5*time.Millisecond))
	d := openDriver(t, env)
	ctx := t.Context()
	require.NoError(t, d.Login(ctx, env.Email, env.Password))
	require.NoError(t, d.StartChat(ctx))
	require.NoError(t, d.Submit(ctx, "Tell me about the press [long-reply]"))
	where := func() view {
		var v view
		require.NoError(t, d.Eval(ctx, readView, &v))
		return v
	}

	require.Eventually(t, func() bool { v := where(); return v.Height-v.Client > 150 && v.fromBottom() <= 80 }, 30*time.Second, 100*time.Millisecond,
		"the reply fills more than the window, and the reader is carried along with it")
	assert.True(t, where().JumpHidden, "nothing to jump to while following")

	var scrolled float64
	require.NoError(t, d.Eval(ctx, `() => { const log = document.querySelector('[data-testid=messages]'); log.scrollTop = 0; return log.scrollTop; }`, &scrolled))
	require.Eventually(t, func() bool { return !where().JumpHidden }, 5*time.Second, 50*time.Millisecond, `"Jump to latest" shows`)
	before := where()
	require.Greater(t, before.fromBottom(), 80.0, "the reader is far from the end")

	require.Eventually(t, func() bool { return where().Height > before.Height+150 }, 20*time.Second, 100*time.Millisecond, "while the reply goes on growing")
	after := where()
	assert.InDelta(t, before.Top, after.Top, 1, "the page did not move under the reader")
	assert.False(t, after.JumpHidden)

	require.NoError(t, d.Eval(ctx, `() => { document.querySelector('[data-testid=jump-latest]').click(); return true; }`, new(bool)))
	require.Eventually(t, func() bool { v := where(); return v.fromBottom() <= 80 && v.JumpHidden }, 5*time.Second, 50*time.Millisecond,
		"pressing it brings the reader to the end, and the button goes away")

	mark := where().Height
	require.Eventually(t, func() bool { return where().Height > mark+100 }, 20*time.Second, 100*time.Millisecond)
	assert.LessOrEqual(t, where().fromBottom(), 80.0, "and the reader is following again")
	assert.Equal(t, domain.ReplyComplete, await(t, d, 60*time.Second).Status)

	// The button follows the reader, not the updates: on a page that has stopped
	// changing, scrolling up shows it and scrolling down hides it.
	require.NoError(t, d.Eval(ctx, `() => { document.querySelector('[data-testid=messages]').scrollTop = 0; return true; }`, new(bool)))
	require.Eventually(t, func() bool { return !where().JumpHidden }, 5*time.Second, 50*time.Millisecond, "scrolling up shows the button, with nothing arriving")
	require.NoError(t, d.Eval(ctx, `() => { const log = document.querySelector('[data-testid=messages]'); log.scrollTop = log.scrollHeight; return true; }`, new(bool)))
	require.Eventually(t, func() bool { return where().JumpHidden }, 5*time.Second, 50*time.Millisecond, "and scrolling to the end hides it")
}

// Sending a message brings the view back to the end of the conversation, wherever
// the reader had scrolled to: what they sent, and the reply to it, are there.
func TestPage_SendingBringsTheViewBack(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := evalenv.Start(t, testagent.WithFixture(longReply(t, "long-reply")), testagent.WithWordDelay(5*time.Millisecond))
	d := openDriver(t, env)
	ctx := t.Context()
	require.NoError(t, d.Login(ctx, env.Email, env.Password))
	require.NoError(t, d.StartChat(ctx))
	require.NoError(t, d.Submit(ctx, "Tell me about the press [long-reply]"))
	require.Equal(t, domain.ReplyComplete, await(t, d, 60*time.Second).Status)
	where := func() view {
		var v view
		require.NoError(t, d.Eval(ctx, readView, &v))
		return v
	}
	whole := where()
	require.Greater(t, whole.Height-whole.Client, 150.0, "the conversation is taller than the window")

	// The reader goes back to the start of the reply, and stays there.
	require.NoError(t, d.Eval(ctx, `() => { document.querySelector('[data-testid=messages]').scrollTop = 0; return true; }`, new(bool)))
	require.Eventually(t, func() bool { v := where(); return !v.JumpHidden && v.fromBottom() > 80 }, 5*time.Second, 50*time.Millisecond,
		"the reader is far from the end, and the page knows it")

	require.NoError(t, d.Submit(ctx, "And what came after it?"))

	require.Eventually(t, func() bool { v := where(); return v.fromBottom() <= 80 && v.JumpHidden }, 10*time.Second, 50*time.Millisecond,
		"sending brings the reader to the end, where the new message is")
	assert.Equal(t, domain.ReplyComplete, await(t, d, 60*time.Second).Status)
	assert.LessOrEqual(t, where().fromBottom(), 80.0, "and the reader is still there when the reply is in")
}

// SC-14: a screen reader hears that the reply is done, once, and not a word of
// the reply as it is written.
func TestPage_CompletionIsAnnouncedOnce(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := evalenv.Start(t, testagent.WithFixture(longReply(t, "long-reply")), testagent.WithWordDelay(5*time.Millisecond))
	d := openDriver(t, env)
	ctx := t.Context()
	require.NoError(t, d.Login(ctx, env.Email, env.Password))
	require.NoError(t, d.StartChat(ctx))
	// A recorder on the live region, in the page before anything is sent: it keeps
	// each distinct thing the region says.
	require.NoError(t, d.Eval(ctx, `() => {
	  window.__announced = [];
	  let last = '';
	  new MutationObserver(() => {
	    const region = document.querySelector('[data-testid=live-status]');
	    const now = region ? region.textContent.trim() : '';
	    if (now && now !== last) window.__announced.push(now);
	    last = now;
	  }).observe(document.documentElement, {childList: true, characterData: true, subtree: true});
	  return true;
	}`, new(bool)))
	announced := func() []string {
		var said []string
		require.NoError(t, d.Eval(ctx, `() => window.__announced`, &said))
		return said
	}
	require.NoError(t, d.Submit(ctx, "Tell me about the press [long-reply]"))

	require.Eventually(t, func() bool {
		status, chars, found, err := d.Progress(ctx)
		return err == nil && found && status == domain.ReplyStreaming && chars > 200
	}, 30*time.Second, 100*time.Millisecond, "the reply is being written")
	assert.Empty(t, announced(), "nothing is announced while it is written")

	assert.Equal(t, domain.ReplyComplete, await(t, d, 60*time.Second).Status)
	require.Eventually(t, func() bool { return len(announced()) > 0 }, 5*time.Second, 50*time.Millisecond)
	time.Sleep(500 * time.Millisecond) // a second announcement would have come by now
	assert.Equal(t, []string{"Reply complete."}, announced(), "once, and only that")
}

// A request that fails without an answer to show (the network drops, the body is
// over the server's limit) must not leave the page looking dead: it says what
// happened, on the line the screen reader already watches, and keeps the draft.
func TestPage_ARequestThatFailsSaysSo(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := evalenv.Start(t, testagent.WithWordDelay(time.Millisecond))
	d := openDriver(t, env)
	ctx := t.Context()
	require.NoError(t, d.Login(ctx, env.Email, env.Password))
	ask(t, d, "A first question, so the page has a conversation to send into")

	line := func() string {
		var said string
		require.NoError(t, d.Eval(ctx, `() => document.querySelector('[data-testid=composer-error]').textContent.trim()`, &said))
		return said
	}
	draft := func() string {
		var typed string
		require.NoError(t, d.Eval(ctx, `() => document.querySelector('[data-testid=composer-input]').value`, &typed))
		return typed
	}
	// sendText types the JavaScript expression text into the composer and sends it.
	sendText := func(text string) {
		require.NoError(t, d.Eval(ctx, fmt.Sprintf(`() => {
		  const box = document.querySelector('[data-testid=composer-input]');
		  box.value = %s;
		  box.dispatchEvent(new Event('input', {bubbles: true}));
		  box.form.requestSubmit();
		  return true;
		}`, text), new(bool)))
	}

	t.Run("the network drops", func(t *testing.T) {
		require.NoError(t, d.Eval(ctx, `() => {
		  window.__send = XMLHttpRequest.prototype.send;
		  XMLHttpRequest.prototype.send = function () { setTimeout(() => this.dispatchEvent(new ProgressEvent('error')), 0); };
		  return true;
		}`, new(bool)))
		t.Cleanup(func() {
			_ = d.Eval(ctx, `() => { XMLHttpRequest.prototype.send = window.__send; return true; }`, new(bool))
		})

		sendText(`"this will not arrive"`)

		require.Eventually(t, func() bool { return strings.Contains(line(), "Couldn't reach the server") }, 10*time.Second, 100*time.Millisecond,
			"the page says the server could not be reached")
		assert.Equal(t, "this will not arrive", draft(), "and the text is still there to send again")
		require.NoError(t, d.Eval(ctx, `() => { XMLHttpRequest.prototype.send = window.__send; return true; }`, new(bool)))
	})

	t.Run("the message is too big for the server to take", func(t *testing.T) {
		// 15,000 characters of three bytes each are 135,000 bytes once a browser has
		// encoded them, over the 128 KiB the server takes: a refusal before it can
		// say what the message limit is.
		sendText(`'中'.repeat(15000)`)

		require.Eventually(t, func() bool { return strings.Contains(line(), "too big") }, 10*time.Second, 100*time.Millisecond,
			"the page says the message is too big")
		assert.Len(t, []rune(draft()), 15000, "and the text is kept, to be shortened")
	})

	t.Run("the next message clears the line and goes through", func(t *testing.T) {
		sendText(`"a short one"`)

		require.Eventually(t, func() bool { return line() == "" }, 10*time.Second, 100*time.Millisecond, "the old message is gone")
		assert.Equal(t, domain.ReplyComplete, await(t, d, 60*time.Second).Status)
		assert.Empty(t, draft(), "the composer is empty again")
	})
}

// A Stop that the server answers with an error did not stop anything: the reply
// goes on, and the button must come back so that it can be pressed again.
func TestPage_AStopThatFailsCanBePressedAgain(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := evalenv.Start(t, testagent.WithSlow(50*time.Millisecond, 400))
	d := openDriver(t, env)
	ctx := t.Context()
	require.NoError(t, d.Login(ctx, env.Email, env.Password))
	require.NoError(t, d.StartChat(ctx))
	require.NoError(t, d.Submit(ctx, "Go on and on #slow"))
	require.Eventually(t, func() bool {
		status, chars, found, err := d.Progress(ctx)
		return err == nil && found && status == domain.ReplyStreaming && chars > 0
	}, 30*time.Second, 100*time.Millisecond, "the reply is being written")
	stopIsOff := func() bool {
		var off bool
		require.NoError(t, d.Eval(ctx, `() => document.querySelector('[data-testid=stop]').disabled`, &off))
		return off
	}

	// The server answers the next Stop with a 500.
	require.NoError(t, d.Eval(ctx, `() => {
	  const open = XMLHttpRequest.prototype.open, send = XMLHttpRequest.prototype.send;
	  window.__restore = () => { XMLHttpRequest.prototype.open = open; XMLHttpRequest.prototype.send = send; };
	  XMLHttpRequest.prototype.open = function (method, url) { this.__url = String(url); return open.apply(this, arguments); };
	  XMLHttpRequest.prototype.send = function (body) {
	    if (!this.__url.endsWith('/stop')) return send.call(this, body);
	    for (const [name, value] of [['status', 500], ['response', 'boom'], ['responseText', 'boom']]) {
	      Object.defineProperty(this, name, { value });
	    }
	    setTimeout(() => this.dispatchEvent(new ProgressEvent('load')), 0);
	  };
	  return true;
	}`, new(bool)))
	require.NoError(t, d.Stop(ctx))

	require.Eventually(t, func() bool { return !stopIsOff() }, 10*time.Second, 100*time.Millisecond,
		"the press failed, so the button comes back")
	status, _, _, err := d.Progress(ctx)
	require.NoError(t, err)
	assert.Equal(t, domain.ReplyStreaming, status, "and the reply goes on")

	require.NoError(t, d.Eval(ctx, `() => { window.__restore(); return true; }`, new(bool)))
	require.NoError(t, d.Stop(ctx))
	assert.Equal(t, domain.ReplyStopped, await(t, d, 30*time.Second).Status, "pressed again, it stops")
}

// contrastOf measures, for each control, the contrast between its text and what
// is behind it as the browser draws them (WCAG 2: 4.5 to 1 for normal text). The
// background is whatever opaque or translucent layers lie behind the control.
// It settles the page's animations first, or it would read a color half way.
const contrastOf = `async (selectors) => {
  // Buttons fade between colors for a moment after the theme changes.
  await Promise.all(document.getAnimations().map((a) => a.finished.catch(() => null)));
  const parse = (css) => {
    const m = css.match(/rgba?\(([^)]+)\)/);
    if (!m) return null;
    const p = m[1].split(/[ ,\/]+/).filter(Boolean).map(Number);
    return { r: p[0], g: p[1], b: p[2], a: p.length > 3 ? p[3] : 1 };
  };
  const light = ({ r, g, b }) => {
    const f = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; };
    return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
  };
  const over = (top, under) => ({
    r: top.r * top.a + under.r * (1 - top.a), g: top.g * top.a + under.g * (1 - top.a), b: top.b * top.a + under.b * (1 - top.a), a: 1,
  });
  const behind = (el) => {
    const layers = [];
    for (let n = el; n; n = n.parentElement) {
      const c = parse(getComputedStyle(n).backgroundColor);
      if (c && c.a > 0) { layers.push(c); if (c.a === 1) break; }
    }
    return layers.reverse().reduce((under, layer) => over(layer, under), { r: 255, g: 255, b: 255, a: 1 });
  };
  const out = {};
  for (const selector of selectors) {
    const el = document.querySelector(selector);
    if (!el) { out[selector] = -1; continue; }
    const text = parse(getComputedStyle(el).color);
    if (!text) { out[selector] = -2; continue; }
    const back = behind(el);
    const a = light(over(text, back)), b = light(back);
    out[selector] = (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
  }
  return out;
}`

// The theme follows the system, so half of the readers see the dark one. Bootstrap
// leaves its outline buttons and its danger text as they are in the light theme,
// which is too faint on a dark background.
func TestPage_TextIsReadableInTheDarkTheme(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := evalenv.Start(t, testagent.WithWordDelay(time.Millisecond))
	d := openDriver(t, env)
	ctx := t.Context()
	require.NoError(t, d.Login(ctx, env.Email, env.Password))
	ask(t, d, "A first question, so the page has a reply with its actions")
	require.NoError(t, d.PressRowButton(ctx, devtools.RowRename, d.Conversation()), "the rename form is open")
	// A message on the composer's alert line, as a failed send leaves it.
	require.NoError(t, d.Eval(ctx, `() => { document.querySelector('[data-testid=composer-error]').textContent = 'Something went wrong.'; return true; }`, new(bool)))
	const wanted = 4.5

	for _, theme := range []string{"light", "dark"} {
		t.Run(theme, func(t *testing.T) {
			require.NoError(t, d.Eval(ctx, fmt.Sprintf(`() => { document.documentElement.setAttribute('data-bs-theme', %q); return true; }`, theme), new(bool)))
			controls := []string{
				"[data-testid=copy-message]", "[data-testid=regenerate]", "[data-testid=stop]",
				"[data-testid=theme-toggle]", "[data-testid=logout]", "[data-testid=rename-cancel]",
				"[data-testid=composer-error]",
			}
			var ratios map[string]float64
			require.NoError(t, d.Eval(ctx, fmt.Sprintf(`() => (%s)(%s)`, contrastOf, mustJSON(t, controls)), &ratios))

			for _, selector := range controls {
				ratio, found := ratios[selector]
				require.True(t, found, selector)
				require.Greater(t, ratio, 0.0, "%s is on the page and its colors can be read", selector)
				assert.GreaterOrEqual(t, ratio, wanted, "%s in the %s theme: %.2f to 1", selector, theme, ratio)
			}
		})
	}
}

// The Copy button copies the reply as it is stored, fetched from the server. If
// the session has ended the fetch is refused, and Copy must say it failed, not
// put the sign-in page on the clipboard.
func TestPage_CopyingAReply(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := evalenv.Start(t, testagent.WithWordDelay(time.Millisecond))
	d := openDriver(t, env)
	ctx := t.Context()
	require.NoError(t, d.Login(ctx, env.Email, env.Password))
	ask(t, d, "something to copy")
	// A clipboard that can be read back: the page's own, behind a stub.
	require.NoError(t, d.Eval(ctx, `() => {
	  window.__copied = [];
	  Object.defineProperty(navigator.clipboard, 'writeText', { configurable: true, value: async (text) => { window.__copied.push(text); } });
	  return true;
	}`, new(bool)))
	label := func() string {
		var text string
		require.NoError(t, d.Eval(ctx, `() => document.querySelector('[data-testid=copy-message]').textContent.trim()`, &text))
		return text
	}
	copied := func() []string {
		var got []string
		require.NoError(t, d.Eval(ctx, `() => window.__copied`, &got))
		return got
	}
	press := func() {
		require.NoError(t, d.Eval(ctx, `() => { document.querySelector('[data-testid=copy-message]').click(); return true; }`, new(bool)))
	}

	t.Run("it copies the reply as it is stored", func(t *testing.T) {
		press()

		require.Eventually(t, func() bool { return label() == "Copied" }, 10*time.Second, 50*time.Millisecond)
		assert.Equal(t, []string{"You said: something to copy"}, copied())
		require.Eventually(t, func() bool { return label() == "Copy" }, 10*time.Second, 50*time.Millisecond, "and the button goes back")
	})

	t.Run("it does not copy an answer that is not the reply's text, whatever its status", func(t *testing.T) {
		// A captive portal or a proxy's sign-in page answers 200 with HTML.
		require.NoError(t, d.Eval(ctx, `() => {
		  window.__fetch = window.fetch;
		  window.fetch = async () => new Response('<html>please sign in</html>', { status: 200, headers: { 'Content-Type': 'text/html' } });
		  return true;
		}`, new(bool)))
		t.Cleanup(func() { _ = d.Eval(ctx, `() => { window.fetch = window.__fetch; return true; }`, new(bool)) })

		press()

		require.Eventually(t, func() bool { return label() == "Copy failed" }, 10*time.Second, 50*time.Millisecond)
		assert.Equal(t, []string{"You said: something to copy"}, copied(), "the page's HTML did not reach the clipboard")
		require.NoError(t, d.Eval(ctx, `() => { window.fetch = window.__fetch; return true; }`, new(bool)))
		require.Eventually(t, func() bool { return label() == "Copy" }, 10*time.Second, 50*time.Millisecond)
	})

	t.Run("it says so when the session has ended, and copies nothing", func(t *testing.T) {
		require.NoError(t, d.Eval(ctx, `async () => { await fetch('/logout', { method: 'POST' }); return true; }`, new(bool)))

		press()

		require.Eventually(t, func() bool { return label() == "Copy failed" }, 10*time.Second, 50*time.Millisecond)
		assert.Equal(t, []string{"You said: something to copy"}, copied(), "the sign-in page did not reach the clipboard")
	})
}

// Every question of an evaluation opens with the same prompt, so every
// conversation has the same title and every button in the sidebar the same name.
// Acting on a conversation must act on that one, not on the first row with that
// name, which can be another worker's, still being written. Two browsers of one
// user, the second one's conversation newer than the first's, and the first
// browser's page showing both with the newer on top.
func TestDriver_ActsOnItsOwnConversationWhenTitlesAreTheSame(t *testing.T) {
	evalenv.RequireBrowser(t)
	const question = "the same words at the start of every conversation"
	setup := func(t *testing.T) (mine, theirs *devtools.Driver, mineID, theirsID string) {
		t.Helper()
		env := evalenv.Start(t, testagent.WithWordDelay(5*time.Millisecond))
		mine, theirs = openDriver(t, env), openDriver(t, env)
		ctx := t.Context()
		require.NoError(t, mine.Login(ctx, env.Email, env.Password))
		require.NoError(t, theirs.Login(ctx, env.Email, env.Password))
		ask(t, mine, question)
		ask(t, theirs, question)
		mineID, theirsID = mine.Conversation(), theirs.Conversation()
		require.NotEqual(t, mineID, theirsID)
		require.NoError(t, mine.Reload(t.Context()))
		return mine, theirs, mineID, theirsID
	}
	// rows are the conversations the server lists, as a fresh page shows them.
	rows := func(t *testing.T, d *devtools.Driver) map[string]string {
		t.Helper()
		require.NoError(t, d.StartChat(t.Context()))
		var found []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		require.NoError(t, d.Eval(t.Context(), `() => [...document.querySelectorAll('[data-testid=conversation]')].map((r) => ({id: r.dataset.id, title: r.querySelector('a').textContent.trim()}))`, &found))
		out := map[string]string{}
		for _, r := range found {
			out[r.ID] = r.Title
		}
		return out
	}

	t.Run("deleting removes its own", func(t *testing.T) {
		mine, theirs, mineID, theirsID := setup(t)

		require.NoError(t, mine.DeleteChat(t.Context()))

		left := rows(t, theirs)
		assert.Contains(t, left, theirsID, "the other conversation is still there")
		assert.NotContains(t, left, mineID, "and mine is gone")
	})
	t.Run("renaming changes its own", func(t *testing.T) {
		mine, theirs, mineID, theirsID := setup(t)
		ctx := t.Context()

		require.NoError(t, mine.PressRowButton(ctx, devtools.RowRename, mineID))
		require.NoError(t, mine.Fill(ctx, devtools.Query{Role: "textbox", Name: "Conversation title"}, "My new title"))
		require.NoError(t, mine.Click(ctx, devtools.Query{Role: "button", Name: "Save"}))

		require.Eventually(t, func() bool { return rows(t, theirs)[mineID] == "My new title" }, 10*time.Second, 200*time.Millisecond, "mine was renamed")
		assert.Equal(t, question, rows(t, theirs)[theirsID], "the other keeps its title")
	})
}

// A reply the agent has not started to write shows a typing indicator, a
// decoration and not text. Progress has to count only what a reader can read, or
// a probe waiting for "some text" presses Stop or reloads the page while there is
// nothing yet to keep. A real model always has such a moment before its first word.
func TestDriver_TheTypingIndicatorIsNotText(t *testing.T) {
	evalenv.RequireBrowser(t)
	fixture, err := testagent.ParseFixture([]byte(`{"version":1,"replies":{"pause":{"text":"Hello at last.","first_delay_ms":6000}}}`))
	require.NoError(t, err)
	env := evalenv.Start(t, testagent.WithFixture(fixture), testagent.WithWordDelay(5*time.Millisecond))
	d := openDriver(t, env)
	ctx := t.Context()
	require.NoError(t, d.Login(ctx, env.Email, env.Password))
	require.NoError(t, d.StartChat(ctx))
	require.NoError(t, d.Submit(ctx, "Say something after a pause [pause]"))
	require.Eventually(t, func() bool {
		status, _, found, err := d.Progress(ctx)
		return err == nil && found && !status.Terminal()
	}, 20*time.Second, 50*time.Millisecond, "the reply is on the page and not finished")

	for range 5 { // the agent is silent for six seconds
		status, chars, found, err := d.Progress(ctx)
		require.NoError(t, err)
		require.True(t, found)
		assert.Zero(t, chars, "the typing indicator is not text (the reply is %s)", status)
		time.Sleep(300 * time.Millisecond)
	}

	require.Eventually(t, func() bool {
		_, chars, _, err := d.Progress(ctx)
		return err == nil && chars > 0
	}, 20*time.Second, 100*time.Millisecond, "and once the agent speaks, its words count")
	assert.Equal(t, domain.ReplyComplete, await(t, d, 30*time.Second).Status)
}

// allowed are the paths the Web UI's own pages ask for, and the one the
// evaluator adds: reading the stored reply.
var allowed = []*regexp.Regexp{
	regexp.MustCompile(`^/$`),
	regexp.MustCompile(`^/login$`),
	regexp.MustCompile(`^/c$`),
	regexp.MustCompile(`^/c/[0-9a-f-]{36}$`),
	regexp.MustCompile(`^/c/[0-9a-f-]{36}/(messages|item|rename|delete)$`),
	regexp.MustCompile(`^/c/[0-9a-f-]{36}/m/[0-9a-f-]{36}/(stream|stop|regenerate)$`),
	regexp.MustCompile(`^/c/[0-9a-f-]{36}/m/[0-9a-f-]{36}/raw$`), // the documented fetch
	regexp.MustCompile(`^/static/`),
	regexp.MustCompile(`^/favicon\.(ico|svg)$`),
}

// EC-4: the evaluator is a user, not a client of the API. What the browser asks
// for in an attempt is what the Web UI's pages ask for, plus reading the stored
// reply; it never goes to the agent; and the agent is asked once per question.
func TestDriver_OnlyDoesWhatABrowserDoes(t *testing.T) {
	evalenv.RequireBrowser(t)
	env := evalenv.Start(t, testagent.WithWordDelay(5*time.Millisecond))
	d := openDriver(t, env)
	ctx := t.Context()
	require.NoError(t, d.Login(ctx, env.Email, env.Password))

	reply := ask(t, d, "what is the capital of France")
	_, err := d.ReadRaw(ctx, reply)
	require.NoError(t, err)
	obs, err := d.Collect(ctx)
	require.NoError(t, err)

	require.NotEmpty(t, obs.Requests)
	var posted, streamed, fetched bool
	for _, r := range obs.Requests {
		u, err := url.Parse(r.URL)
		require.NoError(t, err)
		assert.Equal(t, env.URL, u.Scheme+"://"+u.Host, "every request goes to the Web UI, not to the agent or anywhere else: %s %s", r.Method, r.URL)
		assert.True(t, matchesAny(u.Path), "%s %s is not something the pages ask for", r.Method, u.Path)
		switch {
		case r.Method == "POST" && u.Path == "/c":
			posted = true
		case strings.HasSuffix(u.Path, "/stream"):
			streamed = true
		case strings.HasSuffix(u.Path, "/raw"):
			fetched = true
			assert.Equal(t, "GET", r.Method)
		}
	}
	assert.True(t, posted, "the question was sent by the page's own form")
	assert.True(t, streamed, "the reply came over the page's own event stream")
	assert.True(t, fetched, "and the stored reply was read once, through the documented endpoint")
	assert.Equal(t, int64(1), env.Model.Probe().Runs(), "the agent was asked once, by the Web UI")
}

func matchesAny(path string) bool {
	for _, re := range allowed {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// mustJSON spells v as JSON, to put into a script.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	require.NoError(t, err)
	return string(out)
}
