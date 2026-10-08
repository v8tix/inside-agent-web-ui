package config_test

import (
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/core/domain"
	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/config"
)

func env(pairs map[string]string) config.LookupFunc {
	return func(key string) (string, bool) {
		v, ok := pairs[key]
		return v, ok
	}
}

func TestLoad_Defaults(t *testing.T) {
	c, err := config.Load(env(nil))

	require.NoError(t, err)
	assert.Equal(t, "https://datasets-server.huggingface.co", c.HF.DatasetsServerURL)
	assert.Equal(t, "https://huggingface.co", c.HF.HubURL)
	assert.Equal(t, "gaia-benchmark/GAIA", c.GAIA.Dataset)
	assert.Equal(t, []string{"2023_level1"}, c.GAIA.Levels)
	assert.Equal(t, "./datasets", c.GAIA.DatasetsDir)
	assert.Equal(t, "validation", c.GAIA.Split)
	assert.Equal(t, domain.AttachmentsSkip, c.GAIA.Attachments)
	assert.Equal(t, "./results", c.ResultsDir)
	assert.Equal(t, "http://localhost:8300", c.Eval.BaseURL)
	assert.Equal(t, 10*time.Minute, c.Eval.TaskTimeout)
	assert.Equal(t, 1, c.Eval.Workers)
	assert.Equal(t, 1, c.Eval.Repeats)
	assert.Equal(t, 25*time.Millisecond, c.Eval.TypingDelay)
	assert.False(t, c.Eval.KeepChats)
	assert.Empty(t, c.Eval.PromptFile)
	assert.NotEmpty(t, c.Eval.ProbePrompt)
	assert.Equal(t, []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}, c.MCP.Command)
	assert.Equal(t, []string{"--headless", "--isolated", "--no-usage-statistics", "--no-performance-crux"}, c.MCP.Args)
	assert.Equal(t, domain.DefaultThresholds(), c.Thresholds)
	assert.Empty(t, c.Warnings)
}

func TestLoad_Overrides(t *testing.T) {
	c, err := config.Load(env(map[string]string{
		"HF_TOKEN":                  "hf_secret",
		"HF_GAIA_DATASET":           "my-org/gaia-copy",
		"HF_GAIA_LEVELS":            "2023_level1, 2023_level2,2023_level1",
		"HF_DATASETS_DIR":           "/data/gaia",
		"GAIA_SPLIT":                "test",
		"GAIA_ATTACHMENTS":          "include",
		"RESULTS_DIR":               "/data/results",
		"EVAL_BASE_URL":             "http://web:8300/",
		"EVAL_USER_EMAIL":           "eval@example.com",
		"EVAL_USER_PASSWORD":        "long enough password",
		"EVAL_TASK_TIMEOUT":         "45s",
		"EVAL_WORKERS":              "3",
		"EVAL_REPEATS":              "5",
		"EVAL_TYPING_DELAY":         "0s",
		"EVAL_KEEP_CHATS":           "true",
		"EVAL_PROMPT_FILE":          "prompt.tmpl",
		"EVAL_PROBE_PROMPT":         "Tell me a long story.",
		"EVAL_UI_OVERHEAD_P95_MS":   "250",
		"EVAL_LIGHTHOUSE_MIN_SCORE": "95",
		"EVAL_CLS_MAX":              "0.05",
	}))

	require.NoError(t, err)
	assert.Equal(t, "hf_secret", c.HF.Token.Reveal())
	assert.Equal(t, "my-org/gaia-copy", c.GAIA.Dataset)
	assert.Equal(t, []string{"2023_level1", "2023_level2"}, c.GAIA.Levels, "trimmed, and a repeat is dropped")
	assert.Equal(t, "/data/gaia", c.GAIA.DatasetsDir)
	assert.Equal(t, "test", c.GAIA.Split)
	assert.Equal(t, domain.AttachmentsInclude, c.GAIA.Attachments)
	assert.Equal(t, "/data/results", c.ResultsDir)
	assert.Equal(t, "http://web:8300", c.Eval.BaseURL, "no trailing slash")
	assert.Equal(t, "eval@example.com", c.Eval.UserEmail)
	assert.False(t, c.Eval.UserFromAdmin)
	assert.Equal(t, 45*time.Second, c.Eval.TaskTimeout)
	assert.Equal(t, 3, c.Eval.Workers)
	assert.Equal(t, 5, c.Eval.Repeats)
	assert.Zero(t, c.Eval.TypingDelay, "no delay is allowed")
	assert.True(t, c.Eval.KeepChats)
	assert.Equal(t, "prompt.tmpl", c.Eval.PromptFile)
	assert.Equal(t, "Tell me a long story.", c.Eval.ProbePrompt)
	assert.InDelta(t, 250, c.Thresholds.OverheadP95Ms, 1e-9)
	assert.Equal(t, 95, c.Thresholds.LighthouseMin)
	assert.InDelta(t, 0.05, c.Thresholds.CLSMax, 1e-9)
	assert.Equal(t, 200, c.Thresholds.StreamMinChars, "untouched thresholds keep their defaults")
}

