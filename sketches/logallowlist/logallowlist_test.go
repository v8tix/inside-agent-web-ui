package logallowlist

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// served sends one request through Requests and returns the line that was logged.
// The request carries a word in its query, in a header, in a cookie and in its form.
func served(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	h := Requests(slog.New(slog.NewJSONHandler(&out, nil)), http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The request holds its form by the time the line is written.
		if err := r.ParseForm(); err != nil || r.PostForm.Get("card") != "word-in-the-form" {
			t.Fatalf("the form was not read: %v", err)
		}
	}))
	req := httptest.NewRequest(http.MethodPost, "/orders?token=word-in-the-query", strings.NewReader("card=word-in-the-form"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer word-in-the-header")
	req.AddCookie(&http.Cookie{Name: "session", Value: "word-in-the-cookie"})

	h.ServeHTTP(httptest.NewRecorder(), req)

	return out.String()
}

func TestTheLineHoldsTheNamedAttributesAndNoOther(t *testing.T) {
	var line map[string]any
	if err := json.Unmarshal([]byte(served(t)), &line); err != nil {
		t.Fatalf("the line is not one JSON object: %v", err)
	}

	got := slices.Sorted(maps.Keys(line))

	// What the handler adds (time, level, msg) and the four attributes of the list.
	want := []string{"duration", "level", "method", "msg", "path", "remote", "time"}
	if !slices.Equal(got, want) {
		t.Errorf("the line holds %v, want %v: the list of attributes changed", got, want)
	}
}

func TestNothingThatTheRequestCarriesReachesTheLine(t *testing.T) {
	line := served(t)

	for _, word := range []string{"word-in-the-query", "word-in-the-header", "word-in-the-cookie", "word-in-the-form"} {
		if strings.Contains(line, word) {
			t.Errorf("the line holds %q: %s", word, line)
		}
	}
	if !strings.Contains(line, `"path":"/orders"`) {
		t.Errorf("the line does not hold the path without its query: %s", line)
	}
}
