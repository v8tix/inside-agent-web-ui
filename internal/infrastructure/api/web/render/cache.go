package render

import (
	"container/list"
	"crypto/sha256"
	"html/template"
	"sync"
)

// entryOverhead is what an entry costs besides its HTML: the key, the list
// element and the map slot. It keeps the budget honest for many small entries.
const entryOverhead = 128

// cache keeps the HTML of texts that are finished, up to a budget in bytes, and
// lets the least recently used go first. The key is a hash of the text, so what
// comes back is right for the text whatever the caller assumes about it.
//
// Showing a conversation renders every reply in it, and the cost is the largest
// part of the page: measured at 70 ms for a hundred replies, against 1 ms from
// here. A finished reply never changes, so the same ones are asked for on every
// view.
type cache struct {
	mu    sync.Mutex
	max   int
	size  int
	order *list.List // most recently used first
	items map[[sha256.Size]byte]*list.Element
}

type cacheEntry struct {
	key  [sha256.Size]byte
	html template.HTML
}

func newCache(maxBytes int) *cache {
	return &cache{max: maxBytes, order: list.New(), items: map[[sha256.Size]byte]*list.Element{}}
}

// get returns the HTML for src, working it out with render when it is not here.
// Two readers that ask at the same moment may both work it out; that costs a
// little time and nothing else.
func (c *cache) get(src string, render func(string) template.HTML) template.HTML {
	key := sha256.Sum256([]byte(src))

	c.mu.Lock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		html := el.Value.(*cacheEntry).html
		c.mu.Unlock()
		return html
	}
	c.mu.Unlock()

	html := render(src)

	cost := len(html) + entryOverhead
	if cost > c.max/4 {
		return html // one text must not push out a quarter of the others
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; ok {
		return html // another reader got there first
	}
	c.items[key] = c.order.PushFront(&cacheEntry{key: key, html: html})
	c.size += cost
	for c.size > c.max {
		oldest := c.order.Back()
		e := oldest.Value.(*cacheEntry)
		c.order.Remove(oldest)
		delete(c.items, e.key)
		c.size -= len(e.html) + entryOverhead
	}
	return html
}

// len is how many texts are kept.
func (c *cache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// bytes is what the kept texts cost.
func (c *cache) bytes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}
