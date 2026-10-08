// Package config loads and validates the evaluator's settings. They come from
// the same .env as the Web UI's, and process environment variables win.
package config

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/mail"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/redact"
)

// Secret and LookupFunc are shared with the Web UI's configuration.
type (
	Secret     = envcfg.Secret
	LookupFunc = envcfg.LookupFunc
)

// Config is the evaluator's complete, validated configuration.
type Config struct {
	HF   HuggingFace
	GAIA GAIA
	Eval Eval
	MCP  MCP
	// ResultsDir is where runs are written. It must not be tracked by git.
	ResultsDir string
	Thresholds domain.Thresholds
	// Warnings are things worth saying that do not stop the run.
	Warnings []string
	// RunWarnings are the ones that only matter to `run`: the self-test and the
	// probes bring their own timeouts.
	RunWarnings []string
}

// HuggingFace holds the settings for downloading GAIA.
type HuggingFace struct {
	// Token is needed by `datasets fetch` only: see RequireToken.
	Token             Secret
	DatasetsServerURL string
	HubURL            string
}

// GAIA says which dataset to use and where it lives.
type GAIA struct {
	Dataset string
	// Levels are the configs to download, such as 2023_level1.
	Levels []string
	// DatasetsDir holds the downloaded parquet files. It must not be tracked by
	// git, and may point at a directory that already exists.
	DatasetsDir string
	Split       string
	Attachments domain.AttachmentsMode
}

// Eval holds the settings of a run.
type Eval struct {
	BaseURL      string
	UserEmail    string
	UserPassword Secret
	// UserFromAdmin is true when the user is the Web UI's first user
	// (ADMIN_EMAIL), because no EVAL_USER_EMAIL was set.
	UserFromAdmin bool
	TaskTimeout   time.Duration
	Workers       int
	Repeats       int
	TypingDelay   time.Duration
	KeepChats     bool
	// PromptFile replaces the built-in prompt template when set.
	PromptFile string
	// ProbePrompt is what the probes that need a long reply ask.
	ProbePrompt string
}

// MCP says how to start Chrome DevTools MCP.
type MCP struct {
	// Command and Args are split on spaces; there is no shell quoting.
	Command []string
	Args    []string
}

// RequireToken fails when there is no Hugging Face token, for the commands
// that download data.
func (c Config) RequireToken() error {
	if c.HF.Token.Reveal() == "" {
		return errors.New("HF_TOKEN is not set: downloading GAIA needs a Hugging Face token with access to the gated dataset (put it in .env)")
	}
	return nil
}

// RequireUser fails when there is no user to sign in as, for the commands that
// use the Web UI.
func (c Config) RequireUser() error {
	if c.Eval.UserEmail == "" || c.Eval.UserPassword.Reveal() == "" {
		return errors.New("no user to sign in as: set EVAL_USER_EMAIL and EVAL_USER_PASSWORD (or ADMIN_EMAIL and ADMIN_PASSWORD) in .env")
	}
	return nil
}

// LogValue implements slog.LogValuer: every setting, under the name of its
// field. The token and the password hide themselves, the address of the
// account to sign in with is hidden by the name of its field, the three URLs
// are shown without the credentials or the query they may carry, and
// hf_token_set says whether there is a token.
func (c Config) LogValue() slog.Value {
	shown := c
	shown.HF.DatasetsServerURL = redact.URL(c.HF.DatasetsServerURL)
	shown.HF.HubURL = redact.URL(c.HF.HubURL)
	shown.Eval.BaseURL = redact.URL(c.Eval.BaseURL)
	view, err := redact.Struct(shown, "UserEmail")
	if err != nil {
		return view // the placeholder alone: nothing of the settings is shown
	}
	return slog.GroupValue(append(view.Group(), slog.Bool("hf_token_set", c.HF.Token.Reveal() != ""))...)
}

// String implements fmt.Stringer; it shows the same as LogValue.
func (c Config) String() string { return c.LogValue().String() }

