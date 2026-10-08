// Package deprule keeps an import rule as data, so that a test can enforce it.
package deprule

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
)

// Rule says, in the sentence What, that no Go file in Dir imports a path that starts with Banned.
type Rule struct{ What, Dir, Banned string }

// Check returns a line for each import that breaks the rule. A rule whose Dir
// holds no Go file gets a line too: a rule that reads nothing would pass for ever.
func (r Rule) Check() (problems []string) {
	files, _ := filepath.Glob(filepath.Join(r.Dir, "*.go"))
	if len(files) == 0 {
		return []string{"no file falls under: " + r.What}
	}
	for _, f := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			return append(problems, err.Error())
		}
		for _, imp := range parsed.Imports {
			if path := strings.Trim(imp.Path.Value, "\"`"); strings.HasPrefix(path, r.Banned) {
				problems = append(problems, f+" imports "+path+", but "+r.What)
			}
		}
	}
	return problems
}
