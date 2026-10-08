package web_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/assets"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/api/web"
)

// ownFiles are the files we author (everything except vendored libraries).
func ownFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := fs.WalkDir(assets.EmbeddedFiles, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(p, "static/vendor/") {
			return err
		}
		data, err := fs.ReadFile(assets.EmbeddedFiles, p)
		files[p] = string(data)
		return err
	})
	require.NoError(t, err)
	require.NotEmpty(t, files)
	return files
}

func templates(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for p, content := range ownFiles(t) {
		if strings.HasPrefix(p, "templates/") {
			out[p] = content
		}
	}
	require.NotEmpty(t, out)
	return out
}

func TestTemplatesReferenceExistingStaticFiles(t *testing.T) {
	ref := regexp.MustCompile(`(?:href|src)="(/static/[^"#?]+)"`)
	cssURL := regexp.MustCompile(`url\(\s*['"]?(/static/[^'")#?]+)`)

	for p, content := range ownFiles(t) {
		for _, re := range []*regexp.Regexp{ref, cssURL} {
			for _, m := range re.FindAllStringSubmatch(content, -1) {
				_, err := fs.Stat(assets.EmbeddedFiles, strings.TrimPrefix(m[1], "/"))
				assert.NoError(t, err, "%s references a missing file: %s", p, m[1])
			}
		}
	}
}

func TestOwnFilesLoadNothingExternal(t *testing.T) {
	external := regexp.MustCompile(`https?://[^\s"'<>)]+`)
	for p, content := range ownFiles(t) {
		for _, u := range external.FindAllString(content, -1) {
			assert.True(t, strings.HasPrefix(u, "http://www.w3.org/"),
				"%s loads or links an external host (%s); everything must be vendored", p, u)
		}
	}
}

func TestTemplatesRespectCSP(t *testing.T) {
	// The CSP is script-src 'self' / style-src 'self': inline code would be blocked.
	forbidden := map[string]*regexp.Regexp{
		"inline <script> body":   regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>\s*[^<\s]`),
		"<style> element":        regexp.MustCompile(`(?i)<style[\s>]`),
		"style attribute":        regexp.MustCompile(`(?i)\sstyle\s*=`),
		"inline event handler":   regexp.MustCompile(`(?i)\son[a-z]+\s*=`),
		"javascript: URL":        regexp.MustCompile(`(?i)javascript:`),
		"hx-on attribute (eval)": regexp.MustCompile(`(?i)\shx-on[:-]`),
	}
	for p, content := range templates(t) {
		for what, re := range forbidden {
			assert.False(t, re.MatchString(content), "%s contains a %s", p, what)
		}
	}
}

func TestVendoredFilesMatchVersions(t *testing.T) {
	manifest, err := fs.ReadFile(assets.EmbeddedFiles, "static/vendor/VERSIONS.md")
	require.NoError(t, err)

	row := regexp.MustCompile(`(?m)^\| (\S+\.(?:css|js)) \|[^|]*\|[^|]*\| ([0-9a-f]{64}) \|`)
	rows := row.FindAllStringSubmatch(string(manifest), -1)
	require.NotEmpty(t, rows, "VERSIONS.md must list the vendored files")

	listed := map[string]bool{}
	for _, m := range rows {
		listed[m[1]] = true
		data, err := fs.ReadFile(assets.EmbeddedFiles, path.Join("static/vendor", m[1]))
		require.NoError(t, err, "VERSIONS.md lists a file that does not exist: %s", m[1])
		sum := sha256.Sum256(data)
		assert.Equal(t, m[2], hex.EncodeToString(sum[:]), "%s changed without updating VERSIONS.md", m[1])
	}

	err = fs.WalkDir(assets.EmbeddedFiles, "static/vendor", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, "static/vendor/")
		if rel != "VERSIONS.md" && path.Base(rel) != "LICENSE" {
			assert.True(t, listed[rel], "vendored file %s is not listed in VERSIONS.md", rel)
		}
		return nil
	})
	require.NoError(t, err)
}

func TestVendoredLibrariesKeepTheirLicense(t *testing.T) {
	dirs, err := fs.ReadDir(assets.EmbeddedFiles, "static/vendor")
	require.NoError(t, err)
	for _, d := range dirs {
		if d.IsDir() {
			_, err := fs.Stat(assets.EmbeddedFiles, path.Join("static/vendor", d.Name(), "LICENSE"))
			assert.NoError(t, err, "%s must ship its LICENSE", d.Name())
		}
	}
}

// htmxConfig is the settings that htmx reads from a meta tag of base.html, as
// the JSON text they are.
func htmxConfig(t *testing.T) []byte {
	t.Helper()
	base := templates(t)["templates/base.html"]
	m := regexp.MustCompile(`<meta name="htmx-config" content='([^']*)'`).FindStringSubmatch(base)
	require.NotNil(t, m, "base.html has no htmx-config meta tag")
	return []byte(m[1])
}

