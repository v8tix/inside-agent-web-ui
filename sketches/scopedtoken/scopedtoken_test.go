package scopedtoken

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// recorder is a server that sends the Authorization header of each request to seen
// and, when next is set, redirects to it.
func recorder(t *testing.T, next string) (url string, seen chan string) {
	t.Helper()
	seen = make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		if next != "" {
			http.Redirect(w, r, next, http.StatusFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, seen
}

func get(t *testing.T, client *http.Client, req *http.Request) {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

func TestTheTokenStopsAtARedirectToAnotherHost(t *testing.T) {
	cdn, atCDN := recorder(t, "")       // the same address as the API, on another port
	api, atAPI := recorder(t, cdn+"/b") // the API answers with a redirect to the CDN
	client := &http.Client{Transport: Transport{Token: "made-up", Hosts: []string{api}, Base: http.DefaultTransport}}
	req, _ := http.NewRequest(http.MethodGet, api+"/file", nil)
	// A header that the caller set by hand: the Go client would forward it to another port of
	// the same host, so an empty header at the CDN shows that RoundTrip ran for the redirect too.
	req.Header.Set("Authorization", "Bearer stale")

	get(t, client, req)

	if got := <-atAPI; got != "Bearer made-up" {
		t.Errorf("the API got %q, want the token", got)
	}
	if got := <-atCDN; got != "" {
		t.Errorf("the CDN got %q, want no token", got)
	}
}

func TestAHostThatWasNotNamedGetsNoToken(t *testing.T) {
	other, atOther := recorder(t, "")
	client := &http.Client{Transport: Transport{Token: "made-up", Hosts: []string{"https://api.example"}, Base: http.DefaultTransport}}
	req, _ := http.NewRequest(http.MethodGet, other+"/file", nil)
	req.Header.Set("Authorization", "Bearer made-up") // a caller that sets the header by hand
	before := req.Header.Get("Authorization")

	get(t, client, req)

	if got := <-atOther; got != "" {
		t.Errorf("a host that was not named got %q, want no token", got)
	}
	if req.Header.Get("Authorization") != before {
		t.Error("the request of the caller was changed")
	}
}
