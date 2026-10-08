// Integration tests for the PostgreSQL adapter. Group A: needs Docker.
package postgres_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/postgres"
	"github.com/v8tix/inside-agent-web-ui/internal/platform/envcfg"
	"github.com/v8tix/inside-agent-web-ui/it/internal/pgenv"
)

func TestPoolSuite(t *testing.T) {
	dbURL := pgenv.NewDatabase(t)
	const wrongPassword = "wrong-password-xyz"

	tests := []struct {
		name      string
		db        config.DB
		wantErr   bool
		notInErr  string
		wantConns int32
	}{
		{
			name:      "connects and answers a query",
			db:        config.DB{URL: envcfg.Secret(dbURL), MaxConns: 3},
			wantConns: 3,
		},
		{
			name:     "a wrong password is rejected without echoing it",
			db:       config.DB{URL: envcfg.Secret(pgenv.WithPassword(dbURL, wrongPassword)), MaxConns: 3},
			wantErr:  true,
			notInErr: wrongPassword,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool, err := postgres.NewPool(t.Context(), tc.db)

			if tc.wantErr {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), tc.notInErr)
				return
			}
			require.NoError(t, err)
			defer pool.Close()

			var got int
			require.NoError(t, pool.QueryRow(t.Context(), "SELECT 1").Scan(&got))
			assert.Equal(t, 1, got)
			assert.Equal(t, tc.wantConns, pool.Config().MaxConns, "DB_MAX_CONNS must reach the pool")
			assert.Equal(t, time.Hour, pool.Config().MaxConnLifetime, "a connection is replaced after an hour")
			assert.Equal(t, 5*time.Second, pool.Config().ConnConfig.ConnectTimeout, "a connection that cannot be made is given up after five seconds")
		})
	}
}
