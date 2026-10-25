# Sketches

Small, self-contained reference implementations of this codebase's patterns.
Each one shows how *any* Go web application could use a pattern — not just
this one — so none of them import anything from the rest of this repository.

Every sketch follows the same shape:

- **One file and its test.** `sketches/<name>/<name>.go` plus
  `sketches/<name>/<name>_test.go`, nothing else.
- **Standard library only.** No third-party imports, and none of them import
  each other.
- **Small.** Each one is built to show a single idea, not a feature.
- **A test that means something.** Every sketch has at least one test that
  fails if the idea it shows is broken.

They're part of this module (no separate `go.mod`), so `go build ./...`,
`go vet ./...` and `golangci-lint run ./...` all cover them, but they're not
part of the application and aren't included in its test coverage targets.

| Sketch | Shows |
|--------|-------|
| `appendrows` | reading an append-only file of JSON lines such as `{"key":"a","done":true}` |
| `defaultdeny` | protecting every route that isn't explicitly listed as public |
| `deprule` | keeping an import rule as data, so a test can enforce it |
| `detachedsave` | saving a result after the request that asked for it is gone |
| `digestsessions` | issuing a random token and keeping only its digest |
| `finalorincomplete` | turning a stream that ends without its final event into an error |
| `interval` | reporting a share with its 95% Wilson score interval, so a small sample isn't read as precise |
| `iterchan` | putting an iterator behind a channel, so one loop can wait on its items and on the clock |
| `lazybucket` | limiting a rate with a token bucket that needs no timer |
| `logallowlist` | logging each request as a fixed list of named attributes |
| `middlewarechain` | wrapping a handler in layers, outermost first |
| `outcometable` | mapping every way a job can end to one stored status and one sentence |
| `pacer` | spacing out the updates of a text that grows: the first goes at once, then at most one per interval, always with the newest text |
| `runregistry` | keeping the jobs a process runs, one for each id |
| `scopedtoken` | sending a bearer token only to the hosts it was issued for |
| `secret` | a string type that shows a placeholder to whatever asks it for its text |
| `shutdownhook` | ending long-lived responses as a server starts to shut down, so the wait for open connections ends before the grace period |
| `snapshots` | letting a fast producer feed a slow consumer, where only the newest whole value matters |
| `thirdresult` | giving a check three results, so "could not decide" is never counted as a pass |
| `threecontexts` | giving a stream its three contexts: the client's, the run's and the server's |
