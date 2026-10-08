package security_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/it/internal/stack"
)

// ids are a conversation and its reply, which the routes with {id} and {mid}
// are called with.
type ids struct{ conv, reply string }

// route is a line of the Web UI's route table, with a way to call it.
type route struct {
	// pattern is the route as the web package registers it.
	pattern string
	// public routes are answered without signing in.
	public bool
	// changes routes change something, so the browser's cross-origin rules apply.
	changes bool
	// call makes the request; headers are pairs of name and value.
	call func(c *stack.Client, x ids, headers ...string) *stack.Response
}

// staticFile is an asset the page really links.
const staticFile = "/static/css/app.css"

// routes is every route of the Web UI. TestRoutes_TheTableIsComplete keeps it
// in step with the routes the code registers, so a route added without a line
// here fails a test. The tests named TestRoutes_ read the code and start
// nothing, so the gate runs them where there is no Docker (scripts/check.sh).
//
// Pattern: Tests That Read the Repository
var routes = []route{
	{"GET /healthz", true, false, func(c *stack.Client, _ ids, h ...string) *stack.Response { return c.Get("/healthz", h...) }},
	{"GET /readyz", true, false, func(c *stack.Client, _ ids, h ...string) *stack.Response { return c.Get("/readyz", h...) }},
	{"GET /static/{path...}", true, false, func(c *stack.Client, _ ids, h ...string) *stack.Response { return c.Get(staticFile, h...) }},
	{"GET /favicon.ico", true, false, func(c *stack.Client, _ ids, h ...string) *stack.Response { return c.Get("/favicon.ico", h...) }},
	{"GET /login", true, false, func(c *stack.Client, _ ids, h ...string) *stack.Response { return c.Get("/login", h...) }},
	{"POST /login", true, true, func(c *stack.Client, _ ids, h ...string) *stack.Response {
		return c.Post("/login", url.Values{"email": {stack.Email}, "password": {stack.Password}}, h...)
	}},
	{"POST /logout", false, true, func(c *stack.Client, _ ids, h ...string) *stack.Response { return c.Post("/logout", nil, h...) }},
	{"GET /{$}", false, false, func(c *stack.Client, _ ids, h ...string) *stack.Response { return c.Get("/", h...) }},
	{"POST /c", false, true, func(c *stack.Client, _ ids, h ...string) *stack.Response {
		return c.Post("/c", url.Values{"content": {"a new conversation"}}, h...)
	}},
	{"GET /c/{id}", false, false, func(c *stack.Client, x ids, h ...string) *stack.Response { return c.Get("/c/"+x.conv, h...) }},
	{"PATCH /c/{id}", false, true, func(c *stack.Client, x ids, h ...string) *stack.Response {
		return c.Do(http.MethodPatch, "/c/"+x.conv, url.Values{"title": {"renamed"}}, h...)
	}},
	{"DELETE /c/{id}", false, true, func(c *stack.Client, x ids, h ...string) *stack.Response {
		return c.Do(http.MethodDelete, "/c/"+x.conv, nil, h...)
	}},
	{"GET /c/{id}/item", false, false, func(c *stack.Client, x ids, h ...string) *stack.Response { return c.Get("/c/"+x.conv+"/item", h...) }},
	{"GET /c/{id}/rename", false, false, func(c *stack.Client, x ids, h ...string) *stack.Response { return c.Get("/c/"+x.conv+"/rename", h...) }},
	{"GET /c/{id}/delete", false, false, func(c *stack.Client, x ids, h ...string) *stack.Response { return c.Get("/c/"+x.conv+"/delete", h...) }},
	{"POST /c/{id}/messages", false, true, func(c *stack.Client, x ids, h ...string) *stack.Response {
		return c.Post("/c/"+x.conv+"/messages", url.Values{"content": {"one more"}}, h...)
	}},
	{"GET /c/{id}/m/{mid}/stream", false, false, func(c *stack.Client, x ids, h ...string) *stack.Response {
		return c.Get("/c/"+x.conv+"/m/"+x.reply+"/stream", append([]string{"Accept", "text/event-stream"}, h...)...)
	}},
	{"GET /c/{id}/m/{mid}/raw", false, false, func(c *stack.Client, x ids, h ...string) *stack.Response {
		return c.Get("/c/"+x.conv+"/m/"+x.reply+"/raw", h...)
	}},
	{"POST /c/{id}/m/{mid}/stop", false, true, func(c *stack.Client, x ids, h ...string) *stack.Response {
		return c.Post("/c/"+x.conv+"/m/"+x.reply+"/stop", nil, h...)
	}},
	{"POST /c/{id}/m/{mid}/regenerate", false, true, func(c *stack.Client, x ids, h ...string) *stack.Response {
		return c.Post("/c/"+x.conv+"/m/"+x.reply+"/regenerate", nil, h...)
	}},
}

