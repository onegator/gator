package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/onegator/gator/internal/server/store/db"
)

// A transaction the database broke a deadlock with is run again; any other failure is not.
func TestADeadlockedTransactionRunsAgain(t *testing.T) {
	_, pool := scratch(t)
	runs := 0
	err := InTxRetry(context.Background(), pool, func(*db.Queries) error {
		runs++
		if runs < 3 {
			return &pgconn.PgError{Code: "40P01"}
		}
		return nil
	})
	if err != nil || runs != 3 {
		t.Fatalf("runs %d: %v", runs, err)
	}
	runs = 0
	if err := InTxRetry(context.Background(), pool, func(*db.Queries) error { runs++; return &pgconn.PgError{Code: "23505"} }); err == nil || runs != 1 {
		t.Fatalf("another error is not retried: %d %v", runs, err)
	}
}
