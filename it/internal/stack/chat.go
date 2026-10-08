package stack

import (
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/stretchr/testify/require"
)

// Start opens a conversation with its first message, as the empty-state form
// does, and returns the ids of the conversation and of the reply being written.
func (c *Client) Start(content string) (conv, reply string) {
	c.t.Helper()
	res := c.Post("/c", url.Values{"content": {content}})
	require.Equal(c.t, http.StatusSeeOther, res.Status, res.Body)
	location := res.Header.Get("Location")
	require.True(c.t, strings.HasPrefix(location, "/c/"), "redirected to %q", location)
	conv = path.Base(location)
	return conv, c.LatestReply(conv)
}

// Send adds a message to a conversation the way the composer does (htmx), and
// returns the id of the reply being written.
func (c *Client) Send(conv, content string) (reply string) {
	c.t.Helper()
	res := c.Post("/c/"+conv+"/messages", url.Values{"content": {content}}, "HX-Request", "true")
	require.Equal(c.t, http.StatusOK, res.Status, res.Body)
	messages := MessagesIn(c.t, res.Body)
	require.Len(c.t, messages, 2, "the answer holds the new message and the reply: %s", res.Body)
	require.Equal(c.t, "assistant", messages[1].Role)
	return messages[1].ID
}

// Messages lists the messages of a conversation as its page shows them.
func (c *Client) Messages(conv string) []Message {
	c.t.Helper()
	res := c.Get("/c/" + conv)
	require.Equal(c.t, http.StatusOK, res.Status, res.Body)
	return MessagesIn(c.t, res.Body)
}

// LatestReply is the id of the last message of a conversation.
func (c *Client) LatestReply(conv string) string {
	c.t.Helper()
	messages := c.Messages(conv)
	require.NotEmpty(c.t, messages)
	last := messages[len(messages)-1]
	require.Equal(c.t, "assistant", last.Role)
	return last.ID
}

// Stop presses Stop on a reply.
func (c *Client) Stop(conv, reply string) *Response {
	c.t.Helper()
	return c.Post("/c/"+conv+"/m/"+reply+"/stop", nil, "HX-Request", "true")
}

// Regenerate presses Regenerate (or Retry) on a reply. A new reply takes its
// place; the answer is its wrapper.
func (c *Client) Regenerate(conv, reply string) *Response {
	c.t.Helper()
	return c.Post("/c/"+conv+"/m/"+reply+"/regenerate", nil, "HX-Request", "true")
}

// Raw fetches the text of a finished reply exactly as stored.
func (c *Client) Raw(conv, reply string) *Response {
	c.t.Helper()
	return c.Get("/c/" + conv + "/m/" + reply + "/raw")
}