// Load builds a Config from lookup. It reports every problem at once, and an
// empty value counts as unset.
func Load(lookup LookupFunc) (Config, error) {
	r := &reader{lookup: lookup}
	d := domain.DefaultThresholds()

	var c Config
	c.HF = HuggingFace{
		Token:             Secret(r.str("HF_TOKEN", "")),
		DatasetsServerURL: r.secureURL("HF_DATASETS_SERVER_URL", "https://datasets-server.huggingface.co"),
		HubURL:            r.secureURL("HF_HUB_URL", "https://huggingface.co"),
	}
	c.GAIA = GAIA{
		Dataset:     r.dataset("HF_GAIA_DATASET", "gaia-benchmark/GAIA"),
		Levels:      r.levels("HF_GAIA_LEVELS", "2023_level1"),
		DatasetsDir: r.str("HF_DATASETS_DIR", "./datasets"),
		Split:       r.oneOf("GAIA_SPLIT", "validation", "validation", "test"),
		Attachments: r.attachments("GAIA_ATTACHMENTS", domain.AttachmentsSkip),
	}
	c.ResultsDir = r.str("RESULTS_DIR", "./results")

	c.Eval = Eval{
		BaseURL:     strings.TrimRight(r.url("EVAL_BASE_URL", "http://localhost:8300"), "/"),
		TaskTimeout: positive(r, "EVAL_TASK_TIMEOUT", 10*time.Minute, time.ParseDuration),
		Workers:     positive(r, "EVAL_WORKERS", 1, strconv.Atoi),
		Repeats:     positive(r, "EVAL_REPEATS", 1, strconv.Atoi),
		TypingDelay: nonNegative(r, "EVAL_TYPING_DELAY", 25*time.Millisecond, time.ParseDuration),
		KeepChats:   parse(r, "EVAL_KEEP_CHATS", false, strconv.ParseBool),
		PromptFile:  r.str("EVAL_PROMPT_FILE", ""),
		ProbePrompt: r.oneLine("EVAL_PROBE_PROMPT", defaultProbePrompt),
	}
	c.Eval.UserEmail, c.Eval.UserPassword, c.Eval.UserFromAdmin = r.user()

	c.MCP = MCP{
		Command: strings.Fields(r.str("EVAL_MCP_COMMAND", "npx -y chrome-devtools-mcp@1.10.1")),
		Args:    strings.Fields(r.str("EVAL_MCP_ARGS", "--headless --isolated --no-usage-statistics --no-performance-crux")),
	}
	if err := CheckMCP(c.MCP.Command, c.MCP.Args); err != nil {
		r.fail("%w", err)
	}

	c.Thresholds = domain.Thresholds{
		OverheadP95Ms:    positive(r, "EVAL_UI_OVERHEAD_P95_MS", d.OverheadP95Ms, parseFloat),
		StreamMinChars:   positive(r, "EVAL_STREAM_CHECK_MIN_CHARS", d.StreamMinChars, strconv.Atoi),
		StreamMinUpdates: positive(r, "EVAL_MIN_STREAM_UPDATES", d.StreamMinUpdates, strconv.Atoi),
		ScrollFollowPx:   positive(r, "EVAL_SCROLL_FOLLOW_PX", d.ScrollFollowPx, parseFloat),
		StopWithinMs:     positive(r, "EVAL_STOP_WITHIN_MS", d.StopWithinMs, parseFloat),
		LighthouseMin:    inRange(r, "EVAL_LIGHTHOUSE_MIN_SCORE", d.LighthouseMin, 1, 100),
		CLSMax:           positive(r, "EVAL_CLS_MAX", d.CLSMax, parseFloat),
	}

	c.Warnings, c.RunWarnings = warnings(c, r)
	return c, r.err()
}

// defaultProbePrompt asks for something an agent answers at length, so a probe
// has time to act while the reply is still streaming.
const defaultProbePrompt = "Please write a detailed, well structured essay of about 600 words on the history of the printing press."

var (
	pinned          = regexp.MustCompile(`^chrome-devtools-mcp@\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$`)
	levelName       = regexp.MustCompile(`^\d{4}_(?:level[123]|all)$`)
	datasetName     = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	privacyRequired = []string{"--headless", "--isolated", "--no-usage-statistics", "--no-performance-crux"}
)

