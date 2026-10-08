package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gunzip(t *testing.T, data []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(data))
	require.NoError(t, err, "the body is gzip")
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return out
}

func TestAcceptsGzip(t *testing.T) {
	tests := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip", true},
		{"GZIP", true},
		{"x-gzip", true},
		{"deflate, gzip", true},
		{"br, zstd, gzip;q=0.5", true},
		{"gzip;q=1.0", true},
		{"gzip; q=0.8", true},
		{"gzip;q=0", false},
		{"gzip; q=0.0", false},
		{"br, zstd", false},
		{"identity", false},
		{"*", true},
		{"*;q=0", false},
		{"gzip;q=0, *", false},
		{"br, *", true},
		{"gzip;q=garbage", true},
	}
	for _, tc := range tests {
		t.Run(tc.header, func(t *testing.T) {
			h := http.Header{}
			if tc.header != "" {
				h.Set("Accept-Encoding", tc.header)
			}

			assert.Equal(t, tc.want, acceptsGzip(h))
		})
	}

	t.Run("several header lines count together", func(t *testing.T) {
		h := http.Header{}
		h.Add("Accept-Encoding", "br")
		h.Add("Accept-Encoding", "gzip")

		assert.True(t, acceptsGzip(h))
	})
}

func TestGzipBytes(t *testing.T) {
	src := []byte(strings.Repeat("<li>a sidebar row</li>\n", 500))

	out := gzipBytes(src)

	assert.Less(t, len(out), len(src)/5, "repeated markup shrinks a lot")
	assert.Equal(t, src, gunzip(t, out))

	t.Run("a writer from the pool starts clean each time", func(t *testing.T) {
		for _, text := range []string{"first", "a second, different one", ""} {
			assert.Equal(t, text, string(gunzip(t, gzipBytes([]byte(text)))))
		}
	})

	t.Run("it is safe to use from many requests at once", func(t *testing.T) {
		done := make(chan struct{})
		for i := range 16 {
			go func() {
				defer func() { done <- struct{}{} }()
				text := bytes.Repeat([]byte{byte('a' + i)}, 4000)
				assert.Equal(t, text, gunzip(t, gzipBytes(text)))
			}()
		}
		for range 16 {
			<-done
		}
	})
}

func TestStatic_Compression(t *testing.T) {
	app := testApp(t)
	get := func(method, path string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		app.Routes().ServeHTTP(rec, req)
		return rec
	}
	const css = "/static/css/app.css"
	plain := get(http.MethodGet, css, nil)
	require.Equal(t, http.StatusOK, plain.Code)

	t.Run("a browser that takes gzip gets it, smaller, with its own validator", func(t *testing.T) {
		rec := get(http.MethodGet, css, map[string]string{"Accept-Encoding": "gzip, deflate, br"})

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
		assert.Contains(t, rec.Header().Values("Vary"), "Accept-Encoding", "caches must keep the two apart")
		assert.Equal(t, plain.Body.Bytes(), gunzip(t, rec.Body.Bytes()), "it is the same file")
		assert.Less(t, rec.Body.Len(), plain.Body.Len()/2)
		assert.NotEqual(t, plain.Header().Get("ETag"), rec.Header().Get("ETag"), "two representations, two validators")
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/css")
		assert.Equal(t, plain.Header().Get("Cache-Control"), rec.Header().Get("Cache-Control"))
	})

	t.Run("one that does not gets the file as it is, and the cache is told it varies", func(t *testing.T) {
		assert.Empty(t, plain.Header().Get("Content-Encoding"))
		assert.Contains(t, plain.Header().Values("Vary"), "Accept-Encoding")
	})

	t.Run("gzip refused with q=0 is refused", func(t *testing.T) {
		rec := get(http.MethodGet, css, map[string]string{"Accept-Encoding": "gzip;q=0"})

		assert.Empty(t, rec.Header().Get("Content-Encoding"))
		assert.Equal(t, plain.Body.Bytes(), rec.Body.Bytes())
	})

	t.Run("HEAD says the same and sends nothing", func(t *testing.T) {
		rec := get(http.MethodHead, css, map[string]string{"Accept-Encoding": "gzip"})

		assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
		assert.Zero(t, rec.Body.Len())
	})

	t.Run("each representation is checked against its own validator", func(t *testing.T) {
		gz := get(http.MethodGet, css, map[string]string{"Accept-Encoding": "gzip"})

		again := get(http.MethodGet, css, map[string]string{"Accept-Encoding": "gzip", "If-None-Match": gz.Header().Get("ETag")})
		assert.Equal(t, http.StatusNotModified, again.Code, "the gzip validator matches the gzip file")

		other := get(http.MethodGet, css, map[string]string{"Accept-Encoding": "gzip", "If-None-Match": plain.Header().Get("ETag")})
		assert.Equal(t, http.StatusOK, other.Code, "the plain file's validator says nothing about the gzip one")
		assert.Equal(t, "gzip", other.Header().Get("Content-Encoding"))
	})

	t.Run("every text file of the page is sent compressed", func(t *testing.T) {
		for _, name := range []string{
			"/static/css/app.css", "/static/css/chroma.css", "/static/js/app.js", "/static/js/theme-init.js",
			"/static/vendor/bootstrap-5.3.8/bootstrap.min.css", "/static/vendor/bootstrap-5.3.8/bootstrap.bundle.min.js",
			"/static/vendor/htmx-2.0.11/htmx.min.js", "/static/vendor/htmx-ext-sse-2.2.4/sse.min.js", "/static/favicon.svg",
		} {
			rec := get(http.MethodGet, name, map[string]string{"Accept-Encoding": "gzip"})

			assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"), name)
		}
	})
}

