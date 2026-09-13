// Package store owns the PostgreSQL connection pool and schema migrations.
package store

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store wraps the pool. Generated sqlc code lives in this package's subpackage later.
type Store struct {
	Pool *pgxpool.Pool
}

// Open connects to PostgreSQL and verifies the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.Pool.Close() }

// Ready reports whether the database answers.
func (s *Store) Ready(ctx context.Context) error { return s.Pool.Ping(ctx) }

// migrationLock serialises migrators across processes (and parallel test packages).
const migrationLock = 7345001

// Migrate applies embedded migrations. It is invoked explicitly by `gator-server migrate`,
// never automatically at startup.
func Migrate(ctx context.Context, url string) error {
	unlock, err := lock(ctx, url)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return err
	}
	db := stdlib.OpenDB(*cfg.ConnConfig)
	defer db.Close()
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		return err
	}
	return migrateRiver(ctx, url, rivermigrate.DirectionUp)
}

// migrateRiver applies the queue's own schema (river_job and friends).
func migrateRiver(ctx context.Context, url string, dir rivermigrate.Direction) error {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	m, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return err
	}
	_, err = m.Migrate(ctx, dir, nil)
	return err
}

// lock takes a session-level advisory lock on a dedicated connection.
func lock(ctx context.Context, url string) (func(), error) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		conn.Close(ctx)
		return nil, err
	}
	return func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLock)
		conn.Close(context.Background())
	}, nil
}

// MigrateDown rolls back the most recent migration.
func MigrateDown(ctx context.Context, url string) error {
	unlock, err := lock(ctx, url)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return err
	}
	db := stdlib.OpenDB(*cfg.ConnConfig)
	defer db.Close()
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.DownContext(ctx, db, "migrations")
}