func TestLoad_EmptyValuesCountAsUnset(t *testing.T) {
	c, err := config.Load(env(map[string]string{"EVAL_WORKERS": "  ", "GAIA_SPLIT": "", "EVAL_BASE_URL": ""}))

	require.NoError(t, err)
	assert.Equal(t, 1, c.Eval.Workers)
	assert.Equal(t, "validation", c.GAIA.Split)
	assert.Equal(t, "http://localhost:8300", c.Eval.BaseURL)
}

func TestLoad_Problems(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"an unpinned MCP command", map[string]string{"EVAL_MCP_COMMAND": "npx -y chrome-devtools-mcp@latest"}, "must pin a version"},
		{"no version at all", map[string]string{"EVAL_MCP_COMMAND": "npx -y chrome-devtools-mcp"}, "must pin a version"},
		{"a different server", map[string]string{"EVAL_MCP_COMMAND": "npx -y playwright-mcp@1.0.0"}, "must pin a version of chrome-devtools-mcp"},
		{"a version range", map[string]string{"EVAL_MCP_COMMAND": "npx -y chrome-devtools-mcp@^1.10.1"}, "must pin a version"},
		{"headless dropped", map[string]string{"EVAL_MCP_ARGS": "--isolated --no-usage-statistics --no-performance-crux"}, "missing: --headless"},
		{"isolated dropped", map[string]string{"EVAL_MCP_ARGS": "--headless --no-usage-statistics --no-performance-crux"}, "missing: --isolated"},
		{"statistics not switched off", map[string]string{"EVAL_MCP_ARGS": "--headless --isolated --no-performance-crux"}, "missing: --no-usage-statistics"},
		{"crux not switched off", map[string]string{"EVAL_MCP_ARGS": "--headless --isolated --no-usage-statistics"}, "missing: --no-performance-crux"},
		{"no flags at all", map[string]string{"EVAL_MCP_ARGS": "--slim"}, "EVAL_MCP_ARGS must keep"},
		{"a bad level", map[string]string{"HF_GAIA_LEVELS": "level1"}, "not a GAIA config"},
		{"no level", map[string]string{"HF_GAIA_LEVELS": " , "}, "lists no GAIA config"},
		{"a bad dataset name", map[string]string{"HF_GAIA_DATASET": "gaia"}, "owner/name"},
		{"a bad split", map[string]string{"GAIA_SPLIT": "train"}, "GAIA_SPLIT must be one of"},
		{"a bad attachments mode", map[string]string{"GAIA_ATTACHMENTS": "attach"}, "GAIA_ATTACHMENTS"},
		{"a bad duration", map[string]string{"EVAL_TASK_TIMEOUT": "ten minutes"}, "EVAL_TASK_TIMEOUT"},
		{"a zero timeout", map[string]string{"EVAL_TASK_TIMEOUT": "0s"}, "EVAL_TASK_TIMEOUT must be greater than zero"},
		{"no workers", map[string]string{"EVAL_WORKERS": "0"}, "EVAL_WORKERS must be greater than zero"},
		{"a negative typing delay", map[string]string{"EVAL_TYPING_DELAY": "-1ms"}, "EVAL_TYPING_DELAY must not be negative"},
		{"not a boolean", map[string]string{"EVAL_KEEP_CHATS": "maybe"}, "EVAL_KEEP_CHATS"},
		{"a relative base URL", map[string]string{"EVAL_BASE_URL": "localhost:8300"}, "EVAL_BASE_URL must be an absolute"},
		{"a user without a password", map[string]string{"EVAL_USER_EMAIL": "a@example.com"}, "EVAL_USER_PASSWORD is required"},
		{"a password without a user", map[string]string{"EVAL_USER_PASSWORD": "x"}, "EVAL_USER_EMAIL is required"},
		{"a bad email", map[string]string{"EVAL_USER_EMAIL": "Eval <e@example.com>", "EVAL_USER_PASSWORD": "x"}, "plain email address"},
		{"a score out of range", map[string]string{"EVAL_LIGHTHOUSE_MIN_SCORE": "120"}, "between 1 and 100"},
		{"a threshold of zero", map[string]string{"EVAL_SCROLL_FOLLOW_PX": "0"}, "must be greater than zero"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(env(tc.env))

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	t.Run("every problem is reported at once", func(t *testing.T) {
		_, err := config.Load(env(map[string]string{
			"EVAL_MCP_COMMAND": "npx chrome-devtools-mcp", "EVAL_WORKERS": "-2", "GAIA_SPLIT": "train",
		}))

		require.Error(t, err)
		for _, want := range []string{"must pin a version", "EVAL_WORKERS", "GAIA_SPLIT"} {
			assert.Contains(t, err.Error(), want)
		}
	})

	t.Run("a URL with credentials is never echoed", func(t *testing.T) {
		_, err := config.Load(env(map[string]string{"HF_HUB_URL": "ftp://user:hunter2@host"}))

		require.Error(t, err)
		assert.NotContains(t, err.Error(), "hunter2")
	})
}

