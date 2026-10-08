// Command eval is agent-eval: it drives the Web UI in a real browser the way a
// person does, asks it GAIA-style problems, and scores both the agent's answers
// and the UI.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := newCLI(os.Stdout, os.Stderr).run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