// npxFlags are the flags npx may be given before the package, none of which
// changes which package it runs.
var npxFlags = []string{"-y", "--yes", "--no-install", "--prefer-offline", "-q", "--quiet"}

// pinnedPackage reports whether the command is npx, run on the pinned package:
// the package is the first thing after npx and its harmless flags. A check that
// found the pinned name anywhere in the command would pass "npx -y other-package
// chrome-devtools-mcp@1.10.1", or a shell script that mentions it.
func pinnedPackage(command []string) bool {
	if len(command) < 2 {
		return false
	}
	launcher := strings.TrimSuffix(filepath.Base(command[0]), ".cmd")
	if launcher != "npx" {
		return false
	}
	rest := command[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		if !slices.Contains(npxFlags, rest[0]) {
			return false
		}
		rest = rest[1:]
	}
	return len(rest) > 0 && pinned.MatchString(rest[0])
}

// CheckMCP verifies how Chrome DevTools MCP will be started: npx on a pinned
// version, so a new release cannot change the evaluator's behavior, and the four
// flags that keep it from sending usage statistics or touching a real browser
// profile.
func CheckMCP(command, args []string) error {
	var problems []error
	if !pinnedPackage(command) {
		problems = append(problems, fmt.Errorf(
			"EVAL_MCP_COMMAND must pin a version of chrome-devtools-mcp, like %q: npx, then no flag but %s, then the pinned package (got %q)",
			"npx -y chrome-devtools-mcp@1.10.1", strings.Join(npxFlags, " "), strings.Join(command, " ")))
	}
	var missing []string
	for _, flag := range privacyRequired {
		if !slices.Contains(args, flag) {
			missing = append(missing, flag)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, fmt.Errorf("EVAL_MCP_ARGS must keep %s (missing: %s)",
			strings.Join(privacyRequired, " "), strings.Join(missing, " ")))
	}
	return errors.Join(problems...)
}

func warnings(c Config, r *reader) (general, run []string) {
	if raw, ok := r.raw("AGENT_REQUEST_TIMEOUT"); ok {
		if agent, err := time.ParseDuration(raw); err == nil && c.Eval.TaskTimeout > agent {
			run = append(run, fmt.Sprintf(
				"EVAL_TASK_TIMEOUT (%s) is longer than AGENT_REQUEST_TIMEOUT (%s): the Web UI gives up on a reply first, so those attempts will show as agent errors, not timeouts",
				c.Eval.TaskTimeout, agent))
		}
	}
	if c.GAIA.Split == "test" {
		general = append(general, "GAIA_SPLIT=test has no published answers: its problems can be asked but not graded")
	}
	return general, run
}

// --- reading -------------------------------------------------------------------

type reader struct {
	lookup LookupFunc
	errs   []error
}

func (r *reader) raw(key string) (string, bool) {
	v, ok := r.lookup(key)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (r *reader) fail(format string, args ...any) {
	r.errs = append(r.errs, fmt.Errorf(format, args...))
}

func (r *reader) err() error { return errors.Join(r.errs...) }

func (r *reader) str(key, def string) string {
	if v, ok := r.raw(key); ok {
		return v
	}
	return def
}

func (r *reader) oneOf(key, def string, allowed ...string) string {
	v := r.str(key, def)
	if !slices.Contains(allowed, v) {
		r.fail("%s must be one of %s (got %q)", key, strings.Join(allowed, ", "), v)
		return def
	}
	return v
}

// url validates an absolute http(s) URL without ever echoing it back, since a
// URL can carry credentials. An address that redact.URL cannot show is refused
// as well. It has an @ after its host: a slash, a question mark or a hash sign
// that stands in a user or a password as it is ends the host there, so a piece
// of the password is taken for the host, and an error about that host names it.
func (r *reader) url(key, def string) string {
	v := r.str(key, def)
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || redact.URL(v) == redact.Unreadable {
		r.fail("%s must be an absolute http or https URL, with @, /, ? and # percent-encoded in its credentials (%%40, %%2F, %%3F, %%23)", key)
		return def
	}
	return v
}

// secureURL is url for a host that is sent the Hugging Face token as a bearer
// token: https, or http only to this computer (a local mirror, a test server).
// Over http to anywhere else the token would cross the network in the clear.
func (r *reader) secureURL(key, def string) string {
	v := r.url(key, def)
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "https" {
		return v
	}
	if ip := net.ParseIP(u.Hostname()); u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()) {
		return v
	}
	r.fail("%s must be an https URL (http is only for this computer), because the Hugging Face token is sent to it", key)
	return def
}