func TestLoad_User(t *testing.T) {
	t.Run("the Web UI's first user is the default", func(t *testing.T) {
		c, err := config.Load(env(map[string]string{"ADMIN_EMAIL": "admin@example.com", "ADMIN_PASSWORD": "admin password 1"}))

		require.NoError(t, err)
		assert.Equal(t, "admin@example.com", c.Eval.UserEmail)
		assert.Equal(t, "admin password 1", c.Eval.UserPassword.Reveal())
		assert.True(t, c.Eval.UserFromAdmin)
		require.NoError(t, c.RequireUser())
	})

	t.Run("EVAL_USER_* wins over the admin", func(t *testing.T) {
		c, err := config.Load(env(map[string]string{
			"ADMIN_EMAIL": "admin@example.com", "ADMIN_PASSWORD": "admin password 1",
			"EVAL_USER_EMAIL": "eval@example.com", "EVAL_USER_PASSWORD": "eval password 1",
		}))

		require.NoError(t, err)
		assert.Equal(t, "eval@example.com", c.Eval.UserEmail)
		assert.False(t, c.Eval.UserFromAdmin)
	})

	t.Run("no user is fine until a command needs one", func(t *testing.T) {
		c, err := config.Load(env(nil))

		require.NoError(t, err)
		require.ErrorContains(t, c.RequireUser(), "no user to sign in as")
	})
}

func TestRequireToken(t *testing.T) {
	without, err := config.Load(env(nil))
	require.NoError(t, err)
	require.ErrorContains(t, without.RequireToken(), "HF_TOKEN is not set")

	with, err := config.Load(env(map[string]string{"HF_TOKEN": "hf_x"}))
	require.NoError(t, err)
	require.NoError(t, with.RequireToken())
}

