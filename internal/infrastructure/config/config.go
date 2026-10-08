// Package config loads and validates the Web UI configuration from the
// process environment, optionally backed by a .env file.
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
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/v8tix/inside-agent-web-ui/internal/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/redact"
)

// Config is the complete, validated Web UI configuration.
type Config struct {
	App       App
	HTTP      HTTP
	DB        DB
	Agent     Agent
	Chat      Chat
	RateLimit RateLimit
	Session   Session
	Admin     Admin
}

// App holds process-level settings.
type App struct {
	Env           string
	BaseURL       string
	LogLevel      string
	LogFormat     string
	ShutdownGrace time.Duration
}

// HTTP holds server settings.
type HTTP struct {
	Addr         string
	MaxBodyBytes int64
	// BodyTimeout is how long a client has to send a request body, once the
	// headers are in. ResponseTimeout is how long it has to take a response that
	// is not an event stream. Load refuses zero for both: zero means no limit
	// only to code that builds an HTTP itself.
	BodyTimeout     time.Duration
	ResponseTimeout time.Duration
}

// DB holds database settings.
type DB struct {
	URL            envcfg.Secret
	MaxConns       int
	MigrateOnStart bool
}

// Agent holds the upstream agent settings.
type Agent struct {
	BaseURL        string
	AppName        string
	RequestTimeout time.Duration
}

// Chat holds chat behavior settings.
type Chat struct {
	MaxMessageChars     int
	StreamFlushInterval time.Duration
	ListLimit           int
}

// RateLimit holds the token bucket settings.
type RateLimit struct {
	RPS   float64
	Burst int
}

// Session holds login session settings.
type Session struct {
	TTL time.Duration
}

// Admin optionally bootstraps the first user.
type Admin struct {
	Email    string
	Password envcfg.Secret
}

// SecureCookies reports whether cookies must carry the Secure attribute.
func (c Config) SecureCookies() bool {
	u, err := url.Parse(c.App.BaseURL)
	return err == nil && u.Scheme == "https"
}

// LogValue implements slog.LogValuer: every setting, under the name of its
// field. The two secrets hide themselves, the address of the first user is
// hidden by the name of its field, and the two base URLs are shown without the
// credentials or the query they may carry. Two attributes follow the settings:
// db is where the database is, its host and its name, and admin_bootstrap is
// whether a first user is set.
func (c Config) LogValue() slog.Value {
	shown := c
	shown.App.BaseURL = redact.URL(c.App.BaseURL)
	shown.Agent.BaseURL = redact.URL(c.Agent.BaseURL)
	view, err := redact.Struct(shown, "Email")
	if err != nil {
		return view // the placeholder alone: nothing of the settings is shown
	}
	return slog.GroupValue(append(view.Group(),
		slog.String("db", c.database()),
		slog.Bool("admin_bootstrap", c.Admin.Email != ""),
	)...)
}

// database is where DB_URL points, the host and the name of the database: the
// address as redact.URL shows it, without its scheme.
func (c Config) database() string {
	shown := redact.URL(c.DB.URL.Reveal())
	if _, where, found := strings.Cut(shown, "://"); found {
		return where
	}
	return cmp.Or(shown, "unset")
}

// LoadEnv builds a Config from the process environment and the .env file at
// path. Process values win over file values. A missing file is not an error.
func LoadEnv(path string) (Config, error) {
	lookup, err := envcfg.EnvLookup(path)
	if err != nil {
		return Config{}, err
	}
	return Load(lookup)
}

// A browser sends a message form-encoded, which spells a character of up to four
// UTF-8 bytes in up to twelve (%XX per byte), and a request carries a little more
// besides the text. HTTP_MAX_BODY_BYTES has to hold the longest message
// CHAT_MAX_MESSAGE_CHARS allows, or the server refuses it with a bare 413 before
// it can say what the limit is.
const (
	defaultMaxBodyBytes = 128 << 10
	bytesPerFormChar    = 12
	bodyOverheadBytes   = 4096
)

