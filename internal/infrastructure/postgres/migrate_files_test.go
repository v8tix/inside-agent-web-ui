package postgres

import (
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The embedded migrations are code: a missing down file or a gap in the
// numbering only shows up in production unless a test looks for it.
func TestMigrationFiles(t *testing.T) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	name := regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.(up|down)\.sql$`)
	type pair struct{ up, down bool }
	versions := map[int]*pair{}
	titles := map[int]string{}

	for _, e := range entries {
		m := name.FindStringSubmatch(e.Name())
		if !assert.NotNil(t, m, "bad migration file name: %s", e.Name()) {
			continue
		}
		version, _ := strconv.Atoi(m[1])
		if versions[version] == nil {
			versions[version] = &pair{}
			titles[version] = m[2]
		}
		assert.Equal(t, titles[version], m[2], "up and down of version %d must share a title", version)
		if m[3] == "up" {
			versions[version].up = true
		} else {
			versions[version].down = true
		}

		body, err := fs.ReadFile(migrationFiles, "migrations/"+e.Name())
		require.NoError(t, err)
		assert.NotEmpty(t, body, "%s is empty", e.Name())
	}

	var ordered []int
	for v, p := range versions {
		assert.True(t, p.up, "version %d has no up migration", v)
		assert.True(t, p.down, "version %d has no down migration", v)
		ordered = append(ordered, v)
	}
	slices.Sort(ordered)
	for i, v := range ordered {
		assert.Equal(t, i+1, v, "versions must be contiguous from 1")
	}
}
