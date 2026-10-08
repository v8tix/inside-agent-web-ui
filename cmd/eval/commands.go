package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/resource"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/port/service"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/probe"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/run"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/score"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/devtools"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/gaia"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/safety"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/store"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/synthetic"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/redact"
)

// exitInterrupted is the conventional status of a process ended with Ctrl-C.
const exitInterrupted = 130

// selftestWorkers is how many browsers the self-test uses: two, so that the
// runs of attempts side by side are exercised too.
const selftestWorkers = 2

// runnerSetup is what a command hands to the code that builds its runner.
type runnerSetup struct {
	cfg         config.Config
	source      resource.ProblemSource
	store       resource.ResultStore
	prompt      score.Prompt
	humanTyping bool
	log         *slog.Logger
}

// proberSetup is what a command hands to the code that builds the probes' suite.
type proberSetup struct {
	cfg     config.Config
	store   resource.ResultStore
	prompts devtools.ProbePrompts
	log     *slog.Logger
}

// realProber is the suite of probes of the evaluator: one browser, in which the
// probes are carried out one after another.
func realProber(s proberSetup) service.Prober {
	probes := &devtools.Probes{
		Factory: &devtools.Factory{
			Session: devtools.Options{Command: s.cfg.MCP.Command, Args: s.cfg.MCP.Args, Logger: s.log},
			Driver:  devtools.DriverConfig{BaseURL: s.cfg.Eval.BaseURL, Logger: s.log},
		},
		Email: s.cfg.Eval.UserEmail, Password: s.cfg.Eval.UserPassword.Reveal(),
		Prompts: s.prompts, Thresholds: s.cfg.Thresholds, KeepChats: s.cfg.Eval.KeepChats, Logger: s.log,
	}
	return &probe.Suite{
		Probes: probes, Store: s.store, Logger: s.log,
		Reporter: &run.Reporter{Store: s.store, Thresholds: s.cfg.Thresholds},
	}
}

// livePrompts are what the probes say to an agent that is not the scripted one.
func livePrompts(cfg config.Config) devtools.ProbePrompts {
	return devtools.ProbePrompts{Long: cfg.Eval.ProbePrompt, Short: synthetic.ShortProbePrompt}
}

// scriptedPrompts are what they say to the scripted agent.
func scriptedPrompts() devtools.ProbePrompts {
	return devtools.ProbePrompts{Long: synthetic.LongProbePrompt, Short: synthetic.ShortProbePrompt, Flaky: synthetic.FlakyProbePrompt}
}

// realRunner is the runner of the evaluator: one Chrome DevTools MCP server and
// browser for each worker, results on disk.
func realRunner(s runnerSetup) service.Runner {
	factory := &devtools.Factory{
		Session: devtools.Options{Command: s.cfg.MCP.Command, Args: s.cfg.MCP.Args, Logger: s.log},
		Driver: devtools.DriverConfig{
			BaseURL: s.cfg.Eval.BaseURL, HumanTyping: s.humanTyping, TypingDelay: s.cfg.Eval.TypingDelay, Logger: s.log,
		},
	}
	return &run.Runner{
		Source: s.source, Drivers: factory, Store: s.store, Prompt: s.prompt, Thresholds: s.cfg.Thresholds,
		Info:     run.RunInfo{GitSHA: gitSHA(), UIContract: devtools.SupportedContract, Versions: toolVersions(s.cfg)},
		Versions: factory.Versions,
		Logger:   s.log,
	}
}

// gitSHA is the commit this binary was built from, if the build recorded it.
func gitSHA() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var sha, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			sha = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if len(sha) > 12 {
		sha = sha[:12]
	}
	if sha != "" && modified == "true" {
		sha += "+dirty"
	}
	return sha
}

var pinnedVersion = regexp.MustCompile(`^chrome-devtools-mcp@(.+)$`)

