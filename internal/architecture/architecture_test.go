// Package architecture_test holds the rules about which package may import which.
// The two programs, the Web UI and the evaluator, are each built as a hexagon:
// the use cases at the middle know nothing of HTTP, SQL, browsers or files, and
// the adapters around them plug into the ports the use cases define. Neither
// program knows the other exists. A change that breaks one of these rules fails
// here, with the file and the import.
//
// The package holds two more rules that only reading the code can keep, both
// about secrets: a Secret sits in an exported struct field, where it can be
// asked how it prints, and no string of a production file holds the verb %p,
// under which fmt prints a Secret without asking it.
package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rule says what the packages under some directories may import. Directories
// are relative to the repository, with slashes. The standard library is theirs
// to import, but for the packages a rule names.
//
// Pattern: Dependency Rule as a Test
type rule struct {
	what string
	// under lists the directories the rule is about, each with everything below it.
	under []string
	// internal lists the directories of this module they may import.
	internal []string
	// thirdParty says whether they may import other modules.
	thirdParty bool
	// noStd lists the packages of the standard library they may not import,
	// each with everything below it.
	noStd []string
}

// outside is the part of the standard library that reaches out of the process:
// the network, the database, the operating system and its files. It is for the
// adapters, and no use case imports it.
var outside = []string{"net/http", "database/sql", "os", "io/fs"}

var rules = []rule{
	{what: "the Web UI's use cases know the domain and the ports: not each other, and nothing of HTTP, SQL or files",
		under: []string{"internal/core"}, internal: []string{"internal/core/domain", "internal/core/port"}, noStd: outside},
	{what: "an adapter of the Web UI plugs into the ports, and knows no use case",
		under:    []string{"internal/infrastructure"},
		internal: []string{"internal/core/domain", "internal/core/port", "internal/infrastructure", "internal/platform"}, thirdParty: true},
	{what: "the Web UI is put together from its own parts",
		under:    []string{"internal/app", "cmd/web", "assets"},
		internal: []string{"internal/core", "internal/infrastructure", "internal/platform", "internal/app", "assets"}, thirdParty: true},

	{what: "the evaluator's use cases know only themselves, and nothing of HTTP, SQL or files",
		under: []string{"internal/eval/core"}, internal: []string{"internal/eval/core"}, noStd: outside},
	{what: "the evaluator's adapters plug into its use cases",
		under: []string{"internal/eval/infrastructure"}, internal: []string{"internal/eval", "internal/platform"}, thirdParty: true},
	{what: "the evaluator is put together from its own parts",
		under: []string{"cmd/eval"}, internal: []string{"internal/eval", "internal/platform"}, thirdParty: true},

	{what: "what both programs share knows neither of them",
		under: []string{"internal/platform"}, thirdParty: true},
	{what: "the view of a struct for a log is made with the standard library alone",
		under: []string{"internal/platform/redact"}},
	{what: "the scripted agent and the UI contract know neither program",
		under: []string{"internal/testagent", "internal/contract"}, thirdParty: true},
	{what: "the scripted agent's command is the scripted agent, with the settings every program reads",
		under: []string{"cmd/test-agent"}, internal: []string{"internal/testagent", "internal/platform"}, thirdParty: true},
}

func under(dir, parent string) bool { return dir == parent || strings.HasPrefix(dir, parent+"/") }

func TestDependencies(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	module := moduleOf(t, root)
	files := sourceFiles(t, root)
	require.NotEmpty(t, files)

	for _, f := range files {
		imports := importsOf(t, filepath.Join(root, f))
		rules := rulesFor(f)
		if !assert.NotEmpty(t, rules, "%s is in no rule: say what its package may import", f) {
			continue
		}
		for _, imp := range imports {
			for _, r := range rules {
				assert.Truef(t, allowed(imp, module, r), "%s imports %s, but %s", f, imp, r.what)
			}
		}
	}
}

