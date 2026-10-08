// Package logallowlist logs each request as a fixed list of named attributes.
package logallowlist

import (
	"log/slog"
	"net/http"
	"time"
)

// Requests logs one line for each request that next serves. The four attributes
// named here are the allow-list: the query, the headers, the cookies and the
// form of a request are not named, so they cannot reach the line.
func Requests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path), // the path alone, without the query
			slog.Duration("duration", time.Since(start)),
			slog.String("remote", r.RemoteAddr),
		)
	})
}
