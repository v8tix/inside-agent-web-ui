package web

import "net/http"

// contentSecurityPolicy allows only same-origin scripts, styles and images (plus
// data: images for Bootstrap's embedded icons). No inline scripts or styles:
// templates and JS must stay external.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// permissionsPolicy turns off the browser features the page never uses.
const permissionsPolicy = "camera=(), microphone=(), geolocation=(), payment=(), usb=()"

// SecurityHeaders sets the standard browser hardening headers on every
// response. hsts adds Strict-Transport-Security and should be on only when the
// app is served over https.
//
// Every answer is "no-store" unless its route says otherwise (static files, the
// event stream): a page holds one person's conversations, and the next person at
// the same computer must not get it back with the Back button.
func SecurityHeaders(hsts bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", contentSecurityPolicy)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Permissions-Policy", permissionsPolicy)
			h.Set("Cache-Control", "no-store")
			if hsts {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