func TestLoad_Warnings(t *testing.T) {
	t.Run("a task timeout longer than the agent's", func(t *testing.T) {
		c, err := config.Load(env(map[string]string{"EVAL_TASK_TIMEOUT": "20m", "AGENT_REQUEST_TIMEOUT": "10m"}))

		require.NoError(t, err)
		require.Len(t, c.RunWarnings, 1, "it matters to `run` only")
		assert.Contains(t, c.RunWarnings[0], "EVAL_TASK_TIMEOUT (20m0s) is longer than AGENT_REQUEST_TIMEOUT (10m0s)")
		assert.Empty(t, c.Warnings)
	})
	t.Run("a shorter one is fine", func(t *testing.T) {
		c, err := config.Load(env(map[string]string{"EVAL_TASK_TIMEOUT": "5m", "AGENT_REQUEST_TIMEOUT": "10m"}))

		require.NoError(t, err)
		assert.Empty(t, c.Warnings)
		assert.Empty(t, c.RunWarnings)
	})
	t.Run("the test split cannot be graded", func(t *testing.T) {
		c, err := config.Load(env(map[string]string{"GAIA_SPLIT": "test"}))

		require.NoError(t, err)
		require.Len(t, c.Warnings, 1)
		assert.Contains(t, c.Warnings[0], "no published answers")
		assert.Empty(t, c.RunWarnings)
	})
}

func TestCheckMCP(t *testing.T) {
	good := []string{"--headless", "--isolated", "--no-usage-statistics", "--no-performance-crux"}
	tests := []struct {
		name    string
		command []string
		args    []string
		wantErr bool
	}{
		{"pinned, all flags", []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}, good, false},
		{"extra flags are fine", []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}, append([]string{"--slim"}, good...), false},
		{"a pre-release pin", []string{"npx", "chrome-devtools-mcp@1.11.0-rc.1"}, good, false},
		{"unpinned", []string{"npx", "-y", "chrome-devtools-mcp"}, good, true},
		{"latest", []string{"npx", "-y", "chrome-devtools-mcp@latest"}, good, true},
		{"a flag missing", []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}, good[:3], true},
		{"nothing", nil, nil, true},
		// The pin is only worth something if it is the package that runs. A check
		// that finds the pinned name anywhere in the command passes these.
		{"npx by its path", []string{"/usr/local/bin/npx", "-y", "chrome-devtools-mcp@1.10.1"}, good, false},
		{"npx with its quiet flags", []string{"npx", "--yes", "--no-install", "--prefer-offline", "-q", "chrome-devtools-mcp@1.10.1"}, good, false},
		{"another package first, the pinned name after it", []string{"npx", "-y", "other-package", "chrome-devtools-mcp@1.10.1"}, good, true},
		{"another package named by a flag", []string{"npx", "-p", "other-package", "chrome-devtools-mcp@1.10.1"}, good, true},
		{"another package named by a flag with a value", []string{"npx", "--package=other-package", "chrome-devtools-mcp@1.10.1"}, good, true},
		{"a flag npx does not need for this", []string{"npx", "--call=evil", "chrome-devtools-mcp@1.10.1"}, good, true},
		{"a shell with the name in its script", []string{"sh", "-c", "chrome-devtools-mcp@1.10.1"}, good, true},
		{"node", []string{"node", "chrome-devtools-mcp@1.10.1"}, good, true},
		{"npx behind another command", []string{"env", "npx", "chrome-devtools-mcp@1.10.1"}, good, true},
		{"the pin with something stuck on it", []string{"npx", "chrome-devtools-mcp@1.10.1;rm"}, good, true},
		{"only npx", []string{"npx"}, good, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := config.CheckMCP(tc.command, tc.args)

			assert.Equal(t, tc.wantErr, err != nil, "%v", err)
		})
	}
}