// toolVersions are the versions known before anything runs: Go, Node, and the
// version of Chrome DevTools MCP that the settings pin. Chrome's own is added
// when the first browser has started.
func toolVersions(cfg config.Config) map[string]string {
	v := map[string]string{"go": strings.TrimPrefix(runtime.Version(), "go")}
	for _, word := range cfg.MCP.Command {
		if m := pinnedVersion.FindStringSubmatch(word); m != nil {
			v["chrome-devtools-mcp"] = m[1]
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "node", "--version").Output(); err == nil {
		v["node"] = strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	}
	return v
}

// newRunID makes an id that names the day and the second, with a few random
// characters so two runs started together differ.
func newRunID(prefix string, now time.Time) string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		b = [3]byte{byte(now.Nanosecond()), byte(now.Nanosecond() >> 8), byte(now.Nanosecond() >> 16)}
	}
	return prefix + now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

func (c *cli) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(c.stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// loadPrompt reads the template that EVAL_PROMPT_FILE names, or uses the
// built-in prompt. The file is read here: the core is given the text.
func loadPrompt(cfg config.Config) (score.Prompt, error) {
	if cfg.Eval.PromptFile == "" {
		return score.DefaultPrompt(), nil
	}
	text, err := os.ReadFile(cfg.Eval.PromptFile)
	if err != nil {
		return score.Prompt{}, fmt.Errorf("reading the prompt template: %w", err)
	}
	return score.ParsePrompt(string(text))
}

// checkWebUI asks the Web UI whether it is ready, before a browser is started
// for it: a stack that is not up is the commonest mistake, and the browser's
// own complaint about it is not a helpful one. EVAL_BASE_URL can carry
// credentials, so a message names the address as redact.URL shows it.
func (c *cli) checkWebUI(ctx context.Context, baseURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	shown := redact.URL(baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/readyz", nil)
	if err != nil {
		return fail(exitUsage, "EVAL_BASE_URL %q is not a usable address: %w", shown, reason(err))
	}
	client := cmp.Or(c.httpClient, http.DefaultClient)
	resp, err := client.Do(req)
	if err != nil {
		return fail(exitInfra, "the Web UI at %s is not answering (%v): start it, for example with `docker compose --profile full up -d --wait`", shown, reason(err))
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fail(exitInfra, "the Web UI at %s is not ready (/readyz answered HTTP %d): its database or its agent is down", shown, resp.StatusCode)
	}
	return nil
}

// reason is why a request could not be made or was not answered, without the
// address it was for. net/url and net/http quote the address in their errors:
// the first as it is, the second with the password masked and the user and
// the query as they are.
func reason(err error) error {
	if quoting, ok := errors.AsType[*url.Error](err); ok {
		return quoting.Err
	}
	return err
}

// --- run -------------------------------------------------------------------------------

func runCommand(ctx context.Context, c *cli, cfg config.Config, o runOptions) error {
	if err := cfg.RequireUser(); err != nil {
		return usageError{msg: err.Error()}
	}
	if err := (safety.Guard{}).Check(ctx, cfg.GAIA.DatasetsDir, cfg.ResultsDir); err != nil {
		return fail(exitUsage, "%w", err)
	}
	prompt, err := loadPrompt(cfg)
	if err != nil {
		return fail(exitUsage, "%w", err)
	}
	if o.Resume != "" {
		if err := c.checkResumable(ctx, cfg, o.Resume); err != nil {
			return err
		}
	}
	req := service.RunRequest{
		RunID:   cmp.Or(o.Resume, newRunID("", c.clock())),
		Command: "run",
		Filter: service.Filter{
			Level: o.Level, Limit: o.Limit, TaskIDs: o.TaskIDs, Attachments: cfg.GAIA.Attachments,
		},
		Repeats:     cmp.Or(o.Repeats, cfg.Eval.Repeats),
		Workers:     cmp.Or(o.Workers, cfg.Eval.Workers),
		Timeout:     cmp.Or(o.Timeout, cfg.Eval.TaskTimeout),
		HumanTyping: o.HumanTyping,
		IncludeText: o.IncludeText,
		// A picture of the page shows the question and the reply, so it is kept
		// only when the text is.
		Screenshots: o.IncludeText,
		KeepChats:   cfg.Eval.KeepChats,
		Email:       cfg.Eval.UserEmail,
		Password:    cfg.Eval.UserPassword.Reveal(),
	}
	source := gaia.NewParquet(cfg.GAIA.DatasetsDir, cfg.GAIA.Levels, cfg.GAIA.Split)
	rules := verdictRules{minAccuracy: o.MinAccuracy}
	if !o.NoProbes {
		prompts := livePrompts(cfg)
		rules.probes = &prompts
	}
	return c.execute(ctx, cfg, req, source, prompt, rules)
}

// checkResumable refuses to resume a run that is not there. Beginning a run that
// does not exist creates it, so a mistyped id would start everything again under
// the mistyped name, after hours of work or a paid model, and exit 0.
func (c *cli) checkResumable(ctx context.Context, cfg config.Config, runID string) error {
	_, err := store.New(cfg.ResultsDir).LoadMeta(ctx, runID)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, resource.ErrRunNotFound), errors.Is(err, store.ErrBadRunID):
		return fail(exitUsage, "there is no run %q to resume in %s: check the id (it is printed when a run starts, and when one is interrupted)", runID, cfg.ResultsDir)
	}
	return fail(exitInfra, "reading the run to resume: %w", err)
}

