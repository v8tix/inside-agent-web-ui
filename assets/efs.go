// Package assets embeds the HTML templates and static files into the binary.
package assets

import "embed"

// EmbeddedFiles holds templates/ and static/.
//
//go:embed templates static
var EmbeddedFiles embed.FS
