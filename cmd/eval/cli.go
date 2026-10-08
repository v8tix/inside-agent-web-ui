package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
)

// Exit codes.
const (
	exitOK       = 0
	exitUsage    = 1 // wrong usage or configuration
	exitUIFailed = 2 // a required UI check failed
	exitAccuracy = 3 // --min-accuracy was not reached
	exitInfra    = 4 // something outside the evaluator broke
)

// exitNoSettingsFile is the status for a file that --env names and that is not
// there. It is the status the scripts and agent-web-ui end with for the same
// mistake, and not exitUsage, the one of the other mistakes in a call.
const exitNoSettingsFile = 2

// defaultEnvFile is the settings file when none is named. It may be missing:
// the settings are then the environment's alone.
const defaultEnvFile = ".env"

// exitError carries the exit code a command wants.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func fail(code int, format string, args ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, args...)}
}

// usageError is a mistake in how the command was called.
type usageError struct {
	msg string
	// status is the exit status, when it is not exitUsage.
	status int
}

func (e usageError) Error() string { return e.msg }

// handler runs a command whose flags and configuration are already read.
type handler[O any] func(ctx context.Context, c *cli, cfg config.Config, opts O) error

// cli is the command line: its streams, how it reads configuration, and the
// handlers behind each command, which tests and later tasks replace.
type cli struct {
	stdout, stderr io.Writer
	loadConfig     func(envFile string) (config.Config, error)

	runCmd      handler[runOptions]
	selftestCmd handler[commonOptions]
	probeCmd    handler[probeOptions]
	reportCmd   handler[reportOptions]
	fetchCmd    handler[commonOptions]
	listCmd     handler[commonOptions]

	// buildRunner makes the runner of a run or a self-test, and buildProber the
	// probes' suite; tests replace them.
	buildRunner func(runnerSetup) service.Runner
	buildProber func(proberSetup) service.Prober
	// clock tells the time; tests replace it.
	clock func() time.Time
	// httpClient asks the Web UI whether it is ready; nil means the default.
	httpClient *http.Client
}

// say writes text to w. A failed write to a terminal has nowhere to be reported.
func (c *cli) say(w io.Writer, text string) { _, _ = io.WriteString(w, text) }

func newCLI(stdout, stderr io.Writer) *cli {
	return &cli{
		stdout: stdout, stderr: stderr,
		loadConfig:  loadConfigFile,
		runCmd:      runCommand,
		selftestCmd: selftestCommand,
		probeCmd:    probeCommand,
		reportCmd:   reportCommand,
		fetchCmd:    fetchCommand,
		listCmd:     listCommand,
		buildRunner: realRunner,
		buildProber: realProber,
		clock:       time.Now,
	}
}

// loadConfigFile reads the settings of envFile, and of the environment over
// it. A file that is named has to be there: without it the settings would be
// the environment's alone, and nobody would be told.
func loadConfigFile(envFile string) (config.Config, error) {
	if envFile != defaultEnvFile {
		if _, err := os.Stat(envFile); errors.Is(err, os.ErrNotExist) {
			return config.Config{}, usageError{msg: fmt.Sprintf("no settings file %q", envFile), status: exitNoSettingsFile}
		}
	}
	lookup, err := envcfg.EnvLookup(envFile)
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(lookup)
}

// --- options ---------------------------------------------------------------------

type commonOptions struct {
	EnvFile string
}

type runOptions struct {
	commonOptions
	Level       int // 0 means every level
	Limit       int
	TaskIDs     []string
	Repeats     int // 0 means what the configuration says
	Workers     int
	Timeout     time.Duration
	HumanTyping bool
	Resume      string
	IncludeText bool
	NoProbes    bool    // leave out the UI probes that follow the problems
	MinAccuracy float64 // 0 means no minimum
}

type probeOptions struct {
	commonOptions
	// Scripted says the agent is the scripted self-test agent, so the probes may
	// use the replies it has for them (a long one, one that fails once).
	Scripted bool
}

type reportOptions struct {
	commonOptions
	RunID  string
	Format string
}

// flags makes a flag set that reports its mistakes to the caller, not the screen.
func (c *cli) flags(name string, common *commonOptions) *flag.FlagSet {
	fs := flag.NewFlagSet("agent-eval "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&common.EnvFile, "env", defaultEnvFile, "read the settings from this file")
	return fs
}

// parse reads flags that may come before or after positional arguments, and
// returns the positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError{msg: strings.TrimPrefix(err.Error(), "flag provided but not defined: ") + flagHint(err)}
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func flagHint(err error) string {
	if strings.HasPrefix(err.Error(), "flag provided but not defined") {
		return ": unknown flag"
	}
	return ""
}

