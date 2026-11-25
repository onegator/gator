// Package storetest gives a test package a database of its own. Packages whose tests run the
// outbox's consumers (relays, conversation cards, agents) follow every event in the database they
// are pointed at; on the database every other package tests against, they would act on that
// package's events, and lock its rows, from under its tests.
package storetest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Main runs m on a fresh database made from GATOR_DATABASE_URL, which it points at that database
// for the run, and drops it afterwards. Without GATOR_DATABASE_URL it just runs m.
func Main(m *testing.M, prefix string) {
	base := os.Getenv("GATOR_DATABASE_URL")
	if base == "" {
		os.Exit(m.Run())
	}
	name := fmt.Sprintf("gator_%s_%d", prefix, time.Now().UnixNano())
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		fmt.Fprintln(os.Stderr, prefix, "database:", err)
		os.Exit(1)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		fmt.Fprintln(os.Stderr, prefix, "database:", err)
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
