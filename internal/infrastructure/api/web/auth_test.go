package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/core/port/service/mocks"
)

var alice = domain.User{ID: uuid.NewV7(), Email: "alice@example.com"}

func cookieNamed(res *http.Response, name string) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func postForm(path string, form url.Values, headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func serve(t *testing.T, authMock *mocks.Auth, opts Options, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	testAppWith(t, Services{Auth: authMock}, opts).Routes().ServeHTTP(rec, req)
	return rec
}

func TestLoginForm(t *testing.T) {
	t.Run("shows the form with the hooks the evaluator uses", func(t *testing.T) {
		rec := serve(t, mocks.NewAuth(t), Options{}, httptest.NewRequest(http.MethodGet, "/login?next=%2Fc%2F123", nil))

		require.Equal(t, http.StatusOK, rec.Code)
		body := rec.Body.String()
		for _, hook := range []string{`data-testid="login-email"`, `data-testid="login-password"`, `data-testid="login-submit"`} {
			assert.Contains(t, body, hook)
		}
		assert.Contains(t, body, `value="/c/123"`, "the destination travels with the form")
		assert.Contains(t, body, `<label for="login-email" class="form-label">Email</label>`, "controls have visible labels")
		assert.NotContains(t, body, `data-testid="logout"`, "nobody is signed in")
	})

	t.Run("a signed-in visitor goes straight to the app", func(t *testing.T) {
		authMock := mocks.NewAuth(t)
		authMock.On("Authenticate", mock.Anything, "tok").Return(alice, nil)
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})

		rec := serve(t, authMock, Options{}, req)

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, "/", rec.Header().Get("Location"))
	})
}

func TestLogin_Success(t *testing.T) {
	expires := time.Now().Add(24 * time.Hour)
	tests := []struct {
		name     string
		form     url.Values
		headers  map[string]string
		opts     Options
		wantLoc  string
		wantHX   string
		wantSec  bool
		wantCode int
	}{
		{"goes home by default", url.Values{"email": {"alice@example.com"}, "password": {"pw"}}, nil, Options{}, "/", "", false, http.StatusSeeOther},
		{"returns to where the visitor was headed", url.Values{"email": {"alice@example.com"}, "password": {"pw"}, "next": {"/c/123?x=1"}}, nil, Options{}, "/c/123?x=1", "", false, http.StatusSeeOther},
		{"cookie is Secure over https", url.Values{"email": {"alice@example.com"}, "password": {"pw"}}, nil, Options{SecureCookies: true}, "/", "", true, http.StatusSeeOther},
		{"htmx gets HX-Redirect, not a 303", url.Values{"email": {"alice@example.com"}, "password": {"pw"}, "next": {"/c/9"}}, map[string]string{"HX-Request": "true"}, Options{}, "", "/c/9", false, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authMock := mocks.NewAuth(t)
			authMock.On("Login", mock.Anything, "alice@example.com", "pw").
				Return(service.AuthSession{Token: "the-token", ExpiresAt: expires, User: alice}, nil)

			rec := serve(t, authMock, tc.opts, postForm("/login", tc.form, tc.headers))

			assert.Equal(t, tc.wantCode, rec.Code)
			assert.Equal(t, tc.wantLoc, rec.Header().Get("Location"))
			assert.Equal(t, tc.wantHX, rec.Header().Get("HX-Redirect"))

			c := cookieNamed(rec.Result(), sessionCookie)
			require.NotNil(t, c)
			assert.Equal(t, "the-token", c.Value)
			assert.True(t, c.HttpOnly, "scripts must not read the session")
			assert.Equal(t, http.SameSiteLaxMode, c.SameSite)
			assert.Equal(t, "/", c.Path)
			assert.Equal(t, tc.wantSec, c.Secure)
			assert.InDelta(t, 24*3600, c.MaxAge, 5, "the cookie lives as long as the session")
			assert.WithinDuration(t, expires, c.Expires, 2*time.Second)
		})
	}
}

