package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHealthcheck(t *testing.T) {
	answering := func(status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/readyz", r.URL.Path)
			w.WriteHeader(status)
		}))
	}
	ready, notReady := answering(http.StatusOK), answering(http.StatusServiceUnavailable)
	defer ready.Close()
	defer notReady.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL + "/readyz"
	gone.Close()

	tests := []struct {
		name string
		url  string
		want int
	}{
		{"ready", ready.URL + "/readyz", 0},
		{"not ready", notReady.URL + "/readyz", 1},
		{"nothing listening", goneURL, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, healthcheck([]string{tc.url}, func(string) string { return "" }))
		})
	}
}

func TestPortOf(t *testing.T) {
	tests := []struct{ addr, want string }{
		{":8300", "8300"},
		{"0.0.0.0:9000", "9000"},
		{"127.0.0.1:18300", "18300"},
		{"[::]:8300", "8300"},
		{"", "8300"},
		{"localhost", "8300"},
		{":", "8300"},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) { assert.Equal(t, tc.want, portOf(tc.addr)) })
	}
}
