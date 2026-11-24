package conversations

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// fixture is a real database with the package's services on it. Tests that need one skip
// without GATOR_DATABASE_URL, like the rest of the repository's.
type fixture struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgxpool.Pool
	access    *Access
	directory *Directory
}

var serial atomic.Int64

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("GATOR_DATABASE_URL")
	if url == "" {
		t.Skip("GATOR_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	roles := auth.Authorizer{Pool: pool}
	access := NewAccess(roles, db.New(pool))
	return &fixture{t: t, ctx: ctx, pool: pool, access: access, directory: NewDirectory(pool, access, roles)}
}

func unique64() int64 { return time.Now().UnixNano() + serial.Add(1) }

// person creates a workspace member and answers them as a caller.
func (f *fixture) person(name string) auth.Principal {
	f.t.Helper()
	var id pgtype.UUID
	if err := f.pool.QueryRow(f.ctx, "INSERT INTO users(email,name,workspace_role) VALUES($1,$2,'member') RETURNING id",
		fmt.Sprintf("%s-%d@conv.test", name, unique64()), name).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return auth.Principal{Kind: auth.KindUser, UserID: id, WorkspaceRole: "member", Name: name}
}

// project creates a project with these people in these roles.
func (f *fixture) project(people map[auth.Principal]string) pgtype.UUID {
	f.t.Helper()
	var id pgtype.UUID
	if err := f.pool.QueryRow(f.ctx, "INSERT INTO projects(slug,name,process_config) VALUES($1,'P','{}') RETURNING id",
		fmt.Sprintf("conv%d", unique64())).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	for p, role := range people {
		if _, err := f.pool.Exec(f.ctx, "INSERT INTO memberships(project_id,user_id,role) VALUES($1,$2,$3)", id, p.UserID, role); err != nil {
			f.t.Fatal(err)
		}
	}
	return id
}
