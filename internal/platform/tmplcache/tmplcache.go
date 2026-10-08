// Package tmplcache keeps texts that are no Go code in files of their own, the
// SQL of a store or a prompt, and hands them out filled in. The files are
// templates. A program reads and parses all of them once, when it starts, so a
// text that does not parse stops the start and not a request, and it looks them
// up by their path afterwards. The package knows nothing of this module: the
// caller says which files, and what each is filled in with.
package tmplcache

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"text/template"
)

// Suffix is what the name of a template file ends with. Preload leaves every
// other file alone.
const Suffix = ".gotmpl"

var (
	// ErrNoTemplates is returned by Preload for a file system that holds no template.
	ErrNoTemplates = errors.New("no template files found")
	// ErrNotFound is returned by Execute for a path that no template has.
	ErrNotFound = errors.New("template not found")
)

// Cache holds the parsed templates of one file system by their path in it.
// Preload writes it once and nothing changes it afterwards, so any number of
// goroutines may execute its templates at the same time.
type Cache struct {
	byPath map[string]*template.Template
}

// Preload reads every file of fsys whose name ends with Suffix, at any depth,
// and parses them together, each under its path: one template can include
// another with {{template "user/columns.gotmpl" .}}. funcs are the functions
// the templates may call; nil means none. A template that names a key its data
// does not have fails when it is executed, and so does one that includes a
// template that is not there.
func Preload(fsys fs.FS, funcs template.FuncMap) (*Cache, error) {
	set := template.New("").Option("missingkey=error").Funcs(funcs)
	byPath := map[string]*template.Template{}
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("listing %s: %w", path, err)
		}
		if d.IsDir() || !strings.HasSuffix(path, Suffix) {
			return nil
		}
		text, err := fs.ReadFile(fsys, path)
		if err != nil {
			return fmt.Errorf("reading the template %s: %w", path, err)
		}
		tmpl, err := set.New(path).Parse(string(text))
		if err != nil {
			return fmt.Errorf("parsing the template %s: %w", path, err)
		}
		byPath[path] = tmpl
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(byPath) == 0 {
		return nil, fmt.Errorf("%w: no file name ends with %s", ErrNoTemplates, Suffix)
	}
	return &Cache{byPath: byPath}, nil
}

// Execute fills in the template at path with data and returns the text. A text
// that stops halfway is not returned.
func (c *Cache) Execute(path string, data any) (string, error) {
	tmpl, ok := c.byPath[path]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNotFound, path)
	}
	var text strings.Builder
	if err := tmpl.Execute(&text, data); err != nil {
		return "", fmt.Errorf("filling in the template %s: %w", path, err)
	}
	return text.String(), nil
}

// Paths lists the templates of the cache, sorted.
func (c *Cache) Paths() []string {
	return slices.Sorted(maps.Keys(c.byPath))
}
