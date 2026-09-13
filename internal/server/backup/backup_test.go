package backup

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestDumpAndRestoreRoundTrip proves a dump can be restored: it creates a marker row,
// dumps, deletes the row, restores, and expects the row back.
func TestDumpAndRestoreRoundTrip(t *testing.T) {
	url := os.Getenv("GATOR_DATABASE_URL")
	if url == "" {
		t.Skip("GATOR_DATABASE_URL not set")
	}
	if _, err := exec.LookPath("pg_dump"); err != nil {
		t.Skip("pg_dump not on PATH")
	}
	ctx := context.Background()
	psql := func(sql string) string {
		out, err := exec.CommandContext(ctx, "psql", "-X", "-t", "-A", url, "-c", sql).CombinedOutput()
		if err != nil {
			t.Fatalf("psql %q: %v\n%s", sql, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	psql("DROP TABLE IF EXISTS backup_marker")
	psql("CREATE TABLE backup_marker (v text)")
	psql("INSERT INTO backup_marker VALUES ('present')")
	dir := t.TempDir()
	path, err := Dump(ctx, url, dir)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st == nil || st.Size() == 0 {
		t.Fatal("empty dump")
	}
	psql("DELETE FROM backup_marker")
	if err := Restore(ctx, url, path); err != nil {
		t.Fatal(err)
	}
	if got := psql("SELECT count(*) FROM backup_marker WHERE v='present'"); got != "1" {
		t.Fatalf("after restore marker count = %s", got)
	}
	psql("DROP TABLE backup_marker")
}
