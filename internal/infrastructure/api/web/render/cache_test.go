package render

import (
	"html/template"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// counting is a stand-in for the renderer that counts its work.
type counting struct{ calls atomic.Int64 }

func (c *counting) render(src string) template.HTML {
	c.calls.Add(1)
	return template.HTML("<p>" + src + "</p>")
}

func TestCache(t *testing.T) {
	t.Run("the second ask for a text is answered from memory", func(t *testing.T) {
		var work counting
		c := newCache(1 << 20)

		first := c.get("hello", work.render)
		second := c.get("hello", work.render)

		assert.Equal(t, template.HTML("<p>hello</p>"), first)
		assert.Equal(t, first, second)
		assert.EqualValues(t, 1, work.calls.Load())
		assert.Equal(t, 1, c.len())
	})

	t.Run("different texts are different entries, however alike they start", func(t *testing.T) {
		var work counting
		c := newCache(1 << 20)

		a, b := c.get("the answer is 4", work.render), c.get("the answer is 5", work.render)

		assert.NotEqual(t, a, b)
		assert.EqualValues(t, 2, work.calls.Load())
		assert.Equal(t, 2, c.len())
	})

	t.Run("it keeps within its budget by letting the least recently used go", func(t *testing.T) {
		var work counting
		text := func(n int) string { return strings.Repeat(string(rune('a'+n)), 1000) }
		c := newCache(4 * (1000 + len("<p></p>") + entryOverhead)) // room for four
		for n := range 4 {
			c.get(text(n), work.render)
		}
		require.Equal(t, 4, c.len())
		c.get(text(0), work.render) // 0 is used again, so 1 is now the oldest
		before := work.calls.Load()

		c.get(text(4), work.render) // a fifth: one has to go

		assert.Equal(t, 4, c.len())
		c.get(text(0), work.render)
		assert.Equal(t, before+1, work.calls.Load(), "0 was kept (only the fifth was worked out)")
		c.get(text(1), work.render)
		assert.Equal(t, before+2, work.calls.Load(), "1 was the one that went")
		assert.LessOrEqual(t, c.bytes(), c.max)
	})

	t.Run("a text too big to share the budget is rendered and not kept", func(t *testing.T) {
		var work counting
		c := newCache(10_000)
		big := strings.Repeat("x", 5_000) // more than a quarter of the budget

		c.get(big, work.render)
		c.get(big, work.render)

		assert.EqualValues(t, 2, work.calls.Load())
		assert.Zero(t, c.len())
	})

	t.Run("many readers at once agree", func(t *testing.T) {
		var work counting
		c := newCache(1 << 20)
		var wg sync.WaitGroup
		for i := range 32 {
			wg.Go(func() {
				for range 200 {
					text := string(rune('a' + i%7))
					assert.Equal(t, template.HTML("<p>"+text+"</p>"), c.get(text, work.render))
				}
			})
		}
		wg.Wait()

		assert.Equal(t, 7, c.len())
	})
}

func TestMarkdown_RenderFinal(t *testing.T) {
	md := NewMarkdown()
	src := "# Title\n\nSome **bold** text and `code`.\n\n```go\nfmt.Println(1)\n```\n"

	first := md.RenderFinal(src)
	second := md.RenderFinal(src)

	assert.Equal(t, md.Render(src), first, "it is the same HTML as rendering it afresh")
	assert.Equal(t, first, second)
	assert.Equal(t, 1, md.Cached(), "and the work is kept")

	t.Run("text that is still being written is not kept", func(t *testing.T) {
		md := NewMarkdown()

		md.Render("half a sentence, still comi")

		assert.Zero(t, md.Cached())
	})
}

func BenchmarkRenderFinal(b *testing.B) {
	md := NewMarkdown()
	src := strings.Repeat("## A heading\n\nA paragraph with **bold** and a [link](https://example.com).\n\n- one\n- two\n\n```go\nfmt.Println(1)\n```\n\n", 40)
	b.Run("first view", func(b *testing.B) {
		for b.Loop() {
			NewMarkdown().RenderFinal(src)
		}
	})
	b.Run("later views", func(b *testing.B) {
		md.RenderFinal(src)
		for b.Loop() {
			md.RenderFinal(src)
		}
	})
}