// verdictRules say what makes a finished run a failure beyond a Must check.
type verdictRules struct {
	minAccuracy float64
	// expected, when set, is the self-test's golden: any difference fails.
	expected *run.Expected
	// probes, when set, says what the probes say to the agent; they are carried
	// out after the problems.
	probes *devtools.ProbePrompts
}

// execute runs a request and turns the outcome into output and an exit status.
func (c *cli) execute(ctx context.Context, cfg config.Config, req service.RunRequest, source resource.ProblemSource, prompt score.Prompt, rules verdictRules) error {
	if err := c.checkWebUI(ctx, cfg.Eval.BaseURL); err != nil {
		return err
	}
	results := store.New(cfg.ResultsDir)
	dir, err := results.RunDir(req.RunID)
	if err != nil {
		return fail(exitUsage, "%w", err)
	}
	log := c.logger()
	runner := c.buildRunner(runnerSetup{cfg: cfg, source: source, store: results, prompt: prompt, humanTyping: req.HumanTyping, log: log})

	c.say(c.stderr, fmt.Sprintf("agent-eval %s %s: results go to %s\n", req.Command, req.RunID, dir))
	out, err := runner.Run(ctx, req)
	if err != nil {
		return runFailure(err, req.RunID, dir)
	}
	if rules.probes != nil && !out.Interrupted {
		out, err = c.probe(ctx, cfg, results, out, *rules.probes, log)
		if err != nil {
			return runFailure(err, req.RunID, dir)
		}
	}
	c.printOutcome(out, dir)
	return judge(out, rules, req.RunID)
}

// probe carries out the UI probes for a run that has finished its problems, and
// returns the outcome with the summary they complete.
func (c *cli) probe(ctx context.Context, cfg config.Config, results resource.ResultStore, out service.Outcome, prompts devtools.ProbePrompts, log *slog.Logger) (service.Outcome, error) {
	prober := c.buildProber(proberSetup{cfg: cfg, store: results, prompts: prompts, log: log})
	summary, err := prober.Probe(ctx, out.Meta.RunID)
	switch {
	case ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		out.Interrupted = true // cut short: what finished is in the summary
	case err != nil:
		return out, err
	}
	if summary.RunID != "" {
		out.Summary = summary
	}
	return out, nil
}

// runFailure explains a run that could not go on, and what to do about it.
func runFailure(err error, runID, dir string) error {
	var selection *run.SelectionError
	switch {
	case errors.As(err, &selection):
		return fail(exitUsage, "%w", err)
	case errors.Is(err, domain.ErrLoginFailed):
		return fail(exitUsage, "%w (check EVAL_USER_EMAIL and EVAL_USER_PASSWORD, or ADMIN_EMAIL and ADMIN_PASSWORD)", err)
	case errors.Is(err, domain.ErrContractVersion):
		return fail(exitUsage, "%w", err)
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		return fail(exitInfra, "%w\nwhat was recorded stays in %s; continue with: agent-eval run --resume %s", err, dir, runID)
	}
	return fail(exitInfra, "%w", err)
}

