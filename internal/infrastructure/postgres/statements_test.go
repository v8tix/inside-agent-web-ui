package postgres

import (
	"io/fs"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/platform/tmplcache"
)

// sqlFilesCopy is the embedded SQL in a file system that a test can change.
func sqlFilesCopy(t *testing.T) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	require.NoError(t, fs.WalkDir(sqlFiles, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		text, err := fs.ReadFile(sqlFiles, path)
		fsys[path] = &fstest.MapFile{Data: text}
		return err
	}))
	return fsys
}

// The SQL of the stores is text that Go does not compile. A statement that is
// empty, cut short or misnumbered only shows up at the first query, in a
// database, unless a test looks at it.
func TestLoadStatements(t *testing.T) {
	st, err := LoadStatements()
	require.NoError(t, err)

	var (
		verb        = regexp.MustCompile(`^(SELECT|INSERT|UPDATE|DELETE) `)
		placeholder = regexp.MustCompile(`\$(\d+)`)
	)
	templates := st.templates()
	require.NotEmpty(t, templates)
	for _, path := range slices.Sorted(maps.Keys(templates)) {
		text := *templates[path]
		t.Run(path, func(t *testing.T) {
			assert.Regexp(t, verb, text, "a statement starts with what it does")
			assert.Equal(t, strings.TrimSpace(text), text)
			assert.NotContains(t, text, "{{", "the template is filled in")
			assert.NotContains(t, text, "}}", "the template is filled in")
			assert.NotContains(t, text, "no value", "nothing that a template names is missing")
			assert.NotContains(t, text, ";", "one statement, and the driver ends it")
			assert.NotContains(t, text, "\n\n", "no empty line where a part was included")

			// The arguments of a query are counted from $1 with none left out.
			numbers := map[int]bool{}
			for _, m := range placeholder.FindAllStringSubmatch(text, -1) {
				n, err := strconv.Atoi(m[1])
				require.NoError(t, err)
				numbers[n] = true
			}
			for n := 1; n <= len(numbers); n++ {
				assert.True(t, numbers[n], "$%d is missing: the placeholders are %v", n, slices.Sorted(maps.Keys(numbers)))
			}
		})
	}
}

// A statement that no template fills would be sent empty, and two templates for
// one statement would leave one of them unused.
func TestStatements_EveryStatementHasOneTemplate(t *testing.T) {
	st, err := LoadStatements()
	require.NoError(t, err)

	filled := 0
	stores := reflect.ValueOf(st)
	for i := range stores.NumField() {
		store := stores.Field(i)
		for j := range store.NumField() {
			name := stores.Type().Field(i).Name + "." + store.Type().Field(j).Name
			require.Equal(t, reflect.String, store.Field(j).Kind(), "%s is a statement", name)
			assert.NotEmpty(t, store.Field(j).String(), "no template fills %s", name)
			filled++
		}
	}

	templates := st.templates()
	fields := map[*string]bool{}
	for _, field := range templates {
		fields[field] = true
	}
	assert.Len(t, fields, len(templates), "each template fills a statement of its own")
	assert.Equal(t, filled, len(templates), "as many templates as statements")
}

// A file that nothing reads is SQL that looks as if the program ran it.
func TestSQLFiles_EachIsAStatementOrAPartOfOne(t *testing.T) {
	var st Statements
	templates := st.templates()
	include := regexp.MustCompile(`\{\{-? ?template "([^"]+)"`)

	parts := map[string]bool{}
	var all []string
	require.NoError(t, fs.WalkDir(sqlFiles, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		all = append(all, path)
		text, err := fs.ReadFile(sqlFiles, path)
		for _, m := range include.FindAllStringSubmatch(string(text), -1) {
			parts[m[1]] = true
		}
		return err
	}))
	require.NotEmpty(t, all)
	require.NotEmpty(t, parts, "the statements of a store share their column list")

	for _, path := range all {
		assert.True(t, strings.HasSuffix(path, tmplcache.Suffix), "%s is no template: the cache would leave it out", path)
		_, statement := templates[path]
		assert.True(t, statement != parts[path], "%s is a statement or a part of one, and not both (statement: %t, part: %t)",
			path, statement, parts[path])
	}
	for path := range parts {
		assert.Contains(t, all, path, "a template includes %s, which is no file", path)
	}
}

func TestLoadStatements_Errors(t *testing.T) {
	tests := []struct {
		name   string
		change func(fstest.MapFS)
		is     error
		names  string
	}{
		{"a statement without its file", func(f fstest.MapFS) { delete(f, "sql/session/get.gotmpl") },
			tmplcache.ErrNotFound, "sql/session/get.gotmpl"},
		{"a column list without its file", func(f fstest.MapFS) { delete(f, "sql/user/columns.gotmpl") },
			nil, "sql/user/columns.gotmpl"},
		{"a file that does not parse", func(f fstest.MapFS) { f["sql/user/count.gotmpl"] = &fstest.MapFile{Data: []byte("SELECT {{")} },
			nil, "sql/user/count.gotmpl"},
		{"no file at all", func(f fstest.MapFS) { clear(f) }, tmplcache.ErrNoTemplates, tmplcache.Suffix},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files := sqlFilesCopy(t)
			tc.change(files)

			st, err := loadStatements(files)

			require.Error(t, err)
			assert.Equal(t, Statements{}, st, "half of the statements are not handed out")
			assert.Contains(t, err.Error(), "the SQL of the stores")
			assert.Contains(t, err.Error(), tc.names)
			if tc.is != nil {
				assert.ErrorIs(t, err, tc.is)
			}
		})
	}

	t.Run("and the copy as it is loads", func(t *testing.T) {
		st, err := loadStatements(sqlFilesCopy(t))

		require.NoError(t, err)
		want, err := LoadStatements()
		require.NoError(t, err)
		assert.Equal(t, want, st)
	})
}