// htmx reads its settings from a meta tag. Three of them protect the CSP, and a
// change that drops one only shows up in the browser console.
func TestHtmxConfigKeepsTheCSPWorking(t *testing.T) {
	var cfg struct {
		AllowEval          bool     `json:"allowEval"`
		AllowScriptTags    *bool    `json:"allowScriptTags"`
		IncludeIndicator   bool     `json:"includeIndicatorStyles"`
		HistoryCacheSize   int      `json:"historyCacheSize"`
		DefaultSettleDelay *int     `json:"defaultSettleDelay"`
		AttributesToSettle []string `json:"attributesToSettle"`
	}
	require.NoError(t, json.Unmarshal(htmxConfig(t), &cfg))

	assert.False(t, cfg.AllowEval, "no eval: the CSP has no unsafe-eval")
	require.NotNil(t, cfg.AllowScriptTags, "htmx runs the <script> elements of what it swaps in unless told not to")
	assert.False(t, *cfg.AllowScriptTags, "nothing swapped in is ever a script")
	require.NotNil(t, cfg.DefaultSettleDelay, "htmx waits 20 ms after every swap unless told not to")
	assert.Zero(t, *cfg.DefaultSettleDelay,
		"the delay sits between pressing Send and the reply's stream opening, on every message (U-05)")
	assert.False(t, cfg.IncludeIndicator, "htmx must not inject its own <style> element")
	assert.NotEmpty(t, cfg.AttributesToSettle, "an empty list falls back to the default, which includes style")
	assert.NotContains(t, cfg.AttributesToSettle, "style",
		"settling copies the old element's style attribute with setAttribute, which the CSP blocks; the composer's autosize sets one")
}

// htmx answers a response by the first entry of responseHandling whose code
// matches its status. The server explains a refusal itself (409, 422, 429), so
// those answers are swapped in. Any other error swaps nothing and is reported
// to the page, which says what happened (app.js, htmx:responseError). The entry
// for the refusals has to come before the one for every error, or it is never
// reached.
func TestHtmxConfigSwapsARefusalInAndReportsAnyOtherError(t *testing.T) {
	var cfg struct {
		ResponseHandling []struct {
			Code  string `json:"code"`
			Swap  bool   `json:"swap"`
			Error bool   `json:"error"`
		} `json:"responseHandling"`
	}
	require.NoError(t, json.Unmarshal(htmxConfig(t), &cfg))
	require.NotEmpty(t, cfg.ResponseHandling, "without a list htmx uses its own, which swaps no error answer in")
	// handling finds the entry for a status as htmx does: the first whose code, a
	// regular expression, matches it.
	handling := func(status int) (swap, isError bool) {
		for _, entry := range cfg.ResponseHandling {
			if regexp.MustCompile(entry.Code).MatchString(strconv.Itoa(status)) {
				return entry.Swap, entry.Error
			}
		}
		return false, false // what htmx does with a status that no entry matches
	}

	tests := []struct {
		name      string
		statuses  []int
		wantSwap  bool
		wantError bool
	}{
		{"a page or a fragment is swapped in", []int{http.StatusOK}, true, false},
		{"an answer with nothing in it swaps nothing", []int{http.StatusNoContent}, false, false},
		{
			"a refusal that the server explains is swapped in",
			[]int{http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests}, true, false,
		},
		{
			"any other error swaps nothing and is reported",
			[]int{
				http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusRequestEntityTooLarge,
				http.StatusInternalServerError, http.StatusServiceUnavailable,
			},
			false, true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, status := range tc.statuses {
				swap, isError := handling(status)
				assert.Equal(t, tc.wantSwap, swap, "swap for %d", status)
				assert.Equal(t, tc.wantError, isError, "error for %d", status)
			}
		})
	}
}

// htmx waits for an answer without end unless it is told a limit, and then the
// page's words for an answer that takes too long (app.js, htmx:timeout) are never
// said. The limit is for the requests htmx sends itself; a reply's event stream
// is not one of them, and has none.
func TestHtmxConfigGivesARequestThirtySeconds(t *testing.T) {
	var cfg struct {
		Timeout *int `json:"timeout"`
	}
	require.NoError(t, json.Unmarshal(htmxConfig(t), &cfg))

	require.NotNil(t, cfg.Timeout, "htmx waits for an answer without end unless told a limit")
	assert.Equal(t, 30_000, *cfg.Timeout, "in milliseconds")
	assert.True(t, strings.Contains(ownFiles(t)["static/js/app.js"], "'htmx:timeout'"),
		"app.js listens for htmx:timeout, so the page says so when the limit passes")
}

// SC-13: the first page load stays under 150 KB as it goes over the wire, with
// no external hosts. It asks for the sign-in page and every file that page
// links, as a browser does, and adds up the bytes the server sends. (Compressing
// the files here, as an earlier version of this test did, said 83 KB while the
// server sent 405.)
func TestFirstLoadWeight(t *testing.T) {
	const budget = 150 * 1024
	app, err := web.NewApp(slog.New(slog.NewTextHandler(io.Discard, nil)), assets.EmbeddedFiles, web.Services{}, web.Options{})
	require.NoError(t, err)
	routes := app.Routes()
	fetch := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept-Encoding", "gzip, deflate, br")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, path)
		return rec
	}

	page := fetch("/login")
	total := page.Body.Len()
	markup := page.Body.Bytes()
	if page.Header().Get("Content-Encoding") == "gzip" {
		r, err := gzip.NewReader(bytes.NewReader(markup))
		require.NoError(t, err)
		markup, err = io.ReadAll(r)
		require.NoError(t, err)
	}
	files := 0
	for _, m := range regexp.MustCompile(`(?:href|src)="(/static/[^"]+)"`).FindAllStringSubmatch(string(markup), -1) {
		file := fetch(m[1])
		assert.Equal(t, "gzip", file.Header().Get("Content-Encoding"), "%s goes over the wire compressed", m[1])
		total += file.Body.Len()
		files++
	}
	require.NotZero(t, files, "the page links its files")
	t.Logf("first load, as sent: %d bytes of %d, in %d requests", total, budget, files+1)
	assert.LessOrEqual(t, total, budget)
}