func TestLogin_Failure(t *testing.T) {
	tests := []struct {
		name string
		form url.Values
	}{
		{"wrong credentials", url.Values{"email": {"alice@example.com"}, "password": {"wrong"}}},
		{"unknown user", url.Values{"email": {"ghost@example.com"}, "password": {"pw"}}},
		{"empty form", url.Values{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authMock := mocks.NewAuth(t)
			authMock.On("Login", mock.Anything, tc.form.Get("email"), tc.form.Get("password")).
				Return(service.AuthSession{}, domain.ErrInvalidCredentials)

			rec := serve(t, authMock, Options{}, postForm("/login", tc.form, nil))

			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
			body := rec.Body.String()
			assert.Contains(t, body, loginFailed, "one message for every failure")
			assert.Contains(t, body, `role="alert"`)
			assert.Nil(t, cookieNamed(rec.Result(), sessionCookie), "no session on failure")
			if email := tc.form.Get("email"); email != "" {
				assert.Contains(t, body, `value="`+email+`"`, "the email is kept so the visitor can fix the password")
			}
			if pw := tc.form.Get("password"); pw != "" {
				assert.NotContains(t, body, pw, "the password is never sent back")
			}
		})
	}

	t.Run("a hostile email is escaped when shown again", func(t *testing.T) {
		hostile := `"><script>alert(1)</script>`
		authMock := mocks.NewAuth(t)
		authMock.On("Login", mock.Anything, hostile, "pw").Return(service.AuthSession{}, domain.ErrInvalidCredentials)

		rec := serve(t, authMock, Options{}, postForm("/login", url.Values{"email": {hostile}, "password": {"pw"}}, nil))

		assert.NotContains(t, rec.Body.String(), "<script>alert(1)</script>")
	})

	t.Run("a server too busy to check the password says so and when to try again", func(t *testing.T) {
		authMock := mocks.NewAuth(t)
		authMock.On("Login", mock.Anything, "alice@example.com", "canary-pw").
			Return(service.AuthSession{}, fmt.Errorf("checking the password: %w", domain.ErrBusy))

		rec := serve(t, authMock, Options{}, postForm("/login", url.Values{"email": {"alice@example.com"}, "password": {"canary-pw"}}, nil))

		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Equal(t, "5", rec.Header().Get("Retry-After"))
		body := rec.Body.String()
		assert.Contains(t, body, loginBusy)
		assert.NotContains(t, body, loginFailed, "the password was not found wrong; it was not checked")
		assert.Contains(t, body, `role="alert"`)
		assert.Contains(t, body, `value="alice@example.com"`, "the email is kept")
		assert.NotContains(t, body, "canary-pw", "the password is never sent back")
		assert.Nil(t, cookieNamed(rec.Result(), sessionCookie))
	})

	t.Run("a broken service is a 500 with no detail", func(t *testing.T) {
		authMock := mocks.NewAuth(t)
		authMock.On("Login", mock.Anything, mock.Anything, mock.Anything).Return(service.AuthSession{}, errors.New("db password is hunter2"))

		rec := serve(t, authMock, Options{}, postForm("/login", url.Values{"email": {"a@b.co"}, "password": {"pw"}}, nil))

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.NotContains(t, rec.Body.String(), "hunter2")
		assert.Nil(t, cookieNamed(rec.Result(), sessionCookie))
	})
}

func TestSafeNext(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"/c/123", "/c/123"},
		{"/c/123?x=1&y=2", "/c/123?x=1&y=2"},
		{"/", "/"},
		{"", "/"},
		{"c/123", "/"},
		{"//evil.example", "/"},
		{"//evil.example/path", "/"},
		{"https://evil.example", "/"},
		{"http://evil.example/", "/"},
		{"/\\evil.example", "/"},
		{"\\\\evil.example", "/"},
		{"javascript:alert(1)", "/"},
		{"/path\r\nSet-Cookie: x=1", "/"},
		{"/path\twith-tab", "/"},
		{"/login", "/"},
		{"/login?next=/x", "/"},
		{"/logout", "/"},
		{"/" + strings.Repeat("a", 600), "/"},
		{"/%2F%2Fevil.example", "/%2F%2Fevil.example"}, // still a path on this site, not a host
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			assert.Equal(t, tc.want, safeNext(tc.raw))
		})
	}
}

