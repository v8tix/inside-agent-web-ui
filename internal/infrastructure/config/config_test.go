package config_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
)

const (
	secretDBPassword    = "s3cretDBpw"
	secretAdminPassword = "s3cretAdminPw-123"
)

func requiredEnv() map[string]string {
	return map[string]string{
		"DB_URL":         "postgres://app:" + secretDBPassword + "@localhost:5440/agentwebui",
		"AGENT_BASE_URL": "http://localhost:9093/api/",
		"AGENT_APP_NAME": "ui_agent",
	}
}

func with(base map[string]string, extra map[string]string) map[string]string {
	out := maps.Clone(base)
	maps.Copy(out, extra)
	return out
}

func lookupFrom(env map[string]string) envcfg.LookupFunc {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestLoad_Valid(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		check func(t *testing.T, c config.Config)
	}{
		{
			name: "defaults with only the required keys",
			env:  requiredEnv(),
			check: func(t *testing.T, c config.Config) {
				assert.Equal(t, "development", c.App.Env)
				assert.Equal(t, "http://localhost:8300", c.App.BaseURL)
				assert.Equal(t, "info", c.App.LogLevel)
				assert.Equal(t, "json", c.App.LogFormat)
				assert.Equal(t, 10*time.Second, c.App.ShutdownGrace)
				assert.Equal(t, ":8300", c.HTTP.Addr)
				assert.Equal(t, int64(131072), c.HTTP.MaxBodyBytes)
				assert.Equal(t, 30*time.Second, c.HTTP.BodyTimeout)
				assert.Equal(t, 2*time.Minute, c.HTTP.ResponseTimeout)
				assert.Equal(t, 10, c.DB.MaxConns)
				assert.True(t, c.DB.MigrateOnStart)
				assert.Equal(t, "ui_agent", c.Agent.AppName)
				assert.Equal(t, 120*time.Second, c.Agent.RequestTimeout)
				assert.Equal(t, 8000, c.Chat.MaxMessageChars)
				assert.Equal(t, 50*time.Millisecond, c.Chat.StreamFlushInterval)
				assert.Equal(t, 100, c.Chat.ListLimit)
				assert.InDelta(t, 2.0, c.RateLimit.RPS, 0.0001)
				assert.Equal(t, 10, c.RateLimit.Burst)
				assert.Equal(t, 24*time.Hour, c.Session.TTL)
				assert.Empty(t, c.Admin.Email)
				assert.False(t, c.SecureCookies())
			},
		},
		{
			name: "trailing slash is trimmed from base URLs",
			env:  with(requiredEnv(), map[string]string{"APP_BASE_URL": "https://chat.example.com/"}),
			check: func(t *testing.T, c config.Config) {
				assert.Equal(t, "http://localhost:9093/api", c.Agent.BaseURL)
				assert.Equal(t, "https://chat.example.com", c.App.BaseURL)
				assert.True(t, c.SecureCookies())
			},
		},
		{
			name: "every key can be overridden",
			env: with(requiredEnv(), map[string]string{
				"APP_ENV":                    "production",
				"APP_HTTP_ADDR":              ":9000",
				"APP_BASE_URL":               "https://chat.example.com",
				"APP_LOG_LEVEL":              "debug",
				"APP_LOG_FORMAT":             "text",
				"APP_SHUTDOWN_GRACE":         "30s",
				"HTTP_MAX_BODY_BYTES":        "16384",
				"HTTP_BODY_TIMEOUT":          "5s",
				"HTTP_RESPONSE_TIMEOUT":      "45s",
				"DB_MAX_CONNS":               "25",
				"DB_MIGRATE_ON_START":        "false",
				"AGENT_REQUEST_TIMEOUT":      "600s",
				"CHAT_MAX_MESSAGE_CHARS":     "500",
				"CHAT_STREAM_FLUSH_INTERVAL": "100ms",
				"CHAT_LIST_LIMIT":            "20",
				"RATE_LIMIT_RPS":             "0.5",
				"RATE_LIMIT_BURST":           "3",
				"SESSION_TTL":                "1h",
				"ADMIN_EMAIL":                "admin@example.com",
				"ADMIN_PASSWORD":             secretAdminPassword,
			}),
			check: func(t *testing.T, c config.Config) {
				assert.Equal(t, "production", c.App.Env)
				assert.Equal(t, ":9000", c.HTTP.Addr)
				assert.Equal(t, "debug", c.App.LogLevel)
				assert.Equal(t, "text", c.App.LogFormat)
				assert.Equal(t, 30*time.Second, c.App.ShutdownGrace)
				assert.Equal(t, int64(16384), c.HTTP.MaxBodyBytes)
				assert.Equal(t, 5*time.Second, c.HTTP.BodyTimeout)
				assert.Equal(t, 45*time.Second, c.HTTP.ResponseTimeout)
				assert.Equal(t, 25, c.DB.MaxConns)
				assert.False(t, c.DB.MigrateOnStart)
				assert.Equal(t, 600*time.Second, c.Agent.RequestTimeout)
				assert.Equal(t, 500, c.Chat.MaxMessageChars)
				assert.Equal(t, 100*time.Millisecond, c.Chat.StreamFlushInterval)
				assert.Equal(t, 20, c.Chat.ListLimit)
				assert.InDelta(t, 0.5, c.RateLimit.RPS, 0.0001)
				assert.Equal(t, 3, c.RateLimit.Burst)
				assert.Equal(t, time.Hour, c.Session.TTL)
				assert.Equal(t, "admin@example.com", c.Admin.Email)
			},
		},
		{
			name: "the smallest body limit that still fits the longest message",
			env: with(requiredEnv(), map[string]string{
				"CHAT_MAX_MESSAGE_CHARS": "1000",
				"HTTP_MAX_BODY_BYTES":    "16096", // 1000 characters of up to 12 bytes each, and 4096 for the rest
			}),
			check: func(t *testing.T, c config.Config) {
				assert.Equal(t, int64(16096), c.HTTP.MaxBodyBytes)
			},
		},
		{
			name: "an empty value counts as unset",
			env:  with(requiredEnv(), map[string]string{"APP_HTTP_ADDR": "", "DB_MAX_CONNS": ""}),
			check: func(t *testing.T, c config.Config) {
				assert.Equal(t, ":8300", c.HTTP.Addr)
				assert.Equal(t, 10, c.DB.MaxConns)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := config.Load(lookupFrom(tc.env))
			require.NoError(t, err)
			tc.check(t, c)
		})
	}
}

