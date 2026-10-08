package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/assets"
)

func testRenderer(t *testing.T) *Renderer {
	t.Helper()
	rd, err := NewRenderer(assets.EmbeddedFiles)
	require.NoError(t, err)
	return rd
}

func TestRenderer_Page(t *testing.T) {
	tests := []struct {
		name         string
		headers      map[string]string
		status       int
		wantContains []string
	}{
		{
			name:   "a normal request gets the full document",
			status: http.StatusOK,
			wantContains: []string{
				"<!doctype html>", `<body data-ui-contract="1"`, "<title>Sign in · Agent Web UI</title>",
				"/static/vendor/bootstrap-5.3.8/bootstrap.min.css", `rel="icon"`, "Agent Web UI",
			},
		},
		{
			// htmx swaps partials in by itself; a page it asks for, as a boosted
			// form or a history restore does, is a whole document.
			name:         "an htmx request gets the full document too",
			headers:      map[string]string{"HX-Request": "true", "HX-Boosted": "true"},
			status:       http.StatusOK,
			wantContains: []string{"<!doctype html>", "<body", "<title>Sign in · Agent Web UI</title>"},
		},
		{
			name:         "a custom status is honored",
			status:       http.StatusNotFound,
			wantContains: []string{"<!doctype html>"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rd := testRenderer(t)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()

			require.NoError(t, rd.Page(rec, req, tc.status, "login", nil))

			assert.Equal(t, tc.status, rec.Code)
			assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
			for _, want := range tc.wantContains {
				assert.Contains(t, rec.Body.String(), want)
			}
		})
	}
}

func TestRenderer_Errors(t *testing.T) {
	fsys := fstest.MapFS{
		"templates/base.html":       {Data: []byte(`<html>{{template "page:title" .}}{{template "page:main" .}}</html>`)},
		"templates/partials/p.html": {Data: []byte(`{{define "partial:p"}}<b>{{.}}</b>{{end}}`)},
		"templates/pages/ok.html":   {Data: []byte(`{{define "page:title"}}T{{end}}{{define "page:main"}}ok{{end}}`)},
		"templates/pages/boom.html": {Data: []byte(`{{define "page:title"}}T{{end}}{{define "page:main"}}before{{index . 5}}{{end}}`)},
	}
	rd, err := NewRenderer(fsys)
	require.NoError(t, err)

	t.Run("an unknown page is an error and writes nothing", func(t *testing.T) {
		rec := httptest.NewRecorder()
		err := rd.Page(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "nope", nil)
		require.ErrorIs(t, err, ErrUnknownPage)
		assert.Zero(t, rec.Body.Len())
	})

	t.Run("a template that fails midway leaves the response untouched", func(t *testing.T) {
		rec := httptest.NewRecorder()
		err := rd.Page(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "boom", []int{1})
		require.Error(t, err)
		assert.Zero(t, rec.Body.Len(), "partial output must never reach the client")
		assert.Empty(t, rec.Header().Get("Content-Type"))
	})

	t.Run("Block renders a partial to a string with escaping", func(t *testing.T) {
		out, err := rd.Block("partial:p", "<script>alert(1)</script>")
		require.NoError(t, err)
		assert.Equal(t, "<b>&lt;script&gt;alert(1)&lt;/script&gt;</b>", out)
	})

	t.Run("Block reports an unknown template", func(t *testing.T) {
		_, err := rd.Block("partial:nope", nil)
		assert.Error(t, err)
	})
}

func TestNewRenderer_RequiresTheBaseTemplates(t *testing.T) {
	tests := []struct {
		name string
		fsys fs.FS
	}{
		{"empty filesystem", fstest.MapFS{}},
		{"base template missing", fstest.MapFS{
			"templates/pages/home.html": {Data: []byte(`{{define "page:main"}}x{{end}}`)},
		}},
		{"a page that does not parse", fstest.MapFS{
			"templates/base.html":       {Data: []byte(`<html></html>`)},
			"templates/pages/bad.html":  {Data: []byte(`{{define "page:main"}}{{end`)},
			"templates/partials/p.html": {Data: []byte(`{{define "partial:p"}}p{{end}}`)},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRenderer(tc.fsys)
			assert.Error(t, err)
		})
	}
}