func TestLogout(t *testing.T) {
	tests := []struct {
		name      string
		cookie    string
		logoutErr error
		wantCall  bool
	}{
		{"ends the session and clears the cookie", "tok", nil, true},
		{"a failing store still signs the browser out", "tok", errors.New("db down"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			authMock := mocks.NewAuth(t)
			if tc.wantCall {
				authMock.On("Logout", mock.Anything, tc.cookie).Return(tc.logoutErr)
			}
			authMock.On("Authenticate", mock.Anything, mock.Anything).Return(alice, nil).Maybe()
			req := postForm("/logout", url.Values{}, nil)
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tc.cookie})
			}

			rec := serve(t, authMock, Options{}, req)

			assert.Equal(t, http.StatusSeeOther, rec.Code)
			assert.Equal(t, "/login", rec.Header().Get("Location"))
			c := cookieNamed(rec.Result(), sessionCookie)
			require.NotNil(t, c)
			assert.Negative(t, c.MaxAge, "the cookie is removed")
		})
	}

	t.Run("without a session there is nothing to end: back to sign-in", func(t *testing.T) {
		rec := serve(t, mocks.NewAuth(t), Options{}, postForm("/logout", url.Values{}, nil))

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, "/login", rec.Header().Get("Location"))
	})

	t.Run("logging out is only a POST", func(t *testing.T) {
		rec := serve(t, mocks.NewAuth(t), Options{}, httptest.NewRequest(http.MethodGet, "/logout", nil))

		assert.NotEqual(t, http.StatusOK, rec.Code, "a GET (a link, an image tag) must not sign anyone out")
	})
}