// The rules themselves, on imports made for them: what the sentences of this
// package and of the documents promise to refuse is refused, and what the
// programs are built from is let through. TestDependencies cannot tell: a rule
// that is looser than its sentence passes it for as long as no file uses the
// room.
func TestTheRulesCatchWhatTheyForbid(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	module := moduleOf(t, root)
	ours := func(dir string) string { return module + "/" + dir }

	const (
		webUseCase   = "a use case of the Web UI"
		webAdapter   = "an adapter of the Web UI"
		webRoot      = "the composition root of the Web UI"
		evalUseCase  = "a use case of the evaluator"
		evalAdapter  = "an adapter of the evaluator"
		evalCommand  = "the evaluator's command"
		shared       = "what both programs share"
		agentCommand = "the scripted agent's command"
	)
	type importCase struct {
		who  string
		from string // the directory of the package that imports
		what string
		imp  string
		may  bool // whether every rule for the package lets the import through
	}
	tests := []importCase{
		{webUseCase, "internal/core/chat", "the domain", ours("internal/core/domain"), true},
		{webUseCase, "internal/core/chat", "a port", ours("internal/core/port/resource"), true},
		{webUseCase, "internal/core/chat", "another use case", ours("internal/core/conversation"), false},
		{webUseCase, "internal/core/chat", "an adapter", ours("internal/infrastructure/postgres"), false},
		{webUseCase, "internal/core/auth", "the composition root", ours("internal/app"), false},
		{webUseCase, "internal/core/auth", "what both programs share", ours("internal/platform/envcfg"), false},
		{webUseCase, "internal/core/domain", "another module", "github.com/jackc/pgx/v5", false},
		{webUseCase, "internal/core/conversation", "the evaluator", ours("internal/eval/core/domain"), false},

		{webAdapter, "internal/infrastructure/postgres", "the domain", ours("internal/core/domain"), true},
		{webAdapter, "internal/infrastructure/api/web", "a port", ours("internal/core/port/service"), true},
		{webAdapter, "internal/infrastructure/agent/adkrest", "the settings, which the adapters share", ours("internal/infrastructure/config"), true},
		{webAdapter, "internal/infrastructure/config", "what both programs share", ours("internal/platform/redact"), true},
		{webAdapter, "internal/infrastructure/postgres", "another module", "github.com/jackc/pgx/v5", true},
		{webAdapter, "internal/infrastructure/api/web", "a use case", ours("internal/core/chat"), false},
		{webAdapter, "internal/infrastructure/api/web", "the composition root", ours("internal/app"), false},
		{webAdapter, "internal/infrastructure/config", "the evaluator", ours("internal/eval/infrastructure/config"), false},

		{webRoot, "internal/app", "a use case", ours("internal/core/chat"), true},
		{webRoot, "internal/app", "an adapter", ours("internal/infrastructure/postgres"), true},
		{webRoot, "internal/app", "the embedded files", ours("assets"), true},
		{webRoot, "cmd/web", "the evaluator", ours("internal/eval/infrastructure/config"), false},

		{evalUseCase, "internal/eval/core/run", "the domain", ours("internal/eval/core/domain"), true},
		{evalUseCase, "internal/eval/core/probe", "a port", ours("internal/eval/core/port/resource"), true},
		{evalUseCase, "internal/eval/core/run", "another use case", ours("internal/eval/core/report"), true},
		{evalUseCase, "internal/eval/core/run", "an adapter", ours("internal/eval/infrastructure/store"), false},
		{evalUseCase, "internal/eval/core/run", "what both programs share", ours("internal/platform/envcfg"), false},
		{evalUseCase, "internal/eval/core/score", "another module", "github.com/parquet-go/parquet-go", false},
		{evalUseCase, "internal/eval/core/domain", "the Web UI", ours("internal/core/domain"), false},

		{evalAdapter, "internal/eval/infrastructure/devtools", "a use case", ours("internal/eval/core/probe"), true},
		{evalAdapter, "internal/eval/infrastructure/synthetic", "another adapter", ours("internal/eval/infrastructure/gaia"), true},
		{evalAdapter, "internal/eval/infrastructure/config", "what both programs share", ours("internal/platform/envcfg"), true},
		{evalAdapter, "internal/eval/infrastructure/devtools", "another module", "github.com/modelcontextprotocol/go-sdk/mcp", true},
		{evalAdapter, "internal/eval/infrastructure/devtools", "the Web UI", ours("internal/infrastructure/config"), false},
		{evalAdapter, "internal/eval/infrastructure/devtools", "the UI contract", ours("internal/contract"), false},
		{evalCommand, "cmd/eval", "a use case", ours("internal/eval/core/run"), true},
		{evalCommand, "cmd/eval", "what both programs share", ours("internal/platform/envcfg"), true},
		{evalCommand, "cmd/eval", "the Web UI", ours("internal/core/domain"), false},

		{shared, "internal/platform/envcfg", "another module", "github.com/joho/godotenv", true},
		{shared, "internal/platform/envcfg", "the Web UI", ours("internal/core/domain"), false},
		{shared, "internal/platform/redact", "the evaluator", ours("internal/eval/core/domain"), false},
		{"the view of a struct", "internal/platform/redact", "the standard library", "reflect", true},
		{"the view of a struct", "internal/platform/redact", "another module", "github.com/joho/godotenv", false},
		{"the view of a struct", "internal/platform/redact", "the settings reader beside it", ours("internal/platform/envcfg"), false},
		{"the UI contract", "internal/contract", "the Web UI", ours("internal/infrastructure/api/web"), false},
		{"the scripted agent", "internal/testagent", "the evaluator", ours("internal/eval/core/domain"), false},
		{agentCommand, "cmd/test-agent", "the scripted agent", ours("internal/testagent"), true},
		{agentCommand, "cmd/test-agent", "the Web UI", ours("internal/infrastructure/config"), false},

		{webAdapter, "internal/infrastructure/api/web", "net/http", "net/http", true},
		{webAdapter, "internal/infrastructure/postgres", "database/sql", "database/sql", true},
		{evalAdapter, "internal/eval/infrastructure/store", "os", "os", true},
	}
	// Of the standard library a core may import all but the packages that reach
	// out of the process, and what is below them.
	for _, core := range []importCase{{who: webUseCase, from: "internal/core/chat"}, {who: evalUseCase, from: "internal/eval/core/run"}} {
		for _, std := range []string{"net/http", "net/http/httputil", "database/sql", "database/sql/driver", "os", "os/exec", "io/fs"} {
			tests = append(tests, importCase{core.who, core.from, std, std, false})
		}
		for _, std := range []string{"context", "io", "net/url", "net/mail", "embed", "log/slog", "encoding/json/v2"} {
			tests = append(tests, importCase{core.who, core.from, std, std, true})
		}
	}
	for _, tc := range tests {
		verb := map[bool]string{true: " may import ", false: " may not import "}[tc.may]
		t.Run(tc.who+verb+tc.what, func(t *testing.T) {
			var refusedBy []string
			for _, r := range rulesFor(tc.from + "/file.go") {
				if !allowed(tc.imp, module, r) {
					refusedBy = append(refusedBy, r.what)
				}
			}
			if tc.may {
				assert.Empty(t, refusedBy, "%s is kept from importing %s", tc.from, tc.imp)
				return
			}
			assert.NotEmpty(t, refusedBy, "no rule keeps %s from importing %s", tc.from, tc.imp)
		})
	}
}

