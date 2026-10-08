package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
)

// The server reads one settings file: the one --env names, or .env of the
// directory it was started in. A key that the named file lacks or leaves empty
// stays unset, and is not taken from the .env beside the server.
func TestRun_ReadsTheFileItIsTold(t *testing.T) {
	// A server that has its settings goes on to listen. This port is taken, so it
	// stops there, before it reaches a database, and says which address it had.
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = taken.Close() })
	settings := func(appName string) string {
		return "APP_HTTP_ADDR=" + taken.Addr().String() + "\n" +
			"DB_URL=postgres://u:p@127.0.0.1:1/db\n" + // nothing listens there
			"AGENT_BASE_URL=http://127.0.0.1:1/api\n" +
			"AGENT_APP_NAME=" + appName + "\n"
	}
	const noAppName = "missing required configuration: AGENT_APP_NAME"
	listening := "listen on " + taken.Addr().String()

	tests := []struct {
		name   string
		beside string // .env of the working directory
		named  string // the file --env names; none when empty
		want   string
		not    string
	}{
		{"a key the named file leaves empty is not taken from the .env beside it", settings("ui_agent"), settings(""), noAppName, listening},
		{"what the named file says is what the server gets", settings(""), settings("ui_agent"), listening, noAppName},
		{"with no file named, it is the .env beside it", settings("ui_agent"), "", listening, noAppName},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			require.NoError(t, os.WriteFile(".env", []byte(tc.beside), 0o600))
			var args []string
			if tc.named != "" {
				named := filepath.Join(t.TempDir(), "other.env")
				require.NoError(t, os.WriteFile(named, []byte(tc.named), 0o600))
				args = []string{"--env", named}
			}
			// A shell may export any setting of the server. This one stops a server
			// with the default APP_BASE_URL before it listens.
			t.Setenv("APP_ENV", "production")
			settingsOfTheShellAside(t)
			var stderr strings.Builder

			status := command(args, &stderr)

			assert.Equal(t, 1, status)
			assert.Contains(t, stderr.String(), tc.want)
			assert.NotContains(t, stderr.String(), tc.not)
		})
	}
}

// settingsOfTheShellAside empties, for the length of a test, every variable
// that the loader of the settings asks for. An empty value counts as unset, so
// a test that reads the settings as the server does gets the file it names
// and the defaults, whatever the shell it runs in exports.
func settingsOfTheShellAside(t *testing.T) {
	t.Helper()
	var asked []string
	_, _ = config.Load(func(key string) (string, bool) {
		asked = append(asked, key)
		return "", false
	})
	require.NotEmpty(t, asked, "the loader asked for its settings")
	for _, key := range asked {
		t.Setenv(key, "")
	}
}

// The health check asks the address that the server listens on, so it reads
// the settings file the server reads.
func TestCommand_TheHealthCheckReadsTheFileItIsTold(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/readyz", r.URL.Path)
	}))
	t.Cleanup(ready.Close)
	gone, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, gone.Close()) // nothing listens there now
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(".env", []byte("APP_HTTP_ADDR="+gone.Addr().String()+"\n"), 0o600))
	named := filepath.Join(t.TempDir(), "other.env")
	require.NoError(t, os.WriteFile(named, []byte("APP_HTTP_ADDR="+ready.Listener.Addr().String()+"\n"), 0o600))
	t.Setenv("APP_HTTP_ADDR", "") // an empty value counts as unset

	assert.Zero(t, command([]string{"--env", named, "healthcheck"}, io.Discard), "the server of the named file is ready")
	assert.Equal(t, 1, command([]string{"healthcheck"}, io.Discard), "and the one of the .env beside it is not there")
}

