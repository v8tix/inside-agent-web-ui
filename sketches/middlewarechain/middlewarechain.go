// Package middlewarechain wraps a handler in layers, outermost first.
package middlewarechain

import (
	"net/http"
	"slices"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies mws to h. The first one is the outermost: it sees every request
// first and every answer last, whatever an inner layer refuses. Write the reason
// for each position in a comment next to the list that calls Chain.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for _, mw := range slices.Backward(mws) {
		h = mw(h)
	}
	return h
}