func TestSecretsDoNotPrint(t *testing.T) {
	c, err := config.Load(env(map[string]string{
		"HF_TOKEN": "hf_CANARY_TOKEN", "EVAL_USER_EMAIL": "e@example.com", "EVAL_USER_PASSWORD": "CANARY_PASSWORD",
	}))
	require.NoError(t, err)

	t.Run("every way of formatting the configuration", func(t *testing.T) {
		for name, text := range map[string]string{
			"%v":     fmt.Sprintf("%v", c),
			"%+v":    fmt.Sprintf("%+v", c),
			"%#v":    fmt.Sprintf("%#v", c),
			"String": c.String(),
			"eval":   fmt.Sprintf("%+v", c.Eval),
			"hf":     fmt.Sprintf("%+v", c.HF),
			"token":  fmt.Sprint(c.HF.Token),
		} {
			assert.NotContains(t, text, "CANARY", name)
		}
	})

	t.Run("a log line", func(t *testing.T) {
		var buf strings.Builder
		slog.New(slog.NewTextHandler(&buf, nil)).Info("loaded", "config", c, "token", c.HF.Token)

		assert.NotContains(t, buf.String(), "CANARY")
		assert.Contains(t, buf.String(), "hf_token_set=true", "it says whether the token is set, not what it is")
	})

	// An address can carry a user and a password, and a token in its query. Every
	// setting that is one gets them here, whatever settings there are.
	t.Run("an address shows where it points and nothing else", func(t *testing.T) {
		addresses := map[string]string{}
		_, _ = config.Load(func(key string) (string, bool) {
			if name, ok := strings.CutSuffix(key, "_URL"); ok {
				host := strings.ToLower(strings.ReplaceAll(name, "_", "-")) + ".example"
				addresses[key] = "https://eval:CANARY_PASSWORD@" + host + ":8300/ui?token=CANARY_QUERY"
			}
			return "", false
		})
		require.Len(t, addresses, 3, "EVAL_BASE_URL and the two hosts of the dataset")
		c, err := config.Load(env(addresses))
		require.NoError(t, err)
		var asText, asJSON strings.Builder
		slog.New(slog.NewTextHandler(&asText, nil)).Info("loaded", "config", c)
		slog.New(slog.NewJSONHandler(&asJSON, nil)).Info("loaded", "config", c)

		for name, text := range map[string]string{
			"%v": fmt.Sprintf("%v", c), "String": c.String(), "as text": asText.String(), "as JSON": asJSON.String(),
		} {
			assert.NotContains(t, text, "CANARY", name)
			assert.NotContains(t, text, "eval:", name)
			for _, host := range []string{"eval-base.example", "hf-datasets-server.example", "hf-hub.example"} {
				assert.Contains(t, text, "https://"+host+":8300/ui", name)
			}
		}
	})

	// A slash ends the host of an address, so a password that holds one and was
	// not encoded leaves a part of itself in the path. Load refuses such an
	// address. A Config that was put together by hand may hold one.
	t.Run("an address whose credentials were not encoded shows nothing", func(t *testing.T) {
		c := config.Config{
			HF:   config.HuggingFace{DatasetsServerURL: notEncoded, HubURL: notEncoded},
			Eval: config.Eval{BaseURL: notEncoded},
		}
		var asText strings.Builder
		slog.New(slog.NewTextHandler(&asText, nil)).Info("loaded", "config", c)

		for name, text := range map[string]string{"String": c.String(), "as text": asText.String()} {
			assert.NotContains(t, text, "CANARY", name)
			assert.Equal(t, 3, strings.Count(text, "URL=unreadable"), name)
		}
	})
}

// notEncoded is an address whose password holds an @ and a slash as they are.
// The slash ends the host, so net/url takes "ss" for the host, and the rest of
// the password and the host that was meant for the path.
const notEncoded = "https://eval:P@ss/CANARY_REST@web.example:8300/ui"

