package evalenv

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNodeIsNewEnough(t *testing.T) {
	tests := []struct {
		name    string
		version string // as node --version prints it
		want    bool
	}{
		{"the last 20 that is too old", "v20.18.0", false},
		{"the first 20 that will do", "v20.19.0", true},
		{"a later 20", "v20.20.1", true},
		{"21, which the package does not name", "v21.7.3", false},
		{"the last 22 that is too old", "v22.11.0", false},
		{"the first 22 that will do", "v22.12.0", true},
		{"23", "v23.0.0", true},
		{"a later one", "v26.10.0", true},
		{"18", "v18.20.8", false},
		{"without the v and the patch number", "26.10", true},
		{"and too old, written that way", "21.7", false},
		{"a major number alone", "v22", false},
		{"not a version", "node: command not found", false},
		{"nothing", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, nodeIsNewEnough(tc.version), tc.version)
		})
	}
}
