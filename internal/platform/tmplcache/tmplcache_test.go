package tmplcache_test

import (
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/platform/tmplcache"
)

// files is a file system with one file for each name.
func files(texts map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, text := range texts {
		fsys[name] = &fstest.MapFile{Data: []byte(text)}
	}
	return fsys
}

// failing is a file system in which one name cannot be opened.
type failing struct {
	inner fs.FS
	name  string
	err   error
}

func (f failing) Open(name string) (fs.File, error) {
	if name == f.name {
		return nil, &fs.PathError{Op: "open", Path: name, Err: f.err}
	}
	return f.inner.Open(name)
}

func TestPreload_ReadsEveryTemplateByItsPath(t *testing.T) {
	cache, err := tmplcache.Preload(files(map[string]string{
		"top.gotmpl":             "SELECT 3",
		"user/by_id.gotmpl":      "SELECT 1",
		"user/deep/count.gotmpl": "SELECT 2",
		// What stands beside the templates is left alone.
		"user/README.md":    "{{ not a template",
		"user/by_id.sql":    "{{ nor this",
		"notes.gotmpl.bak":  "{{ nor this",
		"gotmpl":            "{{ nor this",
		"user/.gotmpl.swp":  "{{ nor this",
		"user/nosuffixtmpl": "{{ nor this",
	}), nil)

	require.NoError(t, err)
	assert.Equal(t, []string{"top.gotmpl", "user/by_id.gotmpl", "user/deep/count.gotmpl"}, cache.Paths())
}

