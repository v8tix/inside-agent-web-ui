// Package postgres is the PostgreSQL driven adapter: connection pool,
// migrations and the stores that implement the core's resource ports.
package postgres

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/v8tix/inside-agent-web-ui/internal/infrastructure/config"
)

const (
	connectTimeout  = 5 * time.Second
	pingTimeout     = 5 * time.Second
	maxConnLifetime = time.Hour
)

// NewPool opens a pgx connection pool and checks that the database answers.
// No error holds the connection string as a whole. The reason for a string the
// driver cannot read may name the piece it stumbled on (see parseReason), and
// the driver's error for a connection that fails names the user, the host and
// the database as the driver read them. They are no pieces of the password when
// the driver ends the user and the password where net/url does: config.Load
// takes no other URL, and a DB that was put together by hand may hold one.
func NewPool(ctx context.Context, cfg config.DB) (*pgxpool.Pool, error) {
	if cfg.MaxConns < 1 {
		return nil, fmt.Errorf("pool size must be at least 1 (got %d)", cfg.MaxConns)
	}
	pc, err := pgxpool.ParseConfig(cfg.URL.Reveal())
	if err != nil {
		return nil, fmt.Errorf("parsing DB_URL: %s", parseReason(err))
	}
	pc.MaxConns = int32(min(cfg.MaxConns, math.MaxInt32))
	pc.MaxConnLifetime = maxConnLifetime
	pc.ConnConfig.ConnectTimeout = connectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("opening database pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	return pool, nil
}

// parseReason is why the driver cannot read a connection string, without the
// string. The driver's error carries the string twice: its text quotes it before
// the reason, between backquotes and with the password masked where the driver
// can tell where it is, and its ConnString field holds it as it was given. So
// NewPool does not wrap that error, and of its text only what follows the last
// "`: " is kept. A reason can name the piece it stumbled on; a text of another
// shape gives no reason.
func parseReason(err error) string {
	const mark = "`: " // ends the quoted string and starts the reason
	_, reason, found := strings.CutLast(err.Error(), mark)
	if !found {
		return "the driver gave no reason"
	}
	return reason
}
