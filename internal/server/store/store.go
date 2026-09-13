// Package store owns the PostgreSQL connection pool and schema migrations.
package store

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
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

// Migrate applies embedded migrations. It is invoked explicitly by `gator-server migrate`,
// never automatically at startup.
func Migrate(ctx context.Context, url string) error {
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
	return goose.UpContext(ctx, db, "migrations")
}

// MigrateDown rolls back the most recent migration.
func MigrateDown(ctx context.Context, url string) error {
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