// Load builds a Config from lookup. It reports every problem at once. An empty
// value counts as unset.
func Load(lookup envcfg.LookupFunc) (Config, error) {
	r := &reader{lookup: lookup}

	c := Config{
		App: App{
			Env:           r.oneOf("APP_ENV", "development", "development", "test", "production"),
			LogLevel:      r.oneOf("APP_LOG_LEVEL", "info", "debug", "info", "warn", "error"),
			LogFormat:     r.oneOf("APP_LOG_FORMAT", "json", "json", "text"),
			ShutdownGrace: r.positive("APP_SHUTDOWN_GRACE", 10*time.Second, time.ParseDuration),
		},
		HTTP: HTTP{
			Addr:            r.listenAddr("APP_HTTP_ADDR", ":8300"),
			MaxBodyBytes:    r.positive("HTTP_MAX_BODY_BYTES", defaultMaxBodyBytes, parseInt64),
			BodyTimeout:     r.positive("HTTP_BODY_TIMEOUT", 30*time.Second, time.ParseDuration),
			ResponseTimeout: r.positive("HTTP_RESPONSE_TIMEOUT", 2*time.Minute, time.ParseDuration),
		},
		DB: DB{
			MaxConns:       r.positive("DB_MAX_CONNS", 10, strconv.Atoi),
			MigrateOnStart: r.parse("DB_MIGRATE_ON_START", true, strconv.ParseBool),
		},
		Agent: Agent{
			AppName:        r.required("AGENT_APP_NAME"),
			RequestTimeout: r.positive("AGENT_REQUEST_TIMEOUT", 120*time.Second, time.ParseDuration),
		},
		Chat: Chat{
			MaxMessageChars:     r.positive("CHAT_MAX_MESSAGE_CHARS", 8000, strconv.Atoi),
			StreamFlushInterval: r.positive("CHAT_STREAM_FLUSH_INTERVAL", 50*time.Millisecond, time.ParseDuration),
			ListLimit:           r.positive("CHAT_LIST_LIMIT", 100, strconv.Atoi),
		},
		RateLimit: RateLimit{
			RPS:   r.positive("RATE_LIMIT_RPS", 2.0, parseFloat64),
			Burst: r.positive("RATE_LIMIT_BURST", 10, strconv.Atoi),
		},
		Session: Session{
			TTL: r.positive("SESSION_TTL", 24*time.Hour, time.ParseDuration),
		},
	}

	c.App.BaseURL = strings.TrimRight(r.absoluteURL("APP_BASE_URL", r.str("APP_BASE_URL", "http://localhost:8300"), "http", "https"), "/")
	c.DB.URL = envcfg.Secret(r.databaseURL("DB_URL", r.required("DB_URL")))
	c.Agent.BaseURL = strings.TrimRight(r.absoluteURL("AGENT_BASE_URL", r.required("AGENT_BASE_URL"), "http", "https"), "/")
	c.Admin = r.admin()

	// The checks that compare keys run only when each key is sound by itself, so
	// a typo is not reported twice.
	if len(r.missing) == 0 && len(r.errs) == 0 {
		if need := int64(c.Chat.MaxMessageChars)*bytesPerFormChar + bodyOverheadBytes; c.HTTP.MaxBodyBytes < need {
			r.fail("HTTP_MAX_BODY_BYTES is %d, too small for CHAT_MAX_MESSAGE_CHARS=%d: allow at least %d, or lower the message limit",
				c.HTTP.MaxBodyBytes, c.Chat.MaxMessageChars, need)
		}
		if c.App.Env == "production" && !c.SecureCookies() {
			r.fail("APP_ENV=production needs an https APP_BASE_URL, so the session cookie is only sent over https")
		}
	}

	return c, r.err()
}

// reader accumulates problems while reading keys.
type reader struct {
	lookup  envcfg.LookupFunc
	missing []string
	errs    []error
}