func TestLoad_Invalid(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantInErr  []string
		wantNotErr []string
	}{
		{
			name:      "all missing required keys are named in one error",
			env:       map[string]string{},
			wantInErr: []string{"DB_URL", "AGENT_BASE_URL", "AGENT_APP_NAME"},
		},
		{
			name: "bad scalar values name the key and the value",
			env: with(requiredEnv(), map[string]string{
				"APP_SHUTDOWN_GRACE":  "soon",
				"DB_MAX_CONNS":        "ten",
				"DB_MIGRATE_ON_START": "maybe",
				"RATE_LIMIT_RPS":      "fast",
			}),
			wantInErr: []string{
				"APP_SHUTDOWN_GRACE", `"soon"`,
				"DB_MAX_CONNS", `"ten"`,
				"DB_MIGRATE_ON_START", `"maybe"`,
				"RATE_LIMIT_RPS", `"fast"`,
			},
		},
		{
			name: "out of range values are rejected",
			env: with(requiredEnv(), map[string]string{
				"HTTP_MAX_BODY_BYTES":        "-1",
				"HTTP_BODY_TIMEOUT":          "0s",
				"HTTP_RESPONSE_TIMEOUT":      "-1s",
				"CHAT_MAX_MESSAGE_CHARS":     "0",
				"CHAT_STREAM_FLUSH_INTERVAL": "0s",
				"DB_MAX_CONNS":               "0",
				"RATE_LIMIT_BURST":           "0",
			}),
			wantInErr: []string{
				"HTTP_MAX_BODY_BYTES", "HTTP_BODY_TIMEOUT", "HTTP_RESPONSE_TIMEOUT", "CHAT_MAX_MESSAGE_CHARS", "CHAT_STREAM_FLUSH_INTERVAL",
				"DB_MAX_CONNS", "RATE_LIMIT_BURST",
			},
		},
		{
			name: "zero is no way to switch a limit off",
			env: with(requiredEnv(), map[string]string{
				"HTTP_RESPONSE_TIMEOUT": "0s",
				"RATE_LIMIT_RPS":        "0",
			}),
			wantInErr: []string{"HTTP_RESPONSE_TIMEOUT", "RATE_LIMIT_RPS"},
		},
		{
			name:       "a rate that is no number",
			env:        with(requiredEnv(), map[string]string{"RATE_LIMIT_RPS": "NaN"}),
			wantInErr:  []string{"RATE_LIMIT_RPS", `"NaN"`, "not a finite number"},
			wantNotErr: []string{"must be greater than zero"},
		},
		{
			name:       "a rate without end",
			env:        with(requiredEnv(), map[string]string{"RATE_LIMIT_RPS": "Inf"}),
			wantInErr:  []string{"RATE_LIMIT_RPS", `"Inf"`, "not a finite number"},
			wantNotErr: []string{"must be greater than zero"},
		},
		{
			name:       "a rate below every number",
			env:        with(requiredEnv(), map[string]string{"RATE_LIMIT_RPS": "-Inf"}),
			wantInErr:  []string{"RATE_LIMIT_RPS", `"-Inf"`, "not a finite number"},
			wantNotErr: []string{"must be greater than zero"},
		},
		{
			name:      "a listen address that is only a port",
			env:       with(requiredEnv(), map[string]string{"APP_HTTP_ADDR": "8300"}),
			wantInErr: []string{"APP_HTTP_ADDR", `"8300"`, "127.0.0.1:8300"},
		},
		{
			name: "unknown choices are rejected",
			env: with(requiredEnv(), map[string]string{
				"APP_ENV":        "staging",
				"APP_LOG_FORMAT": "xml",
				"APP_LOG_LEVEL":  "loud",
			}),
			wantInErr: []string{"APP_ENV", "APP_LOG_FORMAT", "APP_LOG_LEVEL"},
		},
		{
			name: "bad URLs name the key but never echo the value",
			env: map[string]string{
				"DB_URL":         "mysql://app:" + secretDBPassword + "@localhost/db",
				"AGENT_BASE_URL": "not a url",
				"AGENT_APP_NAME": "ui_agent",
				"APP_BASE_URL":   "ftp://files.example.com",
			},
			wantInErr:  []string{"DB_URL", "AGENT_BASE_URL", "APP_BASE_URL"},
			wantNotErr: []string{secretDBPassword, "not a url", "ftp://files.example.com"},
		},
		{
			name: "a body limit that cannot hold the longest message names both keys",
			env: with(requiredEnv(), map[string]string{
				"CHAT_MAX_MESSAGE_CHARS": "1000",
				"HTTP_MAX_BODY_BYTES":    "16095", // one byte short
			}),
			wantInErr: []string{"HTTP_MAX_BODY_BYTES", "CHAT_MAX_MESSAGE_CHARS", "16096"},
		},
		{
			name: "the old default body limit no longer fits the default message limit",
			env: with(requiredEnv(), map[string]string{
				"HTTP_MAX_BODY_BYTES": "65536",
			}),
			wantInErr: []string{"HTTP_MAX_BODY_BYTES", "CHAT_MAX_MESSAGE_CHARS"},
		},
		{
			name: "production over plain http would send cookies in the clear",
			env: with(requiredEnv(), map[string]string{
				"APP_ENV":      "production",
				"APP_BASE_URL": "http://chat.example.com",
			}),
			wantInErr: []string{"APP_ENV", "APP_BASE_URL", "https"},
		},
		{
			name:      "admin email without a password",
			env:       with(requiredEnv(), map[string]string{"ADMIN_EMAIL": "admin@example.com"}),
			wantInErr: []string{"ADMIN_PASSWORD"},
		},
		{
			name:       "admin password that is too short",
			env:        with(requiredEnv(), map[string]string{"ADMIN_EMAIL": "admin@example.com", "ADMIN_PASSWORD": "short"}),
			wantInErr:  []string{"ADMIN_PASSWORD", "12"},
			wantNotErr: []string{`"short"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(lookupFrom(tc.env))
			require.Error(t, err)
			for _, want := range tc.wantInErr {
				assert.Contains(t, err.Error(), want)
			}
			for _, unwanted := range tc.wantNotErr {
				assert.NotContains(t, err.Error(), unwanted)
			}
		})
	}
}

// The scripts and the health check take the port from APP_HTTP_ADDR, so it is a
// host, which may be left out, and a port number.
func TestLoad_ListenAddress(t *testing.T) {
	for _, addr := range []string{":8300", "127.0.0.1:8300", "0.0.0.0:80", "localhost:8300", "[::1]:8300", "[::]:65535"} {
		t.Run("taken: "+addr, func(t *testing.T) {
			c, err := config.Load(lookupFrom(with(requiredEnv(), map[string]string{"APP_HTTP_ADDR": addr})))

			require.NoError(t, err)
			assert.Equal(t, addr, c.HTTP.Addr)
		})
	}
	for _, addr := range []string{
		"8300", "localhost", "127.0.0.1", ":", ":http", ":0", ":65536", ":-1", "::1:8300",
		"http://localhost:8300", "127.0.0.1:8300/",
	} {
		t.Run("refused: "+addr, func(t *testing.T) {
			_, err := config.Load(lookupFrom(with(requiredEnv(), map[string]string{"APP_HTTP_ADDR": addr})))

			require.Error(t, err)
			assert.Contains(t, err.Error(), "APP_HTTP_ADDR must be a host and a port number")
		})
	}
}

func TestConfig_LogValueRedactsSecrets(t *testing.T) {
	c, err := config.Load(lookupFrom(with(requiredEnv(), map[string]string{
		"ADMIN_EMAIL":    "admin@example.com",
		"ADMIN_PASSWORD": secretAdminPassword,
	})))
	require.NoError(t, err)

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("config loaded", "config", c)

	out := buf.String()
	assert.NotContains(t, out, secretDBPassword)
	assert.NotContains(t, out, secretAdminPassword)
	assert.Contains(t, out, "localhost:5440")
	assert.Contains(t, out, "ui_agent")
}

func TestConfig_LogValueDropsCredentialsFromURLs(t *testing.T) {
	c, err := config.Load(lookupFrom(with(requiredEnv(), map[string]string{
		"AGENT_BASE_URL": "https://svc:agent-token-1@agent.example.com/api?key=query-secret-2",
		"APP_BASE_URL":   "https://user-token-3@chat.example.com",
	})))
	require.NoError(t, err)

	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("config loaded", "config", c)

	out := buf.String()
	for _, secret := range []string{"agent-token-1", "query-secret-2", "user-token-3", "svc:"} {
		assert.NotContains(t, out, secret)
	}
	assert.Contains(t, out, "https://agent.example.com/api", "where it points is still logged")
	assert.Contains(t, out, "https://chat.example.com")
}

// The start line shows every setting under the name of its field, so a setting
// that is added is logged without anybody adding it to a list.
func TestConfig_LogValueShowsEverySetting(t *testing.T) {
	c, err := config.Load(lookupFrom(with(requiredEnv(), map[string]string{
		"ADMIN_EMAIL":    "admin@example.com",
		"ADMIN_PASSWORD": secretAdminPassword,
	})))
	require.NoError(t, err)
	// A record without a time, so that the handlers write none.
	record := slog.NewRecord(time.Time{}, slog.LevelInfo, "starting", 0)
	record.AddAttrs(slog.Any("config", c))

	t.Run("as text", func(t *testing.T) {
		var buf bytes.Buffer

		require.NoError(t, slog.NewTextHandler(&buf, nil).Handle(t.Context(), record))

		assert.Equal(t, "level=INFO msg=starting"+
			" config.App.Env=development config.App.BaseURL=http://localhost:8300 config.App.LogLevel=info"+
			" config.App.LogFormat=json config.App.ShutdownGrace=10s"+
			" config.HTTP.Addr=:8300 config.HTTP.MaxBodyBytes=131072 config.HTTP.BodyTimeout=30s config.HTTP.ResponseTimeout=2m0s"+
			" config.DB.URL=[redacted] config.DB.MaxConns=10 config.DB.MigrateOnStart=true"+
			" config.Agent.BaseURL=http://localhost:9093/api config.Agent.AppName=ui_agent config.Agent.RequestTimeout=2m0s"+
			" config.Chat.MaxMessageChars=8000 config.Chat.StreamFlushInterval=50ms config.Chat.ListLimit=100"+
			" config.RateLimit.RPS=2 config.RateLimit.Burst=10 config.Session.TTL=24h0m0s"+
			" config.Admin.Email=[redacted] config.Admin.Password=[redacted]"+
			" config.db=localhost:5440/agentwebui config.admin_bootstrap=true\n", buf.String())
	})

	t.Run("as JSON", func(t *testing.T) {
		var buf bytes.Buffer

		require.NoError(t, slog.NewJSONHandler(&buf, nil).Handle(t.Context(), record))

		assert.JSONEq(t, `{"level":"INFO","msg":"starting","config":{
			"App":{"Env":"development","BaseURL":"http://localhost:8300","LogLevel":"info","LogFormat":"json","ShutdownGrace":10000000000},
			"HTTP":{"Addr":":8300","MaxBodyBytes":131072,"BodyTimeout":30000000000,"ResponseTimeout":120000000000},
			"DB":{"URL":"[redacted]","MaxConns":10,"MigrateOnStart":true},
			"Agent":{"BaseURL":"http://localhost:9093/api","AppName":"ui_agent","RequestTimeout":120000000000},
			"Chat":{"MaxMessageChars":8000,"StreamFlushInterval":50000000,"ListLimit":100},
			"RateLimit":{"RPS":2,"Burst":10},
			"Session":{"TTL":86400000000000},
			"Admin":{"Email":"[redacted]","Password":"[redacted]"},
			"db":"localhost:5440/agentwebui","admin_bootstrap":true}}`, buf.String())
	})
}

// The view comes from redact.Struct, which answers a name that is no field with
// its placeholder alone: a group with every part of the settings says that the
// view names real fields.
func TestConfig_LogValueNamesRealFields(t *testing.T) {
	c, err := config.Load(lookupFrom(requiredEnv()))
	require.NoError(t, err)

	view := c.LogValue()

	require.Equal(t, slog.KindGroup, view.Kind())
	var shown, want []string
	for _, attr := range view.Group() {
		shown = append(shown, attr.Key)
	}
	for field := range reflect.TypeFor[config.Config]().Fields() {
		want = append(want, field.Name)
	}
	assert.Equal(t, append(want, "db", "admin_bootstrap"), shown,
		"every part of the settings, then where the database is and whether a first user is set")
}

// The address of the first user names a person, and the start line is kept with
// the logs. The view hides the address, and admin_bootstrap says whether one is
// set.
func TestConfig_LogValueKeepsTheAddressOfTheFirstUserOut(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		set  bool
	}{
		{"a first user is set", with(requiredEnv(), map[string]string{
			"ADMIN_EMAIL": "first.user@people.example", "ADMIN_PASSWORD": secretAdminPassword,
		}), true},
		{"no first user is set", requiredEnv(), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := config.Load(lookupFrom(tc.env))
			require.NoError(t, err)

			var asText, asJSON bytes.Buffer
			slog.New(slog.NewTextHandler(&asText, nil)).Info("starting", "config", c)
			slog.New(slog.NewJSONHandler(&asJSON, nil)).Info("starting", "config", c)

			for _, out := range []string{asText.String(), asJSON.String()} {
				assert.NotContains(t, out, "first.user")
				assert.NotContains(t, out, "people.example")
			}
			assert.Contains(t, asText.String(), "config.Admin.Email=[redacted]")
			assert.Contains(t, asText.String(), fmt.Sprintf("config.admin_bootstrap=%t", tc.set))
			assert.Contains(t, asJSON.String(), fmt.Sprintf(`"admin_bootstrap":%t`, tc.set))
		})
	}
}

// The view shows every field and hides the ones that are a Secret, so a field
// whose name says that it holds a secret has to be one.
func TestConfig_AFieldNamedLikeASecretIsASecret(t *testing.T) {
	// What the evaluator's store and the browser's environment take for the name of a secret.
	named := regexp.MustCompile(`(?i)(password|passwd|secret|token|cookie|credential|api[_-]?key|private[_-]?key)`)
	found := 0
	var check func(of reflect.Type, path string)
	check = func(of reflect.Type, path string) {
		switch of.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			check(of.Elem(), path)
		case reflect.Struct:
			for field := range of.Fields() {
				if named.MatchString(field.Name) {
					found++
					assert.Equal(t, reflect.TypeFor[envcfg.Secret](), field.Type, "%s.%s is shown as it is", path, field.Name)
				}
				check(field.Type, path+"."+field.Name)
			}
		}
	}

	check(reflect.TypeFor[config.Config](), "Config")

	assert.Positive(t, found, "the walk reached the password of the first user")
}

// refusedURL is the text Load has for a URL setting it does not take: the name
// of the setting, the schemes it may have, and nothing of its value.
func refusedURL(key, schemes string) string {
	return key + " must be an absolute " + schemes + " URL, with @, /, ? and # percent-encoded in its credentials (%40, %2F, %3F, %23)"
}

// Two readers take the address of the database apart: net/url, for Load and
// the start line, and the driver, which ends the user and the password at the
// first @ where net/url ends them at the last. An @, a slash, a question mark
// or a hash sign that stands in a password as it is moves that end for one of
// the two, and the errors and the log lines that follow name pieces of the
// password as a host, a database or a parameter. Load refuses such an address,
// in a text that holds nothing of it.
func TestLoad_RefusesAURLWhoseCredentialsAreNotEncoded(t *testing.T) {
	const database, web = "postgres or postgresql", "http or https"
	tests := []struct {
		name, key, value, schemes string
	}{
		{"an @ in the password of the database", "DB_URL", "postgres://agentwebui:p@ss@postgres:5432/agentwebui?sslmode=disable", database},
		{"an @ and a slash", "DB_URL", "postgres://app:P@ss/w0rd-canary-1@localhost:5440/agentwebui", database},
		{"an @ and a question mark", "DB_URL", "postgres://app:P@ss?word@db:5432/app", database},
		{"a hash sign after digits, which pass for a port", "DB_URL", "postgres://app:2024#word-canary-4@db:5432/app", database},
		{"a slash after letters, which no port is made of", "DB_URL", "postgres://app:pa/ss-canary-5@db:5432/app", database},
		{"the address of the agent", "AGENT_BASE_URL", "https://svc:P@ss/w0rd-canary-2@agent.example.com/api", web},
		{"the address of the page", "APP_BASE_URL", "https://user:P@ss/w0rd-canary-3@chat.example.com", web},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(lookupFrom(with(requiredEnv(), map[string]string{tc.key: tc.value})))

			require.Error(t, err)
			assert.Equal(t, refusedURL(tc.key, tc.schemes), err.Error(), "the one problem, in the fixed text")
			for _, piece := range []string{"w0rd", "word", "ss", "canary"} {
				assert.NotContains(t, err.Error(), piece)
			}
		})
	}
}

// The same password with its four signs encoded is one address to both
// readers. Load takes it as it is given, and the start line shows where the
// database is and nothing of the password.
func TestLoad_TakesAURLWhoseCredentialsAreEncoded(t *testing.T) {
	const dbURL = "postgres://app:P%40ss%2Fw0rd%3Fcanary%231@localhost:5440/agentwebui?sslmode=disable"

	c, err := config.Load(lookupFrom(with(requiredEnv(), map[string]string{
		"DB_URL":         dbURL,
		"AGENT_BASE_URL": "https://svc:P%40ss%2Fw0rd-canary-2@agent.example.com/api",
	})))

	require.NoError(t, err)
	assert.True(t, c.DB.URL.Reveal() == dbURL, "as it was given: the driver decodes it")
	var asText, asJSON bytes.Buffer
	slog.New(slog.NewTextHandler(&asText, nil)).Info("starting", "config", c)
	slog.New(slog.NewJSONHandler(&asJSON, nil)).Info("starting", "config", c)
	for _, out := range []string{asText.String(), asJSON.String()} {
		for _, piece := range []string{"w0rd", "canary", "P%40", "%2F", "svc"} {
			assert.NotContains(t, out, piece)
		}
	}
	assert.Contains(t, asText.String(), "config.Agent.BaseURL=https://agent.example.com/api")
	assert.Contains(t, asText.String(), "config.db=localhost:5440/agentwebui")
}

// A slash ends the host of an address. A password that holds one, put into the
// address as it is, leaves a part of itself and the host that was meant in the
// path. Load refuses such an address. A Config that was put together by hand
// may hold one, and its view shows nothing of it.
func TestConfig_LogValueDropsCredentialsThatWereNotEncoded(t *testing.T) {
	c := config.Config{
		App:   config.App{BaseURL: "https://user:P@ss/w0rd-canary-3@chat.example.com"},
		DB:    config.DB{URL: "postgres://app:P@ss/w0rd-canary-1@localhost:5440/agentwebui"},
		Agent: config.Agent{BaseURL: "https://svc:P@ss/w0rd-canary-2@agent.example.com/api"},
	}

	var asText, asJSON bytes.Buffer
	slog.New(slog.NewTextHandler(&asText, nil)).Info("starting", "config", c)
	slog.New(slog.NewJSONHandler(&asJSON, nil)).Info("starting", "config", c)

	for _, out := range []string{asText.String(), asJSON.String()} {
		assert.NotContains(t, out, "canary")
		assert.NotContains(t, out, "w0rd")
	}
	for _, shown := range []string{"config.App.BaseURL=unreadable", "config.Agent.BaseURL=unreadable", "config.db=unreadable"} {
		assert.Contains(t, asText.String(), shown)
	}
}

// Load makes sure that DB_URL is an address. A Config that was put together
// by hand may hold the other form of a connection string, or nothing.
func TestConfig_LogValueOfADatabaseThatIsNoAddress(t *testing.T) {
	tests := []struct {
		name string
		url  envcfg.Secret
		want string
	}{
		{"keywords and values", "host=localhost user=app password=" + secretDBPassword + " dbname=agentwebui", "config.db=unreadable"},
		{"nothing", "", "config.db=unset"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer

			slog.New(slog.NewTextHandler(&buf, nil)).Info("starting", "config", config.Config{DB: config.DB{URL: tc.url}})

			assert.NotContains(t, buf.String(), secretDBPassword)
			assert.Contains(t, buf.String(), tc.want)
		})
	}
}

func TestEnvExample(t *testing.T) {
	const path = "../../../.env.example"
	for _, key := range []string{"DB_URL", "AGENT_BASE_URL", "AGENT_APP_NAME", "ADMIN_EMAIL", "ADMIN_PASSWORD"} {
		t.Setenv(key, "") // an empty value counts as unset
	}

	t.Run("loads without errors", func(t *testing.T) {
		c, err := config.LoadEnv(path)
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1:8300", c.HTTP.Addr, "run from source, the page is for this computer only unless you open it up")
		assert.Equal(t, "ui_agent", c.Agent.AppName)
	})

	t.Run("is safe to source from a shell", func(t *testing.T) {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		for line := range strings.SplitSeq(string(raw), "\n") {
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, found := strings.Cut(line, "=")
			require.True(t, found, "not a KEY=value line: %q", line)
			assert.NotContains(t, key, " ", "no spaces around the key: %q", line)
			assert.NotContains(t, value, " #", "inline comment: %q", line)
			quoted := len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0]
			if !quoted {
				assert.False(t, strings.ContainsAny(value, " &;|<>()$`\\"), "unquoted shell metacharacter in %q", line)
			}
		}
	})
}