func (c *cli) runFlags(args []string) (runOptions, error) {
	var o runOptions
	fs := c.flags("run", &o.commonOptions)
	level := fs.String("level", "all", "1, 2, 3 or all")
	fs.IntVar(&o.Limit, "limit", 0, "ask at most this many problems (0: all)")
	fs.Func("task-id", "ask only this task; repeat for several", func(v string) error {
		for id := range strings.SplitSeq(v, ",") {
			if id = strings.TrimSpace(id); id != "" && !slices.Contains(o.TaskIDs, id) {
				o.TaskIDs = append(o.TaskIDs, id)
			}
		}
		return nil
	})
	fs.IntVar(&o.Repeats, "repeats", 0, "attempts per problem (default: EVAL_REPEATS)")
	fs.IntVar(&o.Workers, "workers", 0, "browsers at once (default: EVAL_WORKERS)")
	fs.DurationVar(&o.Timeout, "timeout", 0, "time allowed per attempt (default: EVAL_TASK_TIMEOUT)")
	fs.BoolVar(&o.HumanTyping, "human-typing", false, "type the prompt with a delay between keys")
	fs.StringVar(&o.Resume, "resume", "", "continue the run with this id, skipping finished attempts")
	fs.BoolVar(&o.IncludeText, "include-text", false, "keep question, truth and answer text in the results")
	fs.BoolVar(&o.NoProbes, "no-probes", false, "do not run the UI probes after the problems")
	fs.Float64Var(&o.MinAccuracy, "min-accuracy", 0, "exit with status 3 when accuracy is below this (0 to 1)")

	positional, err := parse(fs, args)
	if err != nil {
		return o, err
	}
	switch {
	case len(positional) > 0:
		return o, usageError{msg: fmt.Sprintf("run takes no arguments (got %q)", positional[0])}
	case o.Limit < 0:
		return o, usageError{msg: "--limit must not be negative"}
	case o.Repeats < 0 || o.Workers < 0:
		return o, usageError{msg: "--repeats and --workers must be positive"}
	case o.Timeout < 0:
		return o, usageError{msg: "--timeout must be positive"}
	case o.MinAccuracy < 0 || o.MinAccuracy > 1:
		return o, usageError{msg: "--min-accuracy must be between 0 and 1"}
	}
	switch *level {
	case "all":
	case "1", "2", "3":
		o.Level = int((*level)[0] - '0')
	default:
		return o, usageError{msg: fmt.Sprintf("--level must be 1, 2, 3 or all (got %q)", *level)}
	}
	return o, nil
}

func (c *cli) reportFlags(args []string) (reportOptions, error) {
	var o reportOptions
	fs := c.flags("report", &o.commonOptions)
	fs.StringVar(&o.Format, "format", "md", "md or json")
	positional, err := parse(fs, args)
	if err != nil {
		return o, err
	}
	switch {
	case len(positional) != 1:
		return o, usageError{msg: "report needs the id of a run: agent-eval report RUN_ID [--format md|json]"}
	case o.Format != "md" && o.Format != "json":
		return o, usageError{msg: fmt.Sprintf("--format must be md or json (got %q)", o.Format)}
	}
	o.RunID = positional[0]
	return o, nil
}

func (c *cli) probeFlags(args []string) (probeOptions, error) {
	var o probeOptions
	fs := c.flags("probe", &o.commonOptions)
	fs.BoolVar(&o.Scripted, "scripted", false, "the agent is the scripted self-test agent (adds the probe that needs it to fail once)")
	positional, err := parse(fs, args)
	if err != nil {
		return o, err
	}
	if len(positional) > 0 {
		return o, usageError{msg: fmt.Sprintf("probe takes no arguments (got %q)", positional[0])}
	}
	return o, nil
}

func (c *cli) commonFlags(name string, args []string) (commonOptions, error) {
	var o commonOptions
	fs := c.flags(name, &o)
	positional, err := parse(fs, args)
	if err != nil {
		return o, err
	}
	if len(positional) > 0 {
		return o, usageError{msg: fmt.Sprintf("%s takes no arguments (got %q)", name, positional[0])}
	}
	return o, nil
}

// --- dispatch --------------------------------------------------------------------

