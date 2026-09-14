package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// scratchDatabase creates a throwaway database on the test server and returns its URL.
// Restore runs with --clean, so it must never touch the database other test packages use
// concurrently.
func scratchDatabase(t *testing.T, base string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name := "gator_backup_test_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// TestDumpAndRestoreRoundTrip proves a dump can be restored: it creates a marker row,
// dumps, deletes the row, restores, and expects the row back.
func TestDumpAndRestoreRoundTrip(t *testing.T) {
	base := os.Getenv("GATOR_DATABASE_URL")
	if base == "" {
		t.Skip("GATOR_DATABASE_URL not set")
	}
	for _, bin := range []string{"pg_dump", "pg_restore", "psql"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not on PATH")
		}
	}
	dbURL := scratchDatabase(t, base)
	ctx := context.Background()
	psql := func(sql string) string {
		out, err := exec.CommandContext(ctx, "psql", "-X", "-t", "-A", dbURL, "-c", sql).CombinedOutput()
		if err != nil {
			t.Fatalf("psql %q: %v\n%s", sql, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	psql("CREATE TABLE backup_marker (v text)")
	psql("INSERT INTO backup_marker VALUES ('present')")
	path, err := Dump(ctx, dbURL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st == nil || st.Size() == 0 {
		t.Fatal("empty dump")
	}
	psql("DELETE FROM backup_marker")
	if err := Restore(ctx, dbURL, path); err != nil {
		t.Fatal(err)
	}
	if got := psql("SELECT count(*) FROM backup_marker WHERE v='present'"); got != "1" {
		t.Fatalf("after restore marker count = %s", got)
	}
}

// TestFailedRestoreChangesNothing proves the restore is atomic: a dump whose SQL fails
// partway must leave the target database exactly as it was.
func TestFailedRestoreChangesNothing(t *testing.T) {
	base := os.Getenv("GATOR_DATABASE_URL")
	if base == "" {
		t.Skip("GATOR_DATABASE_URL not set")
	}
	for _, bin := range []string{"pg_dump", "pg_restore", "psql"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not on PATH")
		}
	}
	ctx := context.Background()
	src := scratchDatabase(t, base)
	dst := scratchDatabase(t, base)
	run := func(db, sql string) string {
		out, err := exec.CommandContext(ctx, "psql", "-X", "-t", "-A", db, "-c", sql).CombinedOutput()
		if err != nil {
			t.Fatalf("psql %q: %v\n%s", sql, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// Source dump: a table plus a view that depends on a function the target will lack,
	// so applying it fails after the table has already been dropped and recreated.
	run(src, "CREATE TABLE keep (v text); INSERT INTO keep VALUES ('from-dump')")
	run(src, "CREATE FUNCTION f() RETURNS int LANGUAGE sql AS 'SELECT 1'; CREATE VIEW needs_f AS SELECT f()")
	path, err := Dump(ctx, src, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Target: same table with different data, and an object that makes the restore fail
	// (a non-table relation named like the dumped function's view blocks CREATE VIEW).
	run(dst, "CREATE TABLE keep (v text); INSERT INTO keep VALUES ('original')")
	run(dst, "CREATE TABLE needs_f (x int)")
	if err := Restore(ctx, dst, path); err == nil {
		t.Fatal("restore into a conflicting database should fail")
	}
	if got := run(dst, "SELECT string_agg(v, ',') FROM keep"); got != "original" {
		t.Fatalf("failed restore changed data: keep = %q", got)
	}
}
