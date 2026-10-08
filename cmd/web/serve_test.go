package main

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
)

// Starting means recovering the replies a crash left streaming, which marks every
// reply still streaming as interrupted. A second process started by mistake
// (Compose next to scripts/run.sh) would do that to the first one's live replies
// and only then find its port taken. So the port comes first, and a process that
// cannot have it never reaches the database.
func TestServe_TakesThePortBeforeTouchingTheDatabase(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = taken.Close() })
	env := map[string]string{
		"APP_HTTP_ADDR":  taken.Addr().String(),
		"DB_URL":         "postgres://u:p@127.0.0.1:1/db", // nothing listens there
		"AGENT_BASE_URL": "http://127.0.0.1:1/api",
		"AGENT_APP_NAME": "ui_agent",
	}
	cfg, err := config.Load(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	require.NoError(t, err)
	started := time.Now()

	err = serve(t.Context(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "listen on "+taken.Addr().String(), "it is the port that is the problem, not the database")
	assert.Less(t, time.Since(started), 2*time.Second, "and it fails at once")
}
