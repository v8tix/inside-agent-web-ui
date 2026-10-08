package web

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzipMinBytes is the size below which an HTML answer is sent as it is: the
// compressed form of a few hundred bytes saves little and costs a header.
const gzipMinBytes = 1024

// acceptsGzip reports whether the request takes a gzip body (RFC 9110, 12.5.3):
// gzip, or "*", with a quality above zero. An explicit gzip decides over "*".
func acceptsGzip(h http.Header) bool {
	var gzipOK, gzipSeen, starOK bool
	for part := range strings.SplitSeq(strings.Join(h.Values("Accept-Encoding"), ","), ",") {
		coding, params, _ := strings.Cut(part, ";")
		coding = strings.ToLower(strings.TrimSpace(coding))
		ok := qualityOf(params) > 0
		switch coding {
		case "gzip", "x-gzip":
			gzipOK, gzipSeen = ok, true
		case "*":
			starOK = ok
		}
	}
	if gzipSeen {
		return gzipOK
	}
	return starOK
}

// qualityOf reads the q value of a coding's parameters, 1 when there is none
// or it cannot be read.
func qualityOf(params string) float64 {
	for param := range strings.SplitSeq(params, ";") {
		name, value, found := strings.Cut(param, "=")
		if found && strings.EqualFold(strings.TrimSpace(name), "q") {
			if q, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				return q
			}
		}
	}
	return 1
}

// gzipWriters keeps writers for reuse: one holds a few hundred kilobytes of
// state, and a page is compressed on every view.
var gzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(nil) }}

// gzipBytes compresses src in one piece. The default level is a good middle for
// markup that is sent once: about as small as the slow levels, at a fraction
// of the time.
func gzipBytes(src []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(src) / 4)
	w := gzipWriters.Get().(*gzip.Writer)
	defer gzipWriters.Put(w)
	w.Reset(&out)
	_, _ = w.Write(src)
	_ = w.Close() // writing to a buffer cannot fail
	return out.Bytes()
}

// sendHTML writes an HTML answer, compressed when the browser takes gzip and the
// answer is big enough to be worth it. Pages and fragments are rendered into
// memory first, so the length is known and the answer is sent in one piece.
func sendHTML(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Add("Vary", "Accept-Encoding")
	if len(body) >= gzipMinBytes && acceptsGzip(r.Header) {
		body = gzipBytes(body)
		h.Set("Content-Encoding", "gzip")
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