func TestAuthenticateMiddleware(t *testing.T) {
	t.Run("private routes turn anonymous visitors away", func(t *testing.T) {
		tests := []struct {
			name       string
			method     string
			path       string
			headers    map[string]string
			wantStatus int
			wantLoc    string
			wantHX     string
		}{
			{"a page", http.MethodGet, "/", nil, http.StatusSeeOther, "/login?next=%2F", ""},
			{"a page with a query", http.MethodGet, "/c/123?x=1", nil, http.StatusSeeOther, "/login?next=%2Fc%2F123%3Fx%3D1", ""},
			{"a form post", http.MethodPost, "/c", nil, http.StatusSeeOther, "/login", ""},
			{"a stop request", http.MethodPost, "/c/123/m/456/stop", nil, http.StatusSeeOther, "/login", ""},
			{"a regenerate request", http.MethodPost, "/c/123/m/456/regenerate", nil, http.StatusSeeOther, "/login", ""},
			{"the raw text of a message", http.MethodGet, "/c/123/m/456/raw", nil, http.StatusSeeOther, "/login?next=%2Fc%2F123%2Fm%2F456%2Fraw", ""},
			{"a rename", http.MethodPatch, "/c/123", nil, http.StatusSeeOther, "/login", ""},
			{"a delete", http.MethodDelete, "/c/123", nil, http.StatusSeeOther, "/login", ""},
			{"the rename form", http.MethodGet, "/c/123/rename", nil, http.StatusSeeOther, "/login?next=%2Fc%2F123%2Frename", ""},
			{"the delete dialog", http.MethodGet, "/c/123/delete", nil, http.StatusSeeOther, "/login?next=%2Fc%2F123%2Fdelete", ""},
			{"an htmx request", http.MethodGet, "/", map[string]string{"HX-Request": "true"}, http.StatusOK, "", "/login"},
			{"an unknown path is private too", http.MethodGet, "/nothing-here", nil, http.StatusSeeOther, "/login?next=%2Fnothing-here", ""},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequest(tc.method, tc.path, nil)
				for k, v := range tc.headers {
					req.Header.Set(k, v)
				}

				rec := serve(t, mocks.NewAuth(t), Options{}, req) // no cookie: the service is never asked

				assert.Equal(t, tc.wantStatus, rec.Code)
				assert.Equal(t, tc.wantLoc, rec.Header().Get("Location"))
				assert.Equal(t, tc.wantHX, rec.Header().Get("HX-Redirect"))
			})
		}
	})

	// A redirect is for a person following a link. A script that fetches the raw
	// text of a reply (the Copy button, the evaluator) follows it to the sign-in
	// page, gets a 200, and takes the page's HTML for the reply.
	t.Run("a script's request is told it is not signed in, not sent to the sign-in page", func(t *testing.T) {
		tests := []struct {
			name    string
			method  string
			headers map[string]string
		}{
			{"a fetch of the raw text", http.MethodGet, map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty", "Accept": "text/plain"}},
			{"a fetch that posts", http.MethodPost, map[string]string{"Sec-Fetch-Mode": "same-origin", "Sec-Fetch-Dest": "empty"}},
			{"a request with no destination to show it on", http.MethodGet, map[string]string{"Sec-Fetch-Mode": "no-cors"}},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				req := httptest.NewRequest(tc.method, "/c/123/m/456/raw", nil)
				for k, v := range tc.headers {
					req.Header.Set(k, v)
				}

				rec := serve(t, mocks.NewAuth(t), Options{}, req)

				assert.Equal(t, http.StatusUnauthorized, rec.Code)
				assert.Empty(t, rec.Header().Get("Location"), "nothing to follow")
				assert.NotEmpty(t, rec.Header().Get("WWW-Authenticate"), "a 401 names how to sign in")
				assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
				assert.NotContains(t, rec.Body.String(), "<")
			})
		}
	})

	t.Run("a person's navigation still goes to the sign-in page", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/c/123", nil)
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Dest", "document")

		rec := serve(t, mocks.NewAuth(t), Options{}, req)

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, "/login?next=%2Fc%2F123", rec.Header().Get("Location"))
	})

	t.Run("public routes need no session", func(t *testing.T) {
		for _, path := range []string{"/healthz", "/login", "/favicon.ico", "/static/css/app.css"} {
			t.Run(path, func(t *testing.T) {
				rec := serve(t, mocks.NewAuth(t), Options{}, httptest.NewRequest(http.MethodGet, path, nil))

				assert.Equal(t, http.StatusOK, rec.Code)
			})
		}
	})

	// A page of the app loads a dozen files, and every request that carried the
	// cookie cost two queries to look the session up, for files that are the same
	// for everyone. With the database down, a browser holding a cookie would even
	// be told 500 for its stylesheet.
	t.Run("files and probes are served without looking the session up", func(t *testing.T) {
		for _, path := range []string{"/static/css/app.css", "/static/js/app.js", "/favicon.ico", "/healthz", "/readyz"} {
			t.Run(path, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})

				rec := serve(t, mocks.NewAuth(t), Options{}, req) // the service is never asked

				assert.Equal(t, http.StatusOK, rec.Code)
			})
		}
	})

	t.Run("a valid session puts the user on the page", func(t *testing.T) {
		h := newChatHarness(t)
		h.expectSidebar()

		rec := h.do(http.MethodGet, "/", nil, nil)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), "alice@example.com")
		assert.Contains(t, rec.Body.String(), `data-testid="logout"`)
	})

	t.Run("a stale cookie is cleared and the visitor sent to sign in", func(t *testing.T) {
		authMock := mocks.NewAuth(t)
		authMock.On("Authenticate", mock.Anything, "old").Return(domain.User{}, domain.ErrInvalidCredentials)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "old"})

		rec := serve(t, authMock, Options{}, req)

		assert.Equal(t, http.StatusSeeOther, rec.Code)
		c := cookieNamed(rec.Result(), sessionCookie)
		require.NotNil(t, c)
		assert.Negative(t, c.MaxAge)
	})

	t.Run("a stale cookie does not break public routes", func(t *testing.T) {
		authMock := mocks.NewAuth(t)
		authMock.On("Authenticate", mock.Anything, "old").Return(domain.User{}, domain.ErrInvalidCredentials)
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "old"})

		rec := serve(t, authMock, Options{}, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("a failing auth service is a 500, not a login redirect", func(t *testing.T) {
		authMock := mocks.NewAuth(t)
		authMock.On("Authenticate", mock.Anything, "tok").Return(domain.User{}, errors.New("db down"))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "tok"})

		rec := serve(t, authMock, Options{}, req)

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})

	t.Run("an empty cookie is the same as none", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: ""})

		rec := serve(t, mocks.NewAuth(t), Options{}, req)

		assert.Equal(t, http.StatusSeeOther, rec.Code)
	})
}

func TestUserFrom(t *testing.T) {
	_, ok := UserFrom(context.Background())
	assert.False(t, ok)
}