// The rules are about whole programs, so a directory that a rule names and
// that holds no production file is one that is gone, or has another name now:
// the rule would go on passing, about nothing.
func TestEveryRuleStillApplies(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	files := sourceFiles(t, root)

	for _, r := range rules {
		assert.Empty(t, r.gone(files), "no production file is under these directories of the rule %q", r.what)
	}
}

// The check of TestEveryRuleStillApplies, on a rule made for it: a directory
// that is gone is found next to one that is there.
func TestEveryRuleStillApplies_FindsADirectoryThatIsGone(t *testing.T) {
	files := []string{"internal/app/app.go", "cmd/web/main.go"}
	r := rule{under: []string{"internal/app", "cmd/gone"}, internal: []string{"cmd/web", "internal/gone"}}

	assert.Equal(t, []string{"cmd/gone", "internal/gone"}, r.gone(files))
	assert.Empty(t, rule{under: []string{"internal/app"}, internal: []string{"cmd"}}.gone(files), "a directory above a file holds it")
}

// gone lists the directories the rule names that hold no production file: the
// ones it is about, and the ones it lets them import.
func (r rule) gone(files []string) []string {
	var out []string
	for _, dir := range slices.Concat(r.under, r.internal) {
		if !slices.ContainsFunc(files, func(f string) bool { return under(path.Dir(f), dir) }) {
			out = append(out, dir)
		}
	}
	return out
}