func (r *reader) raw(key string) (string, bool) {
	v, ok := r.lookup(key)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (r *reader) fail(format string, args ...any) {
	r.errs = append(r.errs, fmt.Errorf(format, args...))
}

func (r *reader) err() error {
	var errs []error
	if len(r.missing) > 0 {
		errs = append(errs, fmt.Errorf("missing required configuration: %s", strings.Join(r.missing, ", ")))
	}
	errs = append(errs, r.errs...)
	return errors.Join(errs...)
}

func (r *reader) str(key, def string) string {
	if v, ok := r.raw(key); ok {
		return v
	}
	return def
}

func (r *reader) required(key string) string {
	v, ok := r.raw(key)
	if !ok {
		r.missing = append(r.missing, key)
	}
	return v
}

// parse converts the raw value with conv; problems are recorded and def is used.
func (r *reader) parse[T any](key string, def T, conv func(string) (T, error)) T {
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

// positive is parse plus a "greater than zero" check.
func (r *reader) positive[T cmp.Ordered](key string, def T, conv func(string) (T, error)) T {
	v := r.parse(key, def, conv)
	var zero T
	if v <= zero {
		r.fail("%s must be greater than zero (got %v)", key, v)
		return def
	}
	return v
}

func (r *reader) oneOf(key, def string, allowed ...string) string {
	v := r.str(key, def)
	if !slices.Contains(allowed, v) {
		r.fail("%s must be one of %s (got %q)", key, strings.Join(allowed, ", "), v)
		return def
	}
	return v
}

// listenAddr is str for an address to listen on: a host, which may be left out,
// and a port number. The scripts and the health check take the port from it.
func (r *reader) listenAddr(key, def string) string {
	v := r.str(key, def)
	_, port, _ := net.SplitHostPort(v) // no port comes back for what is not host:port
	if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
		r.fail("%s must be a host and a port number, like 127.0.0.1:8300 or :8300 (got %q)", key, v)
		return def
	}
	return v
}

// absoluteURL validates raw without ever echoing it: URLs can carry credentials.
// An address that redact.URL cannot show is refused as well. It has an @ after
// its host: a slash, a question mark or a hash sign that stands in a user or a
// password as it is ends the host there, so a piece of the password is taken
// for the host, and an error about that host names it.
func (r *reader) absoluteURL(key, raw string, schemes ...string) string {
	if raw == "" {
		return "" // already reported as missing
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !slices.Contains(schemes, u.Scheme) || redact.URL(raw) == redact.Unreadable {
		r.refuseURL(key, schemes)
		return ""
	}
	return raw
}

// databaseURL is absoluteURL for the address of the database, which the driver
// reads too, by the rule of libpq: the user and the password end at the first
// @ before the first slash. net/url ends them at the last @ before the first
// slash, question mark or hash sign. With two @ in that part, the driver takes
// a piece of the password for the host and names it in its errors, so the part
// holds one at most. absoluteURL refuses an @ after it, and the driver and
// net/url then end the user and the password at the same @.
func (r *reader) databaseURL(key, raw string) string {
	schemes := []string{"postgres", "postgresql"}
	raw = r.absoluteURL(key, raw, schemes...)
	_, authority, _ := strings.Cut(raw, "://")
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	if strings.Count(authority, "@") > 1 {
		r.refuseURL(key, schemes)
		return ""
	}
	return raw
}

// refuseURL records that the value of key is no address that Load takes. The
// text names the setting and nothing of its value.
func (r *reader) refuseURL(key string, schemes []string) {
	r.fail("%s must be an absolute %s URL, with @, /, ? and # percent-encoded in its credentials (%%40, %%2F, %%3F, %%23)",
		key, strings.Join(schemes, " or "))
}

func (r *reader) admin() Admin {
	email, hasEmail := r.raw("ADMIN_EMAIL")
	password, hasPassword := r.raw("ADMIN_PASSWORD")
	switch {
	case hasEmail && !hasPassword:
		r.fail("ADMIN_PASSWORD is required when ADMIN_EMAIL is set")
	case hasPassword && !hasEmail:
		r.fail("ADMIN_EMAIL is required when ADMIN_PASSWORD is set")
	case hasEmail && hasPassword:
		if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
			r.fail("ADMIN_EMAIL must be a plain email address")
		}
		if utf8.RuneCountInString(password) < domain.MinPasswordChars {
			r.fail("ADMIN_PASSWORD must be at least %d characters", domain.MinPasswordChars)
		}
	}
	return Admin{Email: email, Password: envcfg.Secret(password)}
}

func parseInt64(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }

// parseFloat64 reads a real number. NaN and the infinities parse, and as a rate
// limit two of them pass for greater than zero and then refuse every request:
// a bucket that is filled at such a rate holds NaN tokens, and no comparison
// with NaN is true.
func parseFloat64(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err == nil && (math.IsNaN(f) || math.IsInf(f, 0)) {
		return 0, fmt.Errorf("%q is not a finite number", s)
	}
	return f, err
}