func TestExecute(t *testing.T) {
	cache, err := tmplcache.Preload(files(map[string]string{
		"plain.gotmpl":         "SELECT count(*) FROM users\n",
		"placeholder.gotmpl":   "UPDATE messages SET meta = $2::jsonb WHERE id = $1",
		"filled.gotmpl":        "Question: {{.Question}}",
		"with_part.gotmpl":     "SELECT {{template \"part/columns.gotmpl\"}} FROM users WHERE id = $1",
		"part/columns.gotmpl":  "id, email",
		"trimmed.gotmpl":       "SELECT {{template \"part/line.gotmpl\" -}}\nFROM users",
		"part/line.gotmpl":     "id, email\n",
		"passes_data.gotmpl":   "{{template \"part/greeting.gotmpl\" .}}!",
		"part/greeting.gotmpl": "Hello, {{.Name}}",
		"calls.gotmpl":         "{{upper .Name}}",
		"comment.gotmpl":       "{{- /* what the statement is for */ -}}\nSELECT 1\n",
	}), template.FuncMap{"upper": strings.ToUpper})
	require.NoError(t, err)

	tests := []struct {
		name string
		path string
		data any
		want string
	}{
		{"a text with nothing to fill in comes out as it is", "plain.gotmpl", nil, "SELECT count(*) FROM users\n"},
		{"a placeholder of the database is plain text", "placeholder.gotmpl", nil, "UPDATE messages SET meta = $2::jsonb WHERE id = $1"},
		{"a field of the data is filled in", "filled.gotmpl", struct{ Question string }{"Why?"}, "Question: Why?"},
		{"a template includes another by its path", "with_part.gotmpl", nil, "SELECT id, email FROM users WHERE id = $1"},
		{"a trim marker takes the line end after an include", "trimmed.gotmpl", nil, "SELECT id, email\nFROM users"},
		{"a template hands its data on", "passes_data.gotmpl", map[string]string{"Name": "Ada"}, "Hello, Ada!"},
		{"a function of the caller is called", "calls.gotmpl", map[string]string{"Name": "ada"}, "ADA"},
		{"a comment of the template is not in the text", "comment.gotmpl", nil, "SELECT 1\n"},
		{"a part is a template too", "part/columns.gotmpl", nil, "id, email"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cache.Execute(tc.path, tc.data)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestExecute_Errors(t *testing.T) {
	errBoom := errors.New("boom")
	cache, err := tmplcache.Preload(files(map[string]string{
		"needs_key.gotmpl":     "LIMIT {{.Limit}}",
		"fails.gotmpl":         "before {{fail}} after",
		"no_such_part.gotmpl":  "SELECT {{template \"part/nowhere.gotmpl\"}} FROM users",
		"part/columns.gotmpl":  "id, email",
		"user/by_email.gotmpl": "SELECT 1",
	}), template.FuncMap{"fail": func() (string, error) { return "", errBoom }})
	require.NoError(t, err)

	tests := []struct {
		name  string
		path  string
		data  any
		is    error  // what errors.Is finds in the error, when not nil
		names string // what the text of the error names
	}{
		{"a path that no file has", "user/by_id.gotmpl", nil, tmplcache.ErrNotFound, "user/by_id.gotmpl"},
		{"a path without its folder", "by_email.gotmpl", nil, tmplcache.ErrNotFound, "by_email.gotmpl"},
		{"the empty path", "", nil, tmplcache.ErrNotFound, `""`},
		{"a key that the data does not have", "needs_key.gotmpl", map[string]int{"Offset": 3}, nil, "needs_key.gotmpl"},
		{"a function that fails", "fails.gotmpl", nil, errBoom, "fails.gotmpl"},
		{"an include of a template that is not there", "no_such_part.gotmpl", nil, nil, "part/nowhere.gotmpl"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cache.Execute(tc.path, tc.data)

			require.Error(t, err)
			assert.Empty(t, got, "a text that stopped halfway is not handed out")
			assert.Contains(t, err.Error(), tc.names)
			if tc.is != nil {
				assert.ErrorIs(t, err, tc.is)
			}
		})
	}
}

func TestPreload_Errors(t *testing.T) {
	errDisk := errors.New("the disk is gone")
	good := files(map[string]string{"user/by_id.gotmpl": "SELECT 1", "user/count.gotmpl": "SELECT 2"})

	tests := []struct {
		name  string
		fsys  fs.FS
		is    error
		names string
	}{
		{"a file system with no template", files(map[string]string{"README.md": "nothing here", "user/by_id.sql": "SELECT 1"}),
			tmplcache.ErrNoTemplates, tmplcache.Suffix},
		{"an empty file system", fstest.MapFS{}, tmplcache.ErrNoTemplates, tmplcache.Suffix},
		{"a template that does not parse", files(map[string]string{"user/by_id.gotmpl": "SELECT 1", "user/broken.gotmpl": "SELECT {{.ID"}),
			nil, "user/broken.gotmpl"},
		{"a function that the caller did not give", files(map[string]string{"user/calls.gotmpl": "{{upper .Name}}"}),
			nil, "user/calls.gotmpl"},
		{"a file that cannot be read", failing{inner: good, name: "user/count.gotmpl", err: errDisk}, errDisk, "user/count.gotmpl"},
		{"a folder that cannot be listed", failing{inner: good, name: "user", err: errDisk}, errDisk, "user"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cache, err := tmplcache.Preload(tc.fsys, nil)

			require.Error(t, err)
			assert.Nil(t, cache)
			assert.Contains(t, err.Error(), tc.names)
			if tc.is != nil {
				assert.ErrorIs(t, err, tc.is)
			}
		})
	}
}

// A cache is written once and read by every request after that.
func TestExecute_FromManyGoroutines(t *testing.T) {
	cache, err := tmplcache.Preload(files(map[string]string{
		"filled.gotmpl":       "SELECT {{template \"part/columns.gotmpl\"}} FROM users LIMIT {{.Limit}}",
		"part/columns.gotmpl": "id, email",
	}), nil)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			got, err := cache.Execute("filled.gotmpl", map[string]int{"Limit": i})

			assert.NoError(t, err)
			assert.Equal(t, "SELECT id, email FROM users LIMIT "+strconv.Itoa(i), got)
		})
	}
	wg.Wait()
}

func TestPaths_IsTheCallersToChange(t *testing.T) {
	cache, err := tmplcache.Preload(files(map[string]string{"a.gotmpl": "A", "b.gotmpl": "B"}), nil)
	require.NoError(t, err)

	paths := cache.Paths()
	paths[0] = "changed"

	assert.Equal(t, []string{"a.gotmpl", "b.gotmpl"}, cache.Paths())
}

func TestTheZeroCacheHoldsNothing(t *testing.T) {
	var cache tmplcache.Cache

	_, err := cache.Execute("a.gotmpl", nil)

	assert.ErrorIs(t, err, tmplcache.ErrNotFound)
	assert.Empty(t, cache.Paths())
}