// oneLine is str for text that is typed into the composer by keystrokes, where a
// line break sends the message and a tab leaves the field.
func (r *reader) oneLine(key, def string) string {
	v := r.str(key, def)
	if strings.ContainsAny(v, "\n\r\t") {
		r.fail("%s must be one line, with no line break or tab", key)
		return def
	}
	return v
}

func (r *reader) dataset(key, def string) string {
	v := r.str(key, def)
	if !datasetName.MatchString(v) {
		r.fail("%s must look like owner/name (got %q)", key, v)
		return def
	}
	return v
}

func (r *reader) levels(key, def string) []string {
	var out []string
	for part := range strings.SplitSeq(r.str(key, def), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !levelName.MatchString(part) {
			r.fail("%s: %q is not a GAIA config such as 2023_level1, 2023_level2, 2023_level3 or 2023_all", key, part)
			continue
		}
		if !slices.Contains(out, part) {
			out = append(out, part)
		}
	}
	if len(out) == 0 && len(r.errs) == 0 {
		r.fail("%s lists no GAIA config", key)
	}
	return out
}

func (r *reader) attachments(key string, def domain.AttachmentsMode) domain.AttachmentsMode {
	v, ok := r.raw(key)
	if !ok {
		return def
	}
	mode, err := domain.ParseAttachmentsMode(v)
	if err != nil {
		r.fail("%s: %w", key, err)
		return def
	}
	return mode
}

// user picks the account to sign in as: EVAL_USER_*, or the Web UI's first
// user, which is the usual account in development.
func (r *reader) user() (email string, password Secret, fromAdmin bool) {
	email, hasEmail := r.raw("EVAL_USER_EMAIL")
	pass, hasPass := r.raw("EVAL_USER_PASSWORD")
	if !hasEmail && !hasPass {
		email, hasEmail = r.raw("ADMIN_EMAIL")
		pass, hasPass = r.raw("ADMIN_PASSWORD")
		fromAdmin = hasEmail && hasPass
	}
	switch {
	case hasEmail && !hasPass:
		r.fail("EVAL_USER_PASSWORD is required when EVAL_USER_EMAIL is set")
	case hasPass && !hasEmail:
		r.fail("EVAL_USER_EMAIL is required when EVAL_USER_PASSWORD is set")
	case hasEmail:
		if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
			r.fail("EVAL_USER_EMAIL must be a plain email address")
		}
	}
	return email, Secret(pass), fromAdmin
}

func parse[T any](r *reader, key string, def T, conv func(string) (T, error)) T {
	raw, ok := r.raw(key)
	if !ok {
		return def
	}
	v, err := conv(raw)
	if err != nil {
		r.fail("%s: %w", key, err)
		return def
	}
	return v
}

func positive[T cmp.Ordered](r *reader, key string, def T, conv func(string) (T, error)) T {
	v := parse(r, key, def, conv)
	var zero T
	if v <= zero {
		r.fail("%s must be greater than zero (got %v)", key, v)
		return def
	}
	return v
}

func nonNegative[T cmp.Ordered](r *reader, key string, def T, conv func(string) (T, error)) T {
	v := parse(r, key, def, conv)
	var zero T
	if v < zero {
		r.fail("%s must not be negative (got %v)", key, v)
		return def
	}
	return v
}

func inRange(r *reader, key string, def, lo, hi int) int {
	v := parse(r, key, def, strconv.Atoi)
	if v < lo || v > hi {
		r.fail("%s must be between %d and %d (got %d)", key, lo, hi, v)
		return def
	}
	return v
}

// parseFloat reads a real number. NaN and the infinities parse, and would turn a
// check off without a word, since every comparison with them is false.
func parseFloat(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err == nil && (math.IsNaN(f) || math.IsInf(f, 0)) {
		return 0, errors.New("not a finite number")
	}
	return f, err
}
