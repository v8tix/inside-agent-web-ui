package devtools

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/v8tix/inside-agent-web-ui/internal/eval/infrastructure/config"
)

// BuildCommand returns the process that runs Chrome DevTools MCP, or says why it
// must not be run: the version is not pinned, or one of the flags that keep it
// from sending usage statistics and from touching a real browser profile is
// missing (see config.CheckMCP).
//
// The process gets a trimmed environment (see ChildEnv), and it is killed, along
// with everything it started, when ctx ends.
//
// Pattern: Pinned, Trimmed, Isolated Child
func BuildCommand(ctx context.Context, command, args, environ []string) (*exec.Cmd, error) {
	if err := config.CheckMCP(command, args); err != nil {
		return nil, err
	}
	argv := append(slices.Clone(command), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = ChildEnv(environ)
	// The server is a package fetched from a registry, so it does not run in the
	// project directory, where the settings file with the tokens is.
	cmd.Dir = os.TempDir()
	cmd.Cancel = func() error { killTree(cmd); return nil }
	isolate(cmd)
	return cmd, nil
}

// childEnvNames are the variables the process needs to find Node and Chrome, to
// reach the network through a proxy, and to write temporary files.
var childEnvNames = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"TMPDIR": true, "TMP": true, "TEMP": true, "TERM": true, "TZ": true,
	"LANG": true, "LANGUAGE": true,
	"DISPLAY": true, "WAYLAND_DISPLAY": true, "XAUTHORITY": true, "DBUS_SESSION_BUS_ADDRESS": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"SYSTEMROOT": true, "WINDIR": true, "COMSPEC": true, "PATHEXT": true,
	"APPDATA": true, "LOCALAPPDATA": true, "USERPROFILE": true, "PROGRAMFILES": true,
}

var childEnvPrefixes = []string{"LC_", "XDG_", "NODE_", "NPM_", "NVM_", "FNM_", "VOLTA_", "ASDF_", "PNPM_", "CHROME_", "PUPPETEER_"}

// secretName matches variable names that hold secrets, which never pass.
var secretName = regexp.MustCompile(`(?i)(password|passwd|secret|token|credential|api[_-]?key|private[_-]?key|cookie)`)

// ChildEnv keeps, from an environment in the form os.Environ gives, what
// Chrome DevTools MCP needs. The server is a package fetched from a registry and
// it drives a browser; it has no use for the Hugging Face token, the Web UI
// password or the database settings the evaluator's own environment holds, so it
// does not get them. A name that looks like a secret is dropped even when it
// would otherwise pass.
func ChildEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" || secretName.MatchString(name) {
			continue
		}
		upper := strings.ToUpper(name)
		if childEnvNames[upper] || slices.ContainsFunc(childEnvPrefixes, func(p string) bool { return strings.HasPrefix(upper, p) }) {
			out = append(out, kv)
		}
	}
	return out
}

// tailBuffer keeps the last bytes written to it: the end of what the server
// printed, which is where its complaint is when it fails to start.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if extra := len(t.buf) - t.max; extra > 0 {
		t.buf = t.buf[extra:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
