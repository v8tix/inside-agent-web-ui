// Command web runs the agent-web-ui HTTP server.
//
//	web [--env FILE] [healthcheck [URL]]
//
// The settings come from .env in the directory it is started in, or from the
// file that --env names, which has to be there, and a variable of the
// environment wins over the file.
//
// "web healthcheck [URL]" exits 0 when a running server is ready, for
// container health checks in images that have no curl.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/v8tix/inside-agent-web-ui/internal/app"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/logging"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
)

func main() {
	os.Exit(command(os.Args[1:], os.Stderr))
}

const (
	usage = "usage: agent-web-ui [--env FILE] [healthcheck [URL]]"
	// defaultEnvFile is the settings file when none is named. It may be missing:
	// in a container the settings are in the environment.
	defaultEnvFile = ".env"
)

// command runs one command line and returns the exit status: 0, 1 when the
// server or the health check fails, 2 when the command line is wrong.
func command(args []string, stderr io.Writer) int {
	envFile, words, err := arguments(args)
	if err == nil && envFile != defaultEnvFile {
		// A file that is named has to be there. Without it the settings would be
		// the environment's alone, and nobody would be told.
		if _, missing := os.Stat(envFile); errors.Is(missing, fs.ErrNotExist) {
			err = fmt.Errorf("no settings file %q", envFile)
		}
	}
	switch {
	case errors.Is(err, flag.ErrHelp):
		_, _ = fmt.Fprintln(stderr, usage)
		return 0
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "agent-web-ui: %v\n%s\n", err, usage)
		return 2
	}
	if len(words) > 0 {
		// The same settings the server reads: the file, and the environment over it.
		lookup, err := envcfg.EnvLookup(envFile)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "unhealthy:", err)
			return 1
		}
		return healthcheck(words[1:], func(key string) string { v, _ := lookup(key); return v })
	}
	if err := run(envFile); err != nil {
		_, _ = fmt.Fprintln(stderr, "agent-web-ui:", err)
		return 1
	}
	return 0
}

// arguments reads the command line: the settings file, and the words after the
// flags. The server takes none; the health check is its name and the address
// it may be given.
func arguments(args []string) (envFile string, words []string, err error) {
	flags := flag.NewFlagSet("agent-web-ui", flag.ContinueOnError)
	flags.SetOutput(io.Discard) // command says what is wrong
	flags.StringVar(&envFile, "env", defaultEnvFile, "read the settings from this file")
	if err := flags.Parse(args); err != nil {
		return "", nil, err
	}
	words = flags.Args()
	switch {
	case len(words) == 0:
	case words[0] != "healthcheck":
		return "", nil, fmt.Errorf("unknown argument %q", words[0])
	case slices.ContainsFunc(words, func(word string) bool { return strings.HasPrefix(word, "-") }):
		return "", nil, errors.New("the flags come before healthcheck")
	case len(words) > 2:
		return "", nil, errors.New("healthcheck takes one address")
	}
	return envFile, words, nil
}

// run serves until a signal stops it, with the settings of envFile and of the
// environment over it.
func run(envFile string) error {
	cfg, err := config.LoadEnv(envFile)
	if err != nil {
		return err
	}
	log := logging.New(cfg.App, os.Stdout)
	log.Info("starting", "config", cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, cfg, log)
}

// serve takes the port before anything else: building the application recovers
// replies a crash left streaming, which must not happen in a process that then
// finds its port taken by a running one (see app.Serve).
//
// Pattern: Listener First
func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	ln, err := net.Listen("tcp", cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTP.Addr, err)
	}
	defer func() { _ = ln.Close() }()
	return app.Serve(ctx, cfg, log, ln)
}