// covers reports whether the rule is about the file's package.
func (r rule) covers(file string) bool {
	dir := path.Dir(file)
	return slices.ContainsFunc(r.under, func(p string) bool { return under(dir, p) })
}

func rulesFor(file string) []rule {
	var out []rule
	for _, r := range rules {
		if r.covers(file) {
			out = append(out, r)
		}
	}
	return out
}

// allowed reports whether a package that follows rule r may import imp.
func allowed(imp, module string, r rule) bool {
	switch {
	case strings.HasPrefix(imp, module+"/"):
		rel := strings.TrimPrefix(imp, module+"/")
		return slices.ContainsFunc(r.internal, func(p string) bool { return under(rel, p) })
	case strings.Contains(strings.SplitN(imp, "/", 2)[0], "."):
		return r.thirdParty // the first element of a standard library path has no dot
	}
	return !slices.ContainsFunc(r.noStd, func(p string) bool { return under(imp, p) })
}

// sourceFiles lists the production files of the programs, relative to the
// repository: not the tests, which may reach across, nor the generated mocks.
func sourceFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	for _, dir := range []string{"internal", "cmd", "assets"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			switch {
			case d.IsDir() && (d.Name() == "mocks" || d.Name() == "testdata"):
				return filepath.SkipDir
			case !d.IsDir() && strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go"):
				files = append(files, rel)
			}
			return nil
		})
		require.NoError(t, err)
	}
	return files
}

func importsOf(t *testing.T, file string) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	require.NoError(t, err)
	var out []string
	for _, spec := range parsed.Imports {
		imp, err := strconv.Unquote(spec.Path.Value)
		require.NoError(t, err)
		out = append(out, imp)
	}
	return out
}

func moduleOf(t *testing.T, root string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	for line := range strings.SplitSeq(string(data), "\n") {
		if name, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(name)
		}
	}
	require.FailNow(t, "go.mod names no module")
	return ""
}

// A Secret prints its placeholder because it is asked how it prints, and
// nothing can ask a struct field that is not exported: fmt prints such a field
// by its kind, as the string it is, whatever methods its type has, and so does
// the text handler of slog. So a Secret sits in an exported field.
//
// The test reads the types as they are written: it finds a field of a type
// called Secret, also behind a pointer and in a slice, an array or a map. A
// struct that holds a Secret in an exported field and is itself kept in a
// field that is not exported is beyond it.
func TestSecretsSitInExportedFields(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)

	found := 0
	for _, f := range sourceFiles(t, root) {
		src, err := os.ReadFile(filepath.Join(root, f))
		require.NoError(t, err)
		for _, field := range secretFields(t, f, src) {
			found++
			assert.True(t, ast.IsExported(field.name),
				"%s: the field %s holds a Secret and is not exported, so fmt and slog print what it holds", field.at, field.name)
		}
	}
	assert.Positive(t, found, "the settings of both programs hold secrets, and the scan saw none")
}

// The check of TestSecretsSitInExportedFields, on fields written for it.
func TestSecretsSitInExportedFields_FindsAFieldThatIsNotExported(t *testing.T) {
	const src = `package p

type settings struct {
	Token    envcfg.Secret
	password envcfg.Secret
	Name     string
	secret   string
	keys     []Secret
	byHost   map[string]*Secret
	pair     [2]config.Secret
	a, B     Secret
	Secret
	inner    struct{ pin Secret }
}
`
	var all, hidden []string
	for _, field := range secretFields(t, "p.go", []byte(src)) {
		all = append(all, field.name)
		if !ast.IsExported(field.name) {
			hidden = append(hidden, field.at+" "+field.name)
		}
	}

	assert.Equal(t, []string{"Token", "password", "keys", "byHost", "pair", "a", "B", "pin"}, all,
		"the fields of a type called Secret; an embedded one has the name of its type, which is exported")
	assert.Equal(t, []string{"p.go:5:2 password", "p.go:8:2 keys", "p.go:9:2 byHost", "p.go:10:2 pair", "p.go:11:2 a", "p.go:13:19 pin"}, hidden)
}

// secretField is a struct field of a type called Secret.
type secretField struct {
	name string
	at   string // file, line and column
}

