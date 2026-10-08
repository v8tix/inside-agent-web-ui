package middlewarechain

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// stamp is an outer layer: it marks every answer. refuse is an inner layer: it
// answers 403 and never calls the next handler.
func stamp(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Stamp", "yes")
		next.ServeHTTP(w, r)
	})
}

func refuse(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	})
}

func answer(h http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec
}

func TestAnOuterLayerSeesWhatAnInnerLayerRefuses(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	right := answer(Chain(ok, stamp, refuse))
	if right.Code != http.StatusForbidden || right.Header().Get("X-Stamp") != "yes" {
		t.Fatalf("the refusal %d lacks the stamp of the outer layer", right.Code)
	}
	swapped := answer(Chain(ok, refuse, stamp))
	if swapped.Header().Get("X-Stamp") != "" {
		t.Fatal("the test cannot tell the two orders apart")
	}
}
