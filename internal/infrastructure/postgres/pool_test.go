package postgres_test

import (
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/postgres"
)

const secret = "topsecretpw"

// The driver takes DB_URL apart by the rule of libpq: the user and the password
// end at the first @ before the first slash. Load and the start line read it
// with net/url, which ends them at the last @ before the first slash, question
// mark or hash sign. Where the two differ, the driver takes a piece of the
// password for a host, a database or a parameter, and names it in its errors.
// So Load refuses such a URL, and what it takes the driver reads as net/url
// does.
func TestDBURL_TheDriverReadsWhatLoadTakesAsLoadDoes(t *testing.T) {
	// The driver fills what a URL leaves out from the PG variables of the shell,
	// and an empty one counts as unset.
	for _, variable := range os.Environ() {
		if name, _, _ := strings.Cut(variable, "="); strings.HasPrefix(name, "PG") {
			t.Setenv(name, "")
		}
	}
	tests := []struct {
		name  string
		dbURL string
		taken bool
	}{
		{"a password of letters and digits", "postgres://alice:" + secret + "@db.internal:5432/chats?sslmode=disable", true},
		{"a password with its signs encoded", "postgres://alice:P%40ss%2Fw0rd%3Fcanary%231@db.internal:5432/chats?sslmode=disable", true},
		{"a password with the signs a URL takes as they are", "postgres://alice:a:b-c.d_e~f!g$h&i'j(k)l*m+n,o;p=q@db.internal/chats?sslmode=disable", true},
		{"an @ in the password", "postgres://agentwebui:p@ss@postgres:5432/agentwebui?sslmode=disable", false},
		{"an @ and a slash", "postgres://app:P@ss/w0rd-canary-1@localhost:5440/agentwebui", false},
		{"an @ and a question mark", "postgres://app:P@ss?word@db:5432/app", false},
		{"a hash sign after digits, which pass for a port", "postgres://app:2024#word@db:5432/app?sslmode=disable", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			settings := map[string]string{"DB_URL": tc.dbURL, "AGENT_BASE_URL": "http://agent.internal/api", "AGENT_APP_NAME": "ui_agent"}

			cfg, err := config.Load(func(key string) (string, bool) { value, ok := settings[key]; return value, ok })

			assert.Equal(t, tc.taken, err == nil, "what Load says: %v", err)
			if err != nil {
				return
			}
			asLoad, err := url.Parse(cfg.DB.URL.Reveal())
			require.NoError(t, err)
			password, _ := asLoad.User.Password()
			asDriver, err := pgxpool.ParseConfig(cfg.DB.URL.Reveal())
			require.NoError(t, err)
			assert.Equal(t, asLoad.User.Username(), asDriver.ConnConfig.User)
			assert.True(t, password == asDriver.ConnConfig.Password, "the driver ends the password where net/url ends it")
			assert.Equal(t, asLoad.Hostname(), asDriver.ConnConfig.Host)
			assert.Equal(t, strings.TrimPrefix(asLoad.Path, "/"), asDriver.ConnConfig.Database)
		})
	}
}

func TestNewPool_Errors(t *testing.T) {
	tests := []struct {
		name      string
		db        config.DB
		wantInErr string
		notInErr  []string // besides the password, which no error may hold
	}{
		{
			name:      "a URL the driver cannot read is reported with the driver's reason and without the URL",
			db:        config.DB{URL: "postgres://alice:" + secret + "@db.internal:notaport/chats?sslmode=disable", MaxConns: 5},
			wantInErr: "DB_URL: invalid port",
			notInErr:  []string{"postgres://", "alice", "db.internal", "chats", "sslmode"},
		},
		{
			name: "a connection string that holds the mark the reason follows is left out whole",
			db: config.DB{
				URL:      "application_name='a`: b' user=alice password=" + secret + " host=db.internal port=notaport dbname=chats",
				MaxConns: 5,
			},
			wantInErr: "DB_URL: invalid port",
			notInErr:  []string{"application_name", "alice", "db.internal", "chats"},
		},
		{
			name:      "an unreachable server fails fast without the password",
			db:        config.DB{URL: "postgres://user:" + secret + "@127.0.0.1:1/db", MaxConns: 5},
			wantInErr: "database",
		},
		{
			name:      "a pool size below one is refused",
			db:        config.DB{URL: "postgres://user:" + secret + "@127.0.0.1:1/db", MaxConns: 0},
			wantInErr: "pool size",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := postgres.NewPool(t.Context(), tc.db)

			require.Error(t, err)
			assert.Nil(t, pool)
			assert.Contains(t, err.Error(), tc.wantInErr)
			assert.NotContains(t, err.Error(), secret)
			for _, part := range tc.notInErr {
				assert.NotContains(t, err.Error(), part)
			}
		})
	}
}