// secretFields lists the named struct fields of a file whose type is called
// Secret, as the file writes it.
func secretFields(t *testing.T, name string, src []byte) []secretField {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	require.NoError(t, err)
	var out []secretField
	ast.Inspect(parsed, func(n ast.Node) bool {
		if st, ok := n.(*ast.StructType); ok {
			for _, field := range st.Fields.List {
				if !holdsSecret(field.Type) {
					continue
				}
				for _, id := range field.Names {
					out = append(out, secretField{name: id.Name, at: fset.Position(id.Pos()).String()})
				}
			}
		}
		return true
	})
	return out
}

// holdsSecret reports whether a type, as it is written, is one called Secret,
// a pointer to one, or a slice, an array or a map of them.
func holdsSecret(typ ast.Expr) bool {
	switch typ := typ.(type) {
	case *ast.Ident:
		return typ.Name == "Secret"
	case *ast.SelectorExpr:
		return typ.Sel.Name == "Secret"
	case *ast.StarExpr:
		return holdsSecret(typ.X)
	case *ast.ParenExpr:
		return holdsSecret(typ.X)
	case *ast.ArrayType:
		return holdsSecret(typ.Elt)
	case *ast.MapType:
		return holdsSecret(typ.Key) || holdsSecret(typ.Value)
	}
	return false
}

// fmt answers the verb %p itself, before it asks a value how it prints: given
// a Secret it prints what the Secret holds, whatever the Format method of the
// type writes. go vet refuses the verb for a string, but it does not check the
// verbs of a type that has a Format method, and a Secret has one. So no string
// of a production file holds the verb. No production file prints an address in
// memory, so the rule costs nothing.
func TestNoProductionStringHoldsThePointerVerb(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)

	for _, f := range sourceFiles(t, root) {
		src, err := os.ReadFile(filepath.Join(root, f))
		require.NoError(t, err)
		for _, at := range pointerVerbs(t, f, src) {
			assert.Fail(t, "a string of a production file holds the verb %p", "%s: under it fmt prints what a Secret holds", at)
		}
	}
}

// The check of TestNoProductionStringHoldsThePointerVerb, on strings written for it.
func TestNoProductionStringHoldsThePointerVerb_FindsTheVerb(t *testing.T) {
	for format, want := range map[string]bool{
		"%p": true, "the value at %p": true, "%#p": true, "%-10p": true, "%[1]p": true, "%[2]*.[1]*p": true,
		"%%%p": true, "%v, then %p": true,
		"": false, "p": false, "%": false, "%s": false, "%v %d": false, "%%p": false, "100%% of the pages": false,
		"%5.2f percent": false, "%w": false,
		// A percent sign and a space before a p are prose to the scan, though a
		// format reads them as the verb with its space flag.
		"100% pass": false, "% p": false,
	} {
		assert.Equal(t, want, holdsPointerVerb(format), "%q", format)
	}

	const src = "package p\n\n" +
		"// A comment may name %p.\n" +
		"var (\n" +
		"\ta = fmt.Sprintf(\"%p\", x)\n" +
		"\tb = `raw %[1]p`\n" +
		"\tc = \"%\" + \"p\"\n" +
		"\td = \"%d\"\n" +
		")\n"
	assert.Equal(t, []string{"p.go:5:18", "p.go:6:6"}, pointerVerbs(t, "p.go", []byte(src)),
		"a string in quotes and a raw one; a verb put together from two strings is beyond the scan")
}

// pointerVerbs lists where the strings of a file hold the verb %p.
func pointerVerbs(t *testing.T, name string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	require.NoError(t, err)
	var out []string
	ast.Inspect(parsed, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			text, err := strconv.Unquote(lit.Value)
			require.NoError(t, err)
			if holdsPointerVerb(text) {
				out = append(out, fset.Position(lit.Pos()).String())
			}
		}
		return true
	})
	return out
}

// holdsPointerVerb reports whether a format holds the verb %p: a percent sign,
// what a verb may carry between it and its letter (flags, a width, a
// precision, argument indexes), and then p. A doubled percent sign is no verb.
// Of the flags the space is left out, so that prose such as "100% pass" is not
// taken for the verb.
func holdsPointerVerb(format string) bool {
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++ // the verb starts here, and the loop steps over its letter
		for i < len(format) && strings.IndexByte("+-#0123456789.*[]", format[i]) >= 0 {
			i++
		}
		if i < len(format) && format[i] == 'p' {
			return true
		}
	}
	return false
}
