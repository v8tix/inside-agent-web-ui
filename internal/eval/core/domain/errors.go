package domain

import "errors"

// Errors the ports return and the runner understands.
var (
	// ErrReplyTimeout means the reply did not reach a final status in time.
	ErrReplyTimeout = errors.New("the reply did not finish in time")
	// ErrDriverDied means the browser session is gone (Chrome or the MCP server
	// exited). The runner starts a new driver and tries the attempt once more.
	ErrDriverDied = errors.New("the browser session ended")
	// ErrLoginFailed means the Web UI did not accept the credentials.
	ErrLoginFailed = errors.New("sign-in failed")
	// ErrContractVersion means the Web UI speaks a different UI contract
	// version than this evaluator was built for. Nothing can be trusted then.
	ErrContractVersion = errors.New("the Web UI speaks a different UI contract version")
)