func TestLoadEnv(t *testing.T) {
	const fileContent = "# local settings\n" +
		"DB_URL=postgres://app:pw@localhost:5440/agentwebui\n" +
		"AGENT_BASE_URL=http://localhost:9093/api\n" +
		"AGENT_APP_NAME=from_file\n" +
		"CHAT_LIST_LIMIT=7\n"

	tests := []struct {
		name        string
		writeFile   bool
		processEnv  map[string]string
		wantAppName string
		wantLimit   int
		wantErr     bool
	}{
		{name: "file values are used", writeFile: true, wantAppName: "from_file", wantLimit: 7},
		{
			name:        "process env wins over the file",
			writeFile:   true,
			processEnv:  map[string]string{"AGENT_APP_NAME": "from_env"},
			wantAppName: "from_env",
			wantLimit:   7,
		},
		{
			name:        "missing file falls back to the process env",
			processEnv:  requiredEnv(),
			wantAppName: "ui_agent",
			wantLimit:   100,
		},
		{name: "missing file and no env reports the required keys", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{"DB_URL", "AGENT_BASE_URL", "AGENT_APP_NAME", "CHAT_LIST_LIMIT"} {
				t.Setenv(key, "") // an empty value counts as unset
			}
			for k, v := range tc.processEnv {
				t.Setenv(k, v)
			}
			path := filepath.Join(t.TempDir(), ".env")
			if tc.writeFile {
				require.NoError(t, os.WriteFile(path, []byte(fileContent), 0o600))
			}

			c, err := config.LoadEnv(path)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "DB_URL")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantAppName, c.Agent.AppName)
			assert.Equal(t, tc.wantLimit, c.Chat.ListLimit)
		})
	}
}

// Settings live in .env, and .env.example is where they are written down, with
// their defaults: a key the program reads and the example lacks is a setting
// nobody can find.
func TestEnvExampleDocumentsEveryKey(t *testing.T) {
	raw, err := os.ReadFile("../../../.env.example")
	require.NoError(t, err)
	example := string(raw)

	// Load asks for each key it knows; collect them.
	var asked []string
	_, _ = config.Load(func(key string) (string, bool) {
		asked = append(asked, key)
		return "", false
	})
	require.NotEmpty(t, asked)

	for _, key := range asked {
		documented := strings.Contains(example, "\n"+key+"=") || strings.Contains(example, "\n# "+key+"=")
		assert.True(t, documented, "%s is read by the Web UI but not documented in .env.example", key)
	}
}
