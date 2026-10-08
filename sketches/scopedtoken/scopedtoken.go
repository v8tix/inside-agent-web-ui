// Package scopedtoken sends a bearer token to the hosts it was issued for and to no other.
package scopedtoken

import (
	"net/http"
	"net/url"
	"strings"
)

// Transport adds Token to a request whose scheme and host, port included, equal those of an
// address in Hosts. Every other request goes without it, and so does a redirect to another host.
type Transport struct {
	Token string
	Hosts []string // base addresses, such as https://api.example
	Base  http.RoundTripper
}

// RoundTrip runs for the first request and again for each redirect that the client follows.
func (t Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context()) // a RoundTripper must not change the request it is given
	req.Header.Del("Authorization")
	for _, host := range t.Hosts {
		b, err := url.Parse(host)
		if err == nil && b.Scheme == req.URL.Scheme && strings.EqualFold(b.Host, req.URL.Host) {
			req.Header.Set("Authorization", "Bearer "+t.Token)
		}
	}
	return t.Base.RoundTrip(req)
}
