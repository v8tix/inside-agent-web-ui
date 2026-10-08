// Package threecontexts gives a stream its three contexts: the client's, the run's and the server's.
package threecontexts

import "context"

// Split returns the context of a stream and the context of the run that feeds it.
// When the client leaves, both end. When the server shuts down, only the run ends,
// so the stream stays open long enough to tell the client how the run ended.
// The caller defers release, which also takes the hook off the shutdown context.
func Split(client, shutdown context.Context) (stream, run context.Context, release func()) {
	stream, endStream := context.WithCancel(client)
	run, endRun := context.WithCancel(stream)
	unhook := context.AfterFunc(shutdown, endRun)
	return stream, run, func() {
		unhook()
		endRun()
		endStream()
	}
}
