package web

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
)

// ErrUnknownPage is returned when no template exists for a requested page.
var ErrUnknownPage = errors.New("unknown page")

// Renderer renders html/template pages. Each page is the base layout, all
// partials, and one page file; pages define "page:title" and "page:main". Every
// request for a page gets the whole document: what htmx swaps in by itself are
// the partials (see Block).
type Renderer struct {
	pages    map[string]*template.Template
	partials *template.Template
}

// NewRenderer parses every page under templates/pages in fsys at startup, so a
// broken template fails the boot instead of a request.
func NewRenderer(fsys fs.FS) (*Renderer, error) {
	pageFiles, err := fs.Glob(fsys, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	if len(pageFiles) == 0 {
		return nil, errors.New("no page templates found under templates/pages")
	}
	partialFiles, err := fs.Glob(fsys, "templates/partials/*.html")
	if err != nil {
		return nil, err
	}

	rd := &Renderer{pages: make(map[string]*template.Template, len(pageFiles))}
	for _, pageFile := range pageFiles {
		files := append([]string{"templates/base.html"}, partialFiles...)
		files = append(files, pageFile)
		set, err := template.New("base.html").ParseFS(fsys, files...)
		if err != nil {
			return nil, fmt.Errorf("parsing page %s: %w", pageFile, err)
		}
		rd.pages[pageName(pageFile)] = set
	}
	if len(partialFiles) > 0 {
		if rd.partials, err = template.New("partials").ParseFS(fsys, partialFiles...); err != nil {
			return nil, fmt.Errorf("parsing partials: %w", err)
		}
	}
	return rd, nil
}

func pageName(file string) string {
	base := file[len("templates/pages/"):]
	return base[:len(base)-len(".html")]
}

// Page renders the named page with status. Output is buffered, so a template
// error leaves the response untouched and the caller can answer with a 500.
func (rd *Renderer) Page(w http.ResponseWriter, r *http.Request, status int, page string, data any) error {
	set, ok := rd.pages[page]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownPage, page)
	}
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, "base.html", data); err != nil {
		return fmt.Errorf("rendering page %q: %w", page, err)
	}
	sendHTML(w, r, status, buf.Bytes())
	return nil
}

// Block renders one named partial to a string, for responses that are
// fragments by nature (server-sent events, htmx swaps).
func (rd *Renderer) Block(name string, data any) (string, error) {
	if rd.partials == nil {
		return "", fmt.Errorf("no partial named %q", name)
	}
	var buf bytes.Buffer
	if err := rd.partials.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("rendering partial %q: %w", name, err)
	}
	return buf.String(), nil
}
