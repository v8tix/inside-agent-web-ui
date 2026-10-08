package postgres

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrator applies the embedded SQL migrations. golang-migrate takes a
// PostgreSQL advisory lock while it runs, so several instances can start at
// once and only one of them migrates.
type Migrator struct {
	m  *migrate.Migrate
	db *sql.DB
}

// NewMigrator prepares a Migrator that shares pool's connections. The pool
// needs room for one extra connection, which holds the advisory lock.
func NewMigrator(pool *pgxpool.Pool) (*Migrator, error) {
	source, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("reading embedded migrations: %w", err)
	}
	db := stdlib.OpenDBFromPool(pool)
	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("preparing migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("preparing migrator: %w", err)
	}
	return &Migrator{m: m, db: db}, nil
}

// Up applies every pending migration. Nothing to do is not an error.
func (m *Migrator) Up() error {
	if err := m.m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

// Down reverts every migration. It exists for tests and local resets; the
// application never calls it.
func (m *Migrator) Down() error {
	if err := m.m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("reverting migrations: %w", err)
	}
	return nil
}

// Version reports the applied version (0 when none) and whether a failed
// migration left the database dirty.
func (m *Migrator) Version() (version uint, dirty bool, err error) {
	version, dirty, err = m.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	return version, dirty, err
}

// Close releases the migrator's resources. The pool stays open.
func (m *Migrator) Close() error {
	sourceErr, dbErr := m.m.Close()
	return errors.Join(sourceErr, dbErr)
}

// Migrate applies every pending migration using pool.
func Migrate(pool *pgxpool.Pool) error {
	m, err := NewMigrator(pool)
	if err != nil {
		return err
	}
	upErr := m.Up()
	return errors.Join(upErr, m.Close())
}