func TestArguments(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		envFile string
		words   []string
		wantErr string
	}{
		{name: "nothing", envFile: ".env"},
		{name: "a settings file", args: []string{"--env", "other.env"}, envFile: "other.env"},
		{name: "with one dash", args: []string{"-env", "other.env"}, envFile: "other.env"},
		{name: "with an equals sign", args: []string{"--env=other.env"}, envFile: "other.env"},
		{name: "the health check", args: []string{"healthcheck"}, envFile: ".env", words: []string{"healthcheck"}},
		{
			name: "the health check of an address", args: []string{"healthcheck", "http://127.0.0.1:1/readyz"},
			envFile: ".env", words: []string{"healthcheck", "http://127.0.0.1:1/readyz"},
		},
		{
			name: "the health check with a settings file", args: []string{"--env", "other.env", "healthcheck"},
			envFile: "other.env", words: []string{"healthcheck"},
		},
		{name: "a flag it does not have", args: []string{"--port", "8300"}, wantErr: "flag provided but not defined: -port"},
		{name: "a file name left out", args: []string{"--env"}, wantErr: "flag needs an argument: -env"},
		{name: "a word it does not know", args: []string{"serve"}, wantErr: `unknown argument "serve"`},
		{name: "a file name without its flag", args: []string{"other.env"}, wantErr: `unknown argument "other.env"`},
		{name: "a flag after the health check", args: []string{"healthcheck", "--env", "other.env"}, wantErr: "the flags come before healthcheck"},
		{name: "two addresses to check", args: []string{"healthcheck", "http://127.0.0.1:1/readyz", "http://127.0.0.1:2/readyz"}, wantErr: "healthcheck takes one address"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			envFile, words, err := arguments(tc.args)

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.envFile, envFile)
			assert.Equal(t, strings.Join(tc.words, " "), strings.Join(words, " "))
		})
	}
}

// A command line that is wrong starts nothing: it says what is wrong and how
// the program is called, and the exit status is 2. A file that --env names and
// that is not there is such a mistake, the word after --env that was meant for
// the health check included: the settings would be the environment's alone.
func TestCommand_AWrongCommandLineStartsNothing(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, key := range []string{"APP_HTTP_ADDR", "DB_URL", "AGENT_BASE_URL", "AGENT_APP_NAME"} {
		t.Setenv(key, "") // an empty value counts as unset
	}
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"--port", "8300"}, "flag provided but not defined: -port"},
		{[]string{"serve"}, `unknown argument "serve"`},
		{[]string{"--env"}, "flag needs an argument: -env"},
		{[]string{"--env", "nothing.env"}, `no settings file "nothing.env"`},
		{[]string{"--env", "healthcheck"}, `no settings file "healthcheck"`},
		{[]string{"--env", ""}, `no settings file ""`},
		{[]string{"--env", "nothing.env", "healthcheck"}, `no settings file "nothing.env"`},
		{[]string{"healthcheck", "--env", "nothing.env"}, "the flags come before healthcheck"},
		{[]string{"healthcheck", "http://127.0.0.1:1/readyz", "http://127.0.0.1:2/readyz"}, "healthcheck takes one address"},
	}
	for _, tc := range tests {
		var stderr strings.Builder

		status := command(tc.args, &stderr)

		assert.Equal(t, 2, status, "%v", tc.args)
		assert.Contains(t, stderr.String(), "agent-web-ui: "+tc.want, "%v", tc.args)
		assert.Contains(t, stderr.String(), "usage: agent-web-ui [--env FILE] [healthcheck [URL]]", "%v", tc.args)
	}

	// In a container the settings are in the environment, and there is no .env.
	t.Run("but .env may be missing", func(t *testing.T) {
		for _, args := range [][]string{nil, {"--env", ".env"}} {
			var stderr strings.Builder

			status := command(args, &stderr)

			assert.Equal(t, 1, status, "%v", args)
			assert.Contains(t, stderr.String(), "missing required configuration", "the settings of the environment were read: %v", args)
		}
	})

	t.Run("and asking for help is no mistake", func(t *testing.T) {
		var stderr strings.Builder

		status := command([]string{"--help"}, &stderr)

		assert.Zero(t, status)
		assert.Equal(t, "usage: agent-web-ui [--env FILE] [healthcheck [URL]]\n", stderr.String())
	})
}