// judge decides the exit status of a finished run. A run that was cut short is
// not judged: it has not been completed.
func judge(out service.Outcome, rules verdictRules, runID string) error {
	a, ui := out.Summary.Agent, out.Summary.UI
	switch {
	case out.Interrupted:
		return fail(exitInterrupted, "interrupted: what finished is saved; continue with: agent-eval run --resume %s", runID)
	case len(ui.MustFailed) > 0:
		return fail(exitUIFailed, "required UI checks failed: %s", domain.JoinChecks(ui.MustFailed))
	case a.Attempted == 0 && out.Meta.Command != "probe":
		// A run that asked nothing has not exercised the Web UI at all.
		return fail(exitUsage, "nothing was asked: %s", whyNothingWasAsked(a))
	case cannotDrive(ui) > 0:
		return fail(exitInfra, "the page could not be driven in %s (%s): the browser died, or the session ended and signing in again did not help; "+
			"continue with: agent-eval run --resume %s", domain.Attempts(cannotDrive(ui)), ui.UIErrorList(), runID)
	case ui.UIErrorTotal() > 0:
		return fail(exitUIFailed, "the page broke %s (%s); continue with: agent-eval run --resume %s asks them again",
			domain.Attempts(ui.UIErrorTotal()), ui.UIErrorList(), runID)
	case rules.expected != nil:
		if diffs := rules.expected.Compare(out.Summary); len(diffs) > 0 {
			return fail(exitAccuracy, "the self-test found different results from the expected ones:\n  %s%s", strings.Join(diffs, "\n  "), selftestHint(a))
		}
	case rules.minAccuracy > 0 && a.AccuracyAttempted.Rate < rules.minAccuracy:
		return fail(exitAccuracy, "accuracy %.1f%% of the %d problems asked is below the minimum of %.1f%%", a.AccuracyAttempted.Rate*100, a.Attempted, rules.minAccuracy*100)
	}
	return nil
}

// cannotDrive counts the attempts in which the evaluator could not work the page
// at all: the browser died, or the page would not even start a chat.
func cannotDrive(ui domain.UIScorecard) int {
	return ui.UIErrors[run.ClassDriverDied] + ui.UIErrors[run.ClassStartChat]
}

// whyNothingWasAsked names the likeliest reason a run asked no problem.
func whyNothingWasAsked(a domain.AgentScorecard) string {
	if a.Eligible == 0 {
		return "no problem matched (check --level and --task, and HF_GAIA_LEVELS)"
	}
	return fmt.Sprintf("all %d problems were skipped (an attachment the agent cannot be given, or no published answer; see GAIA_ATTACHMENTS)", a.Eligible)
}

// selftestHint points at the likeliest cause when none of the scripted outcomes
// showed up: the agent that answered was not running the script. A scripted
// agent gives some right answers, one error and one timeout; an agent that just
// echoes the question gives none of those.
func selftestHint(a domain.AgentScorecard) string {
	if a.Attempted > 0 && a.Correct == 0 && a.Statuses[domain.StatusAgentError] == 0 && a.Statuses[domain.StatusTimeout] == 0 {
		return "\nnone of the scripted outcomes (right answers, an agent error, a timeout) turned up: the agent is probably\n" +
			"not running the self-test script. Start the stack with the `selftest` profile\n" +
			"(docker compose --profile selftest up -d --wait), and check that nothing else is listening on the Web UI's port"
	}
	return ""
}

// --- selftest --------------------------------------------------------------------------

func selftestCommand(ctx context.Context, c *cli, cfg config.Config, _ commonOptions) error {
	if err := cfg.RequireUser(); err != nil {
		return usageError{msg: err.Error()}
	}
	if err := (safety.Guard{}).Check(ctx, cfg.ResultsDir); err != nil {
		return fail(exitUsage, "%w", err)
	}
	expected, err := synthetic.Expected()
	if err != nil {
		return fail(exitInfra, "%w", err)
	}
	req := service.RunRequest{
		RunID:   newRunID("selftest-", c.clock()),
		Command: "selftest",
		Filter:  service.Filter{Attachments: domain.AttachmentsSkip},
		Repeats: 1,
		Workers: selftestWorkers,
		Timeout: run.SelftestTimeout,
		// The problems are invented, so there is nothing in a picture to protect.
		Screenshots: true,
		Email:       cfg.Eval.UserEmail,
		Password:    cfg.Eval.UserPassword.Reveal(),
	}
	prompts := scriptedPrompts()
	return c.execute(ctx, cfg, req, synthetic.Problems(), score.DefaultPrompt(), verdictRules{expected: &expected, probes: &prompts})
}

