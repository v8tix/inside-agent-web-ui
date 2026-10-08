package contract_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/contract"
)

// The contract exists so that a template change cannot silently break what reads
// the page. A hook that the evaluator or the browser tests name but the contract
// does not list is outside it: renaming that one would pass the Web UI's suite
// and show up only in a slow browser run. So every test id they name has to be
// in the contract.
func TestEveryHookTheConsumersNameIsInTheContract(t *testing.T) {
	listed := map[string]bool{}
	for _, h := range contract.Hooks {
		listed[h.TestID] = true
	}

	// How a test id is written in scripts and Go code: [data-testid=name], or the
	// name passed on as a Go string where the code walks the page by Tab. A
	// selector that is built from a variable names a hook that this scan cannot
	// see, so a consumer writes each of its hooks out.
	selector := regexp.MustCompile(`data-testid=([a-z][a-z0-9-]*)\b`)
	tabbed := regexp.MustCompile(`tabTo\(ctx, "[^"]+", "([a-z][a-z0-9-]*)"\)`)
	built := regexp.MustCompile(`data-testid=\$\{`)

	consumers := []string{
		"../eval/infrastructure/devtools", // the evaluator's driver and its page scripts
		"../../it",                        // the browser and integration tests
	}
	named := map[string][]string{} // test id -> where it is named
	for _, root := range consumers {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || (!strings.HasSuffix(path, ".js") && !strings.HasSuffix(path, ".go")) {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			assert.False(t, built.Match(src), "%s builds a data-testid selector from a variable: write the hooks out, so that they are checked here", filepath.ToSlash(path))
			for _, re := range []*regexp.Regexp{selector, tabbed} {
				for _, m := range re.FindAllStringSubmatch(string(src), -1) {
					named[m[1]] = append(named[m[1]], filepath.ToSlash(path))
				}
			}
			return nil
		})
		require.NoError(t, err)
	}
	// Ids it must find, one way of writing each: if the patterns or the places stop
	// matching, the loop below would pass for want of anything to check. The two
	// buttons of a sidebar row are among them: one script reads either.
	for _, known := range []string{"message", "composer-input", "stop", "rename", "delete"} {
		require.Contains(t, named, known, "the scan did not find %q: it is looking in the wrong place", known)
	}

	for id, where := range named {
		slices.Sort(where)
		assert.True(t, listed[id], "data-testid=%q is read by %s but is not in contract.Hooks", id, strings.Join(slices.Compact(where), ", "))
	}
}
