package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

const (
	immutableCache  = "public, max-age=31536000, immutable"
	revalidateCache = "no-cache"
)

type staticFile struct {
	data         []byte
	etag         string
	contentType  string
	cacheControl string
	// gz is the gzip form of data, made once at startup. It is nil for a file
	// that is already compressed or that gzip would not make smaller.
	gz     []byte
	gzETag string
}

// Static serves the embedded static/ directory from memory. Vendored files live
// in version-named directories, so they are cached as immutable; our own files
// are revalidated with an ETag on every load. Text files are sent gzipped to a
// browser that takes it: a first load of about 400 KB of text is under 100 KB.
type Static struct {
	files map[string]staticFile
}

// NewStatic reads every file under static/ in fsys.
func NewStatic(fsys fs.FS) (*Static, error) {
	files := map[string]staticFile{}
	err := fs.WalkDir(fsys, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(p, "static/")
		sum := sha256.Sum256(data)
		cache := revalidateCache
		if strings.HasPrefix(name, "vendor/") {
			cache = immutableCache
		}
		f := staticFile{
			data:         data,
			etag:         `"` + hex.EncodeToString(sum[:8]) + `"`,
			contentType:  contentTypeFor(name, data),
			cacheControl: cache,
		}
		if compressible(f.contentType) {
			// The slowest level: it is done once, and every visit gets the saving.
			if gz := gzipBest(data); len(gz) < len(data) {
				f.gz, f.gzETag = gz, `"`+hex.EncodeToString(sum[:8])+`-gzip"`
			}
		}
		files[name] = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("no static files found")
	}
	return &Static{files: files}, nil
}

// compressible reports whether a content type is text that gzip shrinks. Images
// and fonts are already compressed.
func compressible(contentType string) bool {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch {
	case strings.HasPrefix(mediaType, "text/"):
		return true
	case mediaType == "image/svg+xml", strings.HasSuffix(mediaType, "+json"), strings.HasSuffix(mediaType, "+xml"):
		return true
	}
	return mediaType == "application/javascript" || mediaType == "application/json" || mediaType == "application/xml"
}

func gzipBest(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression) // the level is valid
	_, _ = w.Write(data)
	_ = w.Close() // writing to a buffer cannot fail
	return buf.Bytes()
}

func contentTypeFor(name string, data []byte) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return http.DetectContentType(data)
}

// ServeHTTP serves the file named by the {path...} wildcard.
func (s *Static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.serve(w, r, r.PathValue("path"))
}

// favicon answers /favicon.ico, which browsers request on their own. Without
// it every page load would log a 404 and a console error.
func (s *Static) favicon(w http.ResponseWriter, r *http.Request) {
	s.serve(w, r, "favicon.svg")
}

func (s *Static) serve(w http.ResponseWriter, r *http.Request, name string) {
	f, ok := s.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.contentType)
	h.Set("Cache-Control", f.cacheControl)
	body, etag := f.data, f.etag
	if f.gz != nil {
		h.Add("Vary", "Accept-Encoding") // the answer depends on it, for the cache's sake
		if acceptsGzip(r.Header) {
			body, etag = f.gz, f.gzETag
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("ETag", etag)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
}