// --- probe -----------------------------------------------------------------------------

func probeCommand(ctx context.Context, c *cli, cfg config.Config, o probeOptions) error {
	if err := cfg.RequireUser(); err != nil {
		return usageError{msg: err.Error()}
	}
	if err := (safety.Guard{}).Check(ctx, cfg.ResultsDir); err != nil {
		return fail(exitUsage, "%w", err)
	}
	if err := c.checkWebUI(ctx, cfg.Eval.BaseURL); err != nil {
		return err
	}
	results := store.New(cfg.ResultsDir)
	runID := newRunID("probe-", c.clock())
	dir, err := results.RunDir(runID)
	if err != nil {
		return fail(exitUsage, "%w", err)
	}
	meta := domain.RunMeta{
		RunID: runID, Command: "probe", StartedAt: c.clock(), UIContract: devtools.SupportedContract,
		Versions: toolVersions(cfg), Thresholds: cfg.Thresholds,
	}
	if err := results.Begin(ctx, meta); err != nil {
		return fail(exitInfra, "%w", err)
	}

	prompts := livePrompts(cfg)
	if o.Scripted {
		prompts = scriptedPrompts()
	}
	c.say(c.stderr, fmt.Sprintf("agent-eval probe %s: results go to %s\n", runID, dir))
	out := service.Outcome{Meta: meta}
	out, err = c.probe(ctx, cfg, results, out, prompts, c.logger())
	if err != nil {
		return runFailure(err, runID, dir)
	}
	if !out.Interrupted {
		out.Meta.FinishedAt = c.clock()
		if err := results.Finish(ctx, out.Meta); err != nil {
			return fail(exitInfra, "%w", err)
		}
	}
	c.printOutcome(out, dir)
	return judge(out, verdictRules{}, runID)
}

// --- datasets --------------------------------------------------------------------------

func fetchCommand(ctx context.Context, c *cli, cfg config.Config, _ commonOptions) error {
	if err := cfg.RequireToken(); err != nil {
		return usageError{msg: err.Error()}
	}
	if err := (safety.Guard{}).Check(ctx, cfg.GAIA.DatasetsDir); err != nil {
		return fail(exitUsage, "%w", err)
	}
	fetcher := &gaia.Fetcher{
		ServerURL: cfg.HF.DatasetsServerURL, HubURL: cfg.HF.HubURL, Token: cfg.HF.Token, Logger: c.logger(),
	}
	files, err := fetcher.Fetch(ctx, cfg.GAIA.Dataset, cfg.GAIA.Levels, cfg.GAIA.Split, cfg.GAIA.DatasetsDir)
	switch {
	case errors.Is(err, gaia.ErrNoToken):
		return usageError{msg: err.Error()}
	case err != nil:
		return fail(exitInfra, "%w", err)
	}
	c.printFetched(files, cfg.GAIA.DatasetsDir)
	return nil
}

func listCommand(ctx context.Context, c *cli, cfg config.Config, _ commonOptions) error {
	inventory, err := gaia.Inventory(ctx, cfg.GAIA.DatasetsDir)
	if err != nil {
		return fail(exitInfra, "reading %s: %w", cfg.GAIA.DatasetsDir, err)
	}
	c.printInventory(inventory, cfg)
	return nil
}

// --- report ----------------------------------------------------------------------------

func reportCommand(ctx context.Context, c *cli, cfg config.Config, o reportOptions) error {
	results := store.New(cfg.ResultsDir)
	reporter := &run.Reporter{Store: results, Thresholds: cfg.Thresholds}
	summary, err := reporter.Report(ctx, o.RunID)
	if err != nil {
		if errors.Is(err, resource.ErrRunNotFound) || errors.Is(err, store.ErrBadRunID) {
			return usageError{msg: fmt.Sprintf("%v (runs are kept under %s)", err, cfg.ResultsDir)}
		}
		return fail(exitInfra, "%w", err)
	}
	meta, err := results.LoadMeta(ctx, o.RunID)
	if err != nil {
		return fail(exitInfra, "%w", err)
	}
	return c.printReport(o.Format, meta, summary)
}
