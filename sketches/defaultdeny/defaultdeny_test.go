package defaultdeny

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestARouteNobodyListedIsPrivate(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	mux := http.NewServeMux()
	mux.Handle("GET /health", ok)
	mux.Handle("GET /admin", ok)
	known := func(r *http.Request) bool { return r.Header.Get("Authorization") != "" }
	guard := Guard(Paths("/health", "/static/"), known, mux)
	mux.Handle("GET /added/later", ok) // a new route: the public list is not touched

	code := func(path string, signedIn bool) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if signedIn {
			req.Header.Set("Authorization", "token")
		}
		rec := httptest.NewRecorder()
		guard.ServeHTTP(rec, req)
		return rec.Code
	}
	for path, want := range map[string]int{
		"/health": 200, "/static/app.css": 404, // public: the guard lets them through to the mux
		"/admin": 303, "/added/later": 303, "/nothing/here": 303, // private, listed or not
	} {
		if got := code(path, false); got != want {
			t.Errorf("anonymous %s: %d, want %d", path, got, want)
		}
	}
	if code("/admin", true) != 200 || code("/added/later", true) != 200 || code("/nothing/here", true) != 404 {
		t.Error("a signed-in visitor reaches the routes, and a path with no route is a 404")
	}
}