// A request to such an address goes to the host that the slash leaves, a piece
// of the password, and the error that follows names it. Load refuses the
// address in every setting that is one, in a text that holds nothing of it.
func TestLoad_RefusesAURLWhoseCredentialsAreNotEncoded(t *testing.T) {
	var addresses []string
	_, _ = config.Load(func(key string) (string, bool) {
		if strings.HasSuffix(key, "_URL") {
			addresses = append(addresses, key)
		}
		return "", false
	})
	require.Len(t, addresses, 3, "EVAL_BASE_URL and the two hosts of the dataset")

	for _, key := range addresses {
		t.Run(key, func(t *testing.T) {
			_, err := config.Load(env(map[string]string{key: notEncoded}))

			require.Error(t, err)
			assert.Equal(t, key+" must be an absolute http or https URL, with @, /, ? and # percent-encoded in its credentials (%40, %2F, %3F, %23)",
				err.Error(), "the one problem, in the fixed text")
			for _, piece := range []string{"CANARY", "ss", "web.example"} {
				assert.NotContains(t, err.Error(), piece)
			}
		})
	}

	t.Run("and takes it with the two signs encoded", func(t *testing.T) {
		const encoded = "https://eval:P%40ss%2FCANARY_REST@web.example:8300/ui"

		c, err := config.Load(env(map[string]string{"EVAL_BASE_URL": encoded}))

		require.NoError(t, err)
		assert.True(t, c.Eval.BaseURL == encoded, "as it was given: the browser decodes it")
		assert.NotContains(t, c.String(), "CANARY")
		assert.Contains(t, c.String(), "BaseURL=https://web.example:8300/ui")
	})
}

// The evaluator signs in with an account of the Web UI, its first user unless
// it is given another. The view hides the address of that account, as the Web
// UI's view hides the address of its first user.
func TestConfig_LogValueKeepsTheAddressOfTheAccountOut(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"an account of its own", map[string]string{"EVAL_USER_EMAIL": "eval.user@people.example", "EVAL_USER_PASSWORD": "a password of its own"}},
		{"the first user of the Web UI", map[string]string{"ADMIN_EMAIL": "first.user@people.example", "ADMIN_PASSWORD": "the first password"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := config.Load(env(tc.env))
			require.NoError(t, err)
			require.Contains(t, c.Eval.UserEmail, "people.example", "the account is the one of the case")

			var asText, asJSON strings.Builder
			slog.New(slog.NewTextHandler(&asText, nil)).Info("loaded", "config", c)
			slog.New(slog.NewJSONHandler(&asJSON, nil)).Info("loaded", "config", c)

			for name, text := range map[string]string{
				"%v": fmt.Sprintf("%v", c), "String": c.String(), "as text": asText.String(), "as JSON": asJSON.String(),
			} {
				assert.NotContains(t, text, ".user", name)
				assert.NotContains(t, text, "people.example", name)
			}
			assert.Contains(t, asText.String(), "config.Eval.UserEmail=[redacted]")
			assert.Contains(t, asJSON.String(), `"UserEmail":"[redacted]"`)
		})
	}
}

// The view comes from redact.Struct, which answers a name that is no field with
// its placeholder alone: a group with every part of the settings says that the
// view names real fields.
func TestConfig_LogValueNamesRealFields(t *testing.T) {
	c, err := config.Load(env(nil))
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
	assert.Equal(t, append(want, "hf_token_set"), shown, "every part of the settings, then whether there is a token")
	assert.Contains(t, view.String(), "hf_token_set=false", "and here there is none")
}

// The view shows every field and hides the ones that are a Secret, so a field
// whose name says that it holds a secret has to be one.
func TestConfig_AFieldNamedLikeASecretIsASecret(t *testing.T) {
	// What the store and the browser's environment take for the name of a secret.
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
					assert.Equal(t, reflect.TypeFor[config.Secret](), field.Type, "%s.%s is shown as it is", path, field.Name)
				}
				check(field.Type, path+"."+field.Name)
			}
		}
	}

	check(reflect.TypeFor[config.Config](), "Config")

	assert.Equal(t, 2, found, "the token and the password")
}