// registered reads the patterns the Web UI registers from the code of its HTTP
// adapter.
func registered(t *testing.T) []string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return patternsIn(t, filepath.Join(filepath.Dir(here), "..", "..", "internal", "infrastructure", "api", "web"))
}

// A pattern of http.ServeMux reads as a method and a path, or as a path alone.
var (
	routeWithMethod = regexp.MustCompile(`^[A-Z]+ /\S*$`)
	routePathAlone  = regexp.MustCompile(`^/\S*$`)
)

// patternsIn reads the route patterns that the package in dir registers, from each
// of its files that is not a test: every string that reads as a method and a path,
// wherever it stands, and every string that reads as a path alone and is the first
// thing a call is given. So a route is found whichever file registers it and
// whatever takes its pattern. A pattern that is put together while the program runs
// is no such string, and is not found. Each pattern comes out once.
func patternsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	text := func(n ast.Node) string {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return ""
		}
		s, err := strconv.Unquote(lit.Value)
		require.NoError(t, err)
		return s
	}

	found := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		require.NoError(t, err)
		for n := range ast.Preorder(file) {
			if s := text(n); routeWithMethod.MatchString(s) {
				found[s] = true
			}
			if call, ok := n.(*ast.CallExpr); ok && len(call.Args) > 0 {
				if s := text(call.Args[0]); routePathAlone.MatchString(s) {
					found[s] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(found))
}

func TestRoutes_TheTableIsComplete(t *testing.T) {
	var listed []string
	for _, r := range routes {
		listed = append(listed, r.pattern)
	}

	found := registered(t)

	require.NotEmpty(t, found, "the code of the Web UI was read")
	require.ElementsMatch(t, found, listed, "every route has a line in the security table, and the table has no other")
}

// The table is as complete as the scan that fills it. A route is a route whichever
// file registers it and whatever takes its pattern: the mux, a helper that is
// given the mux first, a table or a constant. So the scan goes by the pattern.
func TestRoutes_TheScanFindsARouteHoweverItIsRegistered(t *testing.T) {
	dir := t.TempDir()
	for name, src := range map[string]string{
		"routes.go": `package web

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	mux.Handle("GET /static/{path...}", a.static)
	mux.HandleFunc(` + "`GET /{$}`" + `, a.newChatPage)
	a.more(mux)
	return a.authenticate(mux)
}
`,
		"more.go": `package web

const about = "GET /about"

func (a *App) more(mux *http.ServeMux) {
	mux.HandleFunc("POST /c/{id}/share", a.share)
	a.signedIn("GET /settings", a.settings)
	route("DELETE /c/{id}/share", a.unshare)
	handle(mux, "PUT /c/{id}/pin", a.pin)
	for pattern, h := range map[string]http.HandlerFunc{"GET /export": a.export} {
		mux.HandleFunc(pattern, h)
	}
	mux.HandleFunc(about, a.about)
	mux.Handle("/metrics", a.metrics)

	// A pattern that the code names a second time is one route.
	if r.Pattern == "GET /settings" {
		return
	}
	// Paths that are no routes: they are not what a call is given first, or they are
	// no pattern.
	if strings.HasPrefix(r.URL.Path, "/static/") {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
	next := "/c/" + id
	a.log.Info("GET /settings was asked for")
	a.log.Debug("served /static")
}
`,
		"more_test.go": `package web

func TestMore(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /only-in-a-test", func(http.ResponseWriter, *http.Request) {})
}
`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600))
	}

	found := patternsIn(t, dir)

	require.ElementsMatch(t, []string{
		"GET /healthz", "GET /static/{path...}", "GET /{$}",
		"POST /c/{id}/share", "GET /settings", "DELETE /c/{id}/share", "/metrics",
		"PUT /c/{id}/pin", "GET /export", "GET /about",
	}, found)
}