// A file that gzip would make bigger is sent as it is.
func TestNewStatic_CompressesOnlyWhatShrinks(t *testing.T) {
	st, err := NewStatic(fstest.MapFS{
		"static/tiny.css": {Data: []byte("a{}")},
		"static/big.css":  {Data: []byte(strings.Repeat("a { color: red }\n", 200))},
		"static/logo.png": {Data: bytes.Repeat([]byte{0x89, 'P', 'N', 'G'}, 500)},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.Handle("GET /static/{path...}", st)
	serve := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	assert.Empty(t, serve("/static/tiny.css").Header().Get("Content-Encoding"), "gzip would add its header to three bytes")
	assert.Equal(t, "gzip", serve("/static/big.css").Header().Get("Content-Encoding"))
	assert.Empty(t, serve("/static/logo.png").Header().Get("Content-Encoding"), "images are already compressed")
}

func TestRenderer_PageCompression(t *testing.T) {
	rd := testRenderer(t)
	page := func(accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if accept != "" {
			req.Header.Set("Accept-Encoding", accept)
		}
		rec := httptest.NewRecorder()
		require.NoError(t, rd.Page(rec, req, http.StatusOK, "login", nil))
		return rec
	}
	plain := page("")
	require.Greater(t, plain.Body.Len(), 1024, "the sign-in page is more than a kilobyte")

	t.Run("a page for a browser that takes gzip is sent compressed", func(t *testing.T) {
		rec := page("gzip, br")

		assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
		assert.Contains(t, rec.Header().Values("Vary"), "Accept-Encoding")
		assert.Equal(t, plain.Body.Bytes(), gunzip(t, rec.Body.Bytes()))
		assert.Less(t, rec.Body.Len(), plain.Body.Len()*2/3)
		assert.Equal(t, rec.Header().Get("Content-Length"), strconv.Itoa(rec.Body.Len()), "the length is that of what is sent")
	})

	t.Run("otherwise it is sent as it is, and the cache is told it varies", func(t *testing.T) {
		assert.Empty(t, plain.Header().Get("Content-Encoding"))
		assert.Contains(t, plain.Header().Values("Vary"), "Accept-Encoding")
		assert.Equal(t, "text/html; charset=utf-8", plain.Header().Get("Content-Type"))
	})
}

func TestWriteHTML_Compression(t *testing.T) {
	send := func(accept, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if accept != "" {
			req.Header.Set("Accept-Encoding", accept)
		}
		rec := httptest.NewRecorder()
		writeHTML(rec, req, http.StatusCreated, body)
		return rec
	}
	big := strings.Repeat("<div class=\"message\">a reply</div>\n", 100)

	t.Run("a big fragment is compressed", func(t *testing.T) {
		rec := send("gzip", big)

		assert.Equal(t, http.StatusCreated, rec.Code)
		assert.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
		assert.Equal(t, big, string(gunzip(t, rec.Body.Bytes())))
	})
	t.Run("a small one is not: the header would cost more than it saves", func(t *testing.T) {
		rec := send("gzip", "<b>ok</b>")

		assert.Empty(t, rec.Header().Get("Content-Encoding"))
		assert.Equal(t, "<b>ok</b>", rec.Body.String())
	})
	t.Run("nor one for a browser that does not take gzip", func(t *testing.T) {
		rec := send("", big)

		assert.Empty(t, rec.Header().Get("Content-Encoding"))
		assert.Equal(t, big, rec.Body.String())
	})
	t.Run("an empty answer stays empty", func(t *testing.T) {
		rec := send("gzip", "")

		assert.Empty(t, rec.Header().Get("Content-Encoding"))
		assert.Zero(t, rec.Body.Len())
		assert.Equal(t, "0", rec.Header().Get("Content-Length"))
	})
}
