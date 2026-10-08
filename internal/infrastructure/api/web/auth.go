package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"uuid"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
)

// sessionCookie is the cookie that carries the login token.
const sessionCookie = "awu_session"

type ctxUserKey struct{}

// UserFrom returns the signed-in user of the request, if any.
func UserFrom(ctx context.Context) (domain.User, bool) {
	u, ok := ctx.Value(ctxUserKey{}).(domain.User)
	return u, ok
}

// isPublic reports whether a path is reachable without signing in. Everything
// else is private: a new route is protected unless it is listed here.
//
// Pattern: Default-Deny Router
func isPublic(r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz", "/readyz", "/login", "/favicon.ico":
		return true
	}
	return strings.HasPrefix(r.URL.Path, "/static/")
}

// needsNoSession reports whether a request is answered the same for everyone, so
// that looking up who is asking would be two queries for nothing: the files of
// the page (a page loads a dozen), the favicon and the probes. It also keeps them
// working when the database is down.
func needsNoSession(r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz", "/readyz", "/favicon.ico":
		return true
	}
	return strings.HasPrefix(r.URL.Path, "/static/")
}

// authenticate resolves the session cookie, if there is one, to a user on the
// request context, and turns away anonymous requests to private routes. A
// stale cookie is cleared.
func (a *App) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" && !needsNoSession(r) {
			user, err := a.svc.Auth.Authenticate(r.Context(), c.Value)
			switch {
			case err == nil:
				r = r.WithContext(context.WithValue(r.Context(), ctxUserKey{}, user))
			case errors.Is(err, domain.ErrInvalidCredentials):
				http.SetCookie(w, a.expiredCookie())
			default:
				a.serverError(w, r, err)
				return
			}
		}
		if _, signedIn := UserFrom(r.Context()); !signedIn && !isPublic(r) {
			a.sendToLogin(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isScript reports whether a request comes from a script rather than from a
// person following a link or sending a form. Browsers say so in Sec-Fetch-Mode:
// every mode but "navigate" is a script (fetch, XMLHttpRequest) or a page
// loading a resource. A client that does not send it is treated as a person.
func isScript(r *http.Request) bool {
	mode := r.Header.Get("Sec-Fetch-Mode")
	return mode != "" && mode != "navigate"
}

// sendToLogin answers an anonymous request to a private route. Pages go to the
// sign-in form and come back afterwards; htmx requests are redirected by htmx
// itself, since it would otherwise swap the login page into a fragment. A
// script is told plainly that it is not signed in: a redirect would be followed
// to the sign-in page, whose HTML it would take for the answer.
//
// Pattern: Client-Aware Refusal
func (a *App) sendToLogin(w http.ResponseWriter, r *http.Request) {
	switch {
	case wantsEventStream(r):
		// A redirect would make the event-stream extension retry forever; say
		// what happened inside the stream instead.
		a.sessionEndedStream(w, r)
	case fromHTMX(r):
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusOK)
	case isScript(r):
		w.Header().Set("WWW-Authenticate", `Cookie realm="Agent Web UI"`)
		http.Error(w, "You are not signed in.", http.StatusUnauthorized)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

type loginPage struct {
	basePage
	Email string
	Next  string
	Error string
}

const (
	loginFailed = "Wrong email or password."
	loginBusy   = "Too many people are signing in right now. Try again in a few seconds."
)

// busyRetrySeconds is what a sign-in turned away for being too busy is told to
// wait: about as long as a hash waits for its turn before it gives up.
const busyRetrySeconds = "5"

func (a *App) loginForm(w http.ResponseWriter, r *http.Request) {
	if _, signedIn := UserFrom(r.Context()); signedIn {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.renderLogin(w, r, http.StatusOK, loginPage{Next: safeNext(r.URL.Query().Get("next"))})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		if !writeBodyError(w, err) {
			http.Error(w, "bad form", http.StatusBadRequest)
		}
		return
	}
	email, next := r.PostFormValue("email"), safeNext(r.PostFormValue("next"))

	session, err := a.svc.Auth.Login(r.Context(), email, r.PostFormValue("password"))
	switch {
	case errors.Is(err, domain.ErrInvalidCredentials):
		// One message for every kind of failure; the password is never echoed.
		a.renderLogin(w, r, http.StatusUnprocessableEntity, loginPage{Email: email, Next: next, Error: loginFailed})
		return
	case errors.Is(err, domain.ErrBusy):
		// The password was not found wrong, it was not checked, and nobody is told
		// which emails have an account: the answer is the same for every one.
		w.Header().Set("Retry-After", busyRetrySeconds)
		a.renderLogin(w, r, http.StatusServiceUnavailable, loginPage{Email: email, Next: next, Error: loginBusy})
		return
	case err != nil:
		a.serverError(w, r, err)
		return
	}

	http.SetCookie(w, a.sessionCookie(session))
	redirect(w, r, next)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := a.svc.Auth.Logout(r.Context(), c.Value); err != nil {
			// The browser is signed out either way: its cookie is cleared below.
			// The session row stays until it expires, so a copy of the token
			// still signs in until then, which is why this is logged as an error.
			a.log.ErrorContext(r.Context(), "ending a session failed", "error", err)
		}
	}
	http.SetCookie(w, a.expiredCookie())
	redirect(w, r, "/login")
}

func (a *App) renderLogin(w http.ResponseWriter, r *http.Request, status int, page loginPage) {
	page.basePage = a.base(r)
	if err := a.renderer.Page(w, r, status, "login", page); err != nil {
		a.serverError(w, r, err)
	}
}

func (a *App) sessionCookie(s service.AuthSession) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Value:    s.Token,
		Path:     "/",
		Expires:  s.ExpiresAt,
		MaxAge:   max(1, int(time.Until(s.ExpiresAt).Seconds())),
		HttpOnly: true,
		Secure:   a.opts.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}

func (a *App) expiredCookie() *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookie,
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.opts.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}

// redirect sends the browser to target: with a status for plain requests and
// with HX-Redirect for htmx ones, which would otherwise follow the redirect
// inside an XHR and swap the target page into the wrong place.
func redirect(w http.ResponseWriter, r *http.Request, target string) {
	// A boosted request wants the redirect followed inside the XHR: htmx then
	// swaps the target page in without reloading the document.
	if fromHTMX(r) && r.Header.Get("HX-Boosted") != "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// safeNext accepts only a plain path on this site, so "next" cannot be turned
// into an open redirect. Anything else becomes "/".
func safeNext(raw string) string {
	const fallback = "/"
	if raw == "" || len(raw) > 512 || raw[0] != '/' {
		return fallback
	}
	if strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\r\n\t") {
		return fallback
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return fallback
	}
	if u.Path == "/login" || u.Path == "/logout" {
		return fallback // would loop straight back
	}
	return raw
}

// sessionEndedStream answers an event-stream request whose session is gone with
// a message in the reply's place, then closes the stream.
func (a *App) sessionEndedStream(w http.ResponseWriter, r *http.Request) {
	sse, err := newSSEWriter(w)
	if err != nil {
		return
	}
	// The mux has not run yet, so path values are not set: read the ids from the path.
	var convID, msgID uuid.UUID
	if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); len(parts) >= 4 {
		convID, _ = parseUUID(parts[1])
		msgID, _ = parseUUID(parts[3])
	}
	html, err := a.replyHTML(convID, msgID, domain.StatusFailed, "", sessionEnded, domain.Meta{})
	if err != nil {
		a.log.ErrorContext(r.Context(), "rendering a reply failed", "error", err)
		return
	}
	if sse.event("delta", html) == nil {
		_ = sse.event("done", "done")
	}
}
