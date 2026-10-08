package render

import "github.com/yuin/goldmark"

// MaxRenderBytes is maxRenderBytes, for the test that holds it to the size at
// which a reply is cut off.
const MaxRenderBytes = maxRenderBytes

// SetConverter puts md in the place of the Markdown converter, for the test of a
// conversion that fails.
func (m *Markdown) SetConverter(md goldmark.Markdown) { m.md = md }
