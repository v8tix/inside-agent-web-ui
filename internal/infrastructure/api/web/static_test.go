package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/assets"
)

func testApp(t *testing.T) *App { return testAppWith(t, Services{}, Options{}) }

func testAppWith(t *testing.T, svc Services, opts Options) *App {
	t.Helper()
	app, err := NewApp(testLogger(), assets.EmbeddedFiles, svc, opts)
	require.NoError(t, err)
	return app
}

func TestStatic(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		path         string
		wantStatus   int
		wantCache    string
		wantType     string
		wantBodyHint string
	}{
		{"app css revalidates", http.MethodGet, "/static/css/app.css", http.StatusOK, "no-cache", "text/css", "--app-sidebar-width"},
		{"vendored files are immutable", http.MethodGet, "/static/vendor/htmx-2.0.11/htmx.min.js", http.StatusOK, "public, max-age=31536000, immutable", "text/javascript", ""},
		{"favicon is served from static", http.MethodGet, "/static/favicon.svg", http.StatusOK, "no-cache", "image/svg+xml", "<svg"},
		{"HEAD works without a body", http.MethodHead, "/static/css/app.css", http.StatusOK, "no-cache", "text/css", ""},
		{"missing file", http.MethodGet, "/static/nope.js", http.StatusNotFound, "", "", ""},
		{"no directory listing", http.MethodGet, "/static/", http.StatusNotFound, "", "", ""},
		{"no vendor directory listing", http.MethodGet, "/static/vendor/", http.StatusNotFound, "", "", ""},
		{"POST is not allowed", http.MethodPost, "/static/css/app.css", http.StatusMethodNotAllowed, "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			testApp(t).Routes().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			assert.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantStatus != http.StatusOK {
				return
			}
			assert.Equal(t, tc.wantCache, rec.Header().Get("Cache-Control"))
			assert.Contains(t, rec.Header().Get("Content-Type"), tc.wantType)
			assert.NotEmpty(t, rec.Header().Get("ETag"))
			if tc.method == http.MethodGet {
				assert.Contains(t, rec.Body.String(), tc.wantBodyHint)
			} else {
				assert.Zero(t, rec.Body.Len())
			}
		})
	}
}

func TestStatic_ConditionalRequests(t *testing.T) {
	app := testApp(t)
	first := httptest.NewRecorder()
	app.Routes().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/static/css/app.css", nil))
	etag := first.Header().Get("ETag")
	require.NotEmpty(t, etag)

	tests := []struct {
		name       string
		ifNoneMatc string
		wantStatus int
	}{
		{"matching ETag gets 304", etag, http.StatusNotModified},
		{"different ETag gets the file", `"other"`, http.StatusOK},
		{"no validator gets the file", "", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/static/css/app.css", nil)
			if tc.ifNoneMatc != "" {
				req.Header.Set("If-None-Match", tc.ifNoneMatc)
			}
			rec := httptest.NewRecorder()

			app.Routes().ServeHTTP(rec, req)

			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

func TestFavicon(t *testing.T) {
	rec := httptest.NewRecorder()
	testApp(t).Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))

	assert.Equal(t, http.StatusOK, rec.Code, "browsers ask for /favicon.ico; a 404 would be a console error on every page")
	assert.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
}

func TestNewStatic_RequiresFiles(t *testing.T) {
	_, err := NewStatic(fstest.MapFS{})
	assert.Error(t, err)
}
