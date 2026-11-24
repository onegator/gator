package e2e

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestMain gives the package a database of its own. Its servers follow the outbox like a real
// one does — agents answer mentions, cards follow work — and on the database every other package
// tests against, they would answer that package's mentions too, from under its tests.
func TestMain(m *testing.M) {
	base := os.Getenv("GATOR_DATABASE_URL")
	if base == "" {
		os.Exit(m.Run())
	}
	name := fmt.Sprintf("gator_e2e_%d", time.Now().UnixNano())
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e database:", err)
		os.Exit(1)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		fmt.Fprintln(os.Stderr, "e2e database:", err)
		os.Exit(1)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	_ = os.Setenv("GATOR_DATABASE_URL", u.String())
	code := m.Run()
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	admin.Close(ctx)
	os.Exit(code)
}