const usage = `agent-eval drives the Web UI like a person and scores the agent and the UI.

Usage:
  agent-eval <command> [flags]

Commands:
  datasets fetch   download the GAIA parquet files (needs HF_TOKEN)
  datasets list    show the local configs, splits, rows and attachments
  run              ask GAIA problems through the browser and score them
  selftest         run the synthetic problems against the scripted agent
  probe            run the UI probes and audits only
  report RUN_ID    rebuild the summaries of a finished run

Every command reads its settings from .env, or from the file that --env FILE
names, which has to be there, and from the environment over it.

Run flags:
  --level 1|2|3|all   --limit N   --task-id ID (repeatable)   --repeats R
  --workers W   --timeout D   --human-typing   --resume RUN_ID
  --include-text   --no-probes   --min-accuracy X

Probe flags:
  --scripted   the agent is the scripted self-test agent

Exit codes: 0 ok; 1 usage or configuration error; 2 a required UI check failed,
or no file behind --env; 3 --min-accuracy not reached, or the self-test found
other results than expected; 4 infrastructure failure; 130 interrupted.
`

// run executes one command line and returns the process exit code.
func (c *cli) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		c.say(c.stderr, usage)
		return exitUsage
	}
	name, rest := args[0], args[1:]
	switch name {
	case "help", "-h", "--help", "-help":
		c.say(c.stdout, usage)
		return exitOK
	}
	if wantsHelp(rest) {
		c.say(c.stdout, usage)
		return exitOK
	}

	err := c.dispatch(ctx, name, rest)
	var exit *exitError
	var usageErr usageError
	switch {
	case err == nil:
		return exitOK
	case errors.As(err, &exit):
		c.say(c.stderr, "agent-eval: "+exit.err.Error()+"\n")
		return exit.code
	case errors.As(err, &usageErr):
		c.say(c.stderr, fmt.Sprintf("agent-eval: %s\n\nTry: agent-eval help\n", usageErr))
		return cmp.Or(usageErr.status, exitUsage)
	default:
		c.say(c.stderr, "agent-eval: "+err.Error()+"\n")
		return exitInfra
	}
}

func wantsHelp(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return a == "-h" || a == "--help" || a == "-help" })
}

func (c *cli) dispatch(ctx context.Context, name string, args []string) error {
	switch name {
	case "run":
		o, err := c.runFlags(args)
		if err != nil {
			return err
		}
		cfg, err := c.config(o.EnvFile)
		if err != nil {
			return err
		}
		c.warn(cfg.RunWarnings)
		return c.runCmd(ctx, c, cfg, o)
	case "selftest":
		o, err := c.commonFlags(name, args)
		if err != nil {
			return err
		}
		cfg, err := c.config(o.EnvFile)
		if err != nil {
			return err
		}
		return c.selftestCmd(ctx, c, cfg, o)
	case "probe":
		o, err := c.probeFlags(args)
		if err != nil {
			return err
		}
		cfg, err := c.config(o.EnvFile)
		if err != nil {
			return err
		}
		return c.probeCmd(ctx, c, cfg, o)
	case "report":
		o, err := c.reportFlags(args)
		if err != nil {
			return err
		}
		cfg, err := c.config(o.EnvFile)
		if err != nil {
			return err
		}
		return c.reportCmd(ctx, c, cfg, o)
	case "datasets":
		return c.datasets(ctx, args)
	}
	return usageError{msg: fmt.Sprintf("unknown command %q", name)}
}

func (c *cli) datasets(ctx context.Context, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return usageError{msg: "datasets needs a subcommand: fetch or list"}
	}
	sub, rest := args[0], args[1:]
	if sub != "fetch" && sub != "list" {
		return usageError{msg: fmt.Sprintf("unknown datasets subcommand %q (fetch or list)", sub)}
	}
	o, err := c.commonFlags("datasets "+sub, rest)
	if err != nil {
		return err
	}
	cfg, err := c.config(o.EnvFile)
	if err != nil {
		return err
	}
	if sub == "fetch" {
		return c.fetchCmd(ctx, c, cfg, o)
	}
	return c.listCmd(ctx, c, cfg, o)
}

// config loads the settings and prints what is worth saying about them.
func (c *cli) config(envFile string) (config.Config, error) {
	cfg, err := c.loadConfig(envFile)
	if _, wrongCall := errors.AsType[usageError](err); wrongCall {
		return config.Config{}, err // there are no settings to complain about
	}
	if err != nil {
		return config.Config{}, fail(exitUsage, "configuration: %w", err)
	}
	c.warn(cfg.Warnings)
	return cfg, nil
}

func (c *cli) warn(warnings []string) {
	for _, w := range warnings {
		c.say(c.stderr, "warning: "+w+"\n")
	}
}
