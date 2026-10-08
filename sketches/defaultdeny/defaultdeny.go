// Package defaultdeny protects every route that is not listed as public.
package defaultdeny

import (
	"net/http"
	"strings"
)

// Paths builds the public list. A path that ends in "/" opens everything below it.
func Paths(open ...string) func(path string) bool {
	return func(path string) bool {
		for _, p := range open {
			if path == p || strings.HasSuffix(p, "/") && strings.HasPrefix(path, p) {
				return true
			}
		}
		return false
	}
}

// Guard lets a request through when its path is public or signedIn says its
// sender is known. A route that nobody mentioned is therefore private, and a
// route added later is protected without a change here.
func Guard(public func(string) bool, signedIn func(*http.Request) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !public(r.URL.Path) && !signedIn(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