func TestEnvExampleDocumentsEveryKey(t *testing.T) {
	const path = "../../../../.env.example"
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	example := string(raw)

	// Load asks for each key it knows; collect them.
	var asked []string
	_, err = config.Load(func(key string) (string, bool) {
		asked = append(asked, key)
		return "", false
	})
	require.NoError(t, err)
	require.NotEmpty(t, asked)

	for _, key := range asked {
		documented := strings.Contains(example, "\n"+key+"=") || strings.Contains(example, "\n# "+key+"=")
		assert.True(t, documented, "%s is read by the evaluator but not documented in .env.example", key)
	}

	t.Run("the example loads, with the web keys it shares", func(t *testing.T) {
		values, err := godotenv.Read(path) // the file alone, whatever the environment says
		require.NoError(t, err)

		c, err := config.Load(func(key string) (string, bool) { v, ok := values[key]; return v, ok })

		require.NoError(t, err)
		assert.Equal(t, []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}, c.MCP.Command, "the quotes are removed")
		assert.Equal(t, domain.DefaultThresholds(), c.Thresholds, "the example states the defaults")
		assert.Equal(t, 10*time.Minute, c.Eval.TaskTimeout)
	})
}

// The probe prompt is typed into the composer by keystrokes, where a line break
// is a send; and a value that is not a real number would turn a check off
// without a word, because every comparison with it is false.
func TestLoad_ValuesThatWouldQuietlyBreakARun(t *testing.T) {
	t.Run("the probe prompt is one line", func(t *testing.T) {
		for name, prompt := range map[string]string{
			"a line break":      "first line\nsecond line",
			"a carriage return": "first\rsecond",
			"a tab":             "first\tsecond",
		} {
			_, err := config.Load(env(map[string]string{"EVAL_PROBE_PROMPT": prompt}))

			require.Error(t, err, name)
			assert.Contains(t, err.Error(), "EVAL_PROBE_PROMPT", name)
			assert.Contains(t, err.Error(), "one line", name)
		}
		c, err := config.Load(env(map[string]string{"EVAL_PROBE_PROMPT": "Tell me a long story about a lighthouse."}))
		require.NoError(t, err)
		assert.Equal(t, "Tell me a long story about a lighthouse.", c.Eval.ProbePrompt)
	})

	t.Run("a threshold is a real number", func(t *testing.T) {
		for _, value := range []string{"NaN", "nan", "Inf", "+Inf", "inf", "Infinity"} {
			_, err := config.Load(env(map[string]string{"EVAL_UI_OVERHEAD_P95_MS": value}))

			require.Error(t, err, value)
			assert.Contains(t, err.Error(), "EVAL_UI_OVERHEAD_P95_MS", value)
		}
		c, err := config.Load(env(map[string]string{"EVAL_UI_OVERHEAD_P95_MS": "250.5"}))
		require.NoError(t, err)
		assert.InDelta(t, 250.5, c.Thresholds.OverheadP95Ms, 0.0001)
	})

	// The Hugging Face token goes to these hosts as a bearer token. Over http it
	// would cross the network in the clear.
	t.Run("the dataset hosts are reached over https, unless they are on this computer", func(t *testing.T) {
		for _, key := range []string{"HF_HUB_URL", "HF_DATASETS_SERVER_URL"} {
			for _, bad := range []string{"http://huggingface.co", "http://192.168.1.20:8080", "http://hub.example"} {
				_, err := config.Load(env(map[string]string{key: bad}))

				require.Error(t, err, key+"="+bad)
				assert.Contains(t, err.Error(), key)
				assert.Contains(t, err.Error(), "https")
				assert.NotContains(t, err.Error(), "192.168.1.20", "the address is not echoed")
			}
			for _, fine := range []string{"https://hf-mirror.example", "http://127.0.0.1:8080", "http://localhost:9000", "http://[::1]:7000"} {
				_, err := config.Load(env(map[string]string{key: fine}))

				assert.NoError(t, err, key+"="+fine)
			}
		}
	})
}
