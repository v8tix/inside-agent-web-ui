// Package pgenv gives integration tests a real PostgreSQL through
// Testcontainers: one container per test binary, one fresh database per test.
//
// It needs Docker. The container is removed by the Testcontainers reaper when
// the test process exits.
package pgenv

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Image is the pinned PostgreSQL image. Keep it in step with docker-compose.yml.
const Image = "postgres:18.6"

var (
	once     sync.Once
	adminURL string
	startErr error
)

func start() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ctr, err := postgres.Run(ctx, Image,
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		startErr = err
		return
	}
	adminURL, startErr = ctr.ConnectionString(ctx, "sslmode=disable")
}

// NewDatabase creates an empty database in the shared container and returns its
// connection URL. The database is dropped when the test ends.
func NewDatabase(t testing.TB) string {
	t.Helper()
	once.Do(start)
	if startErr != nil {
		t.Fatalf("starting the PostgreSQL test container (is Docker running?): %v", startErr)
	}

	name := "t_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connecting to the test container: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating test database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		admin, err := pgx.Connect(cleanupCtx, adminURL)
		if err != nil {
			return
		}
		defer func() { _ = admin.Close(cleanupCtx) }()
		_, _ = admin.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parsing the test container URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// WithPassword returns dbURL with a different password, for failure tests.
func WithPassword(dbURL, password string) string {
	u, err := url.Parse(dbURL)
	if err != nil {
		panic(fmt.Sprintf("pgenv: bad URL: %v", err))
	}
	u.User = url.UserPassword(u.User.Username(), password)
	return u.String()
}
