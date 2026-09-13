package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store"
)

func testPool(t *testing.T) *pgxpool.Pool {
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
	return pool
}

func newUser(t *testing.T, pool *pgxpool.Pool, role string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), "INSERT INTO users(email,name,workspace_role) VALUES($1,'t',$2) RETURNING id",
		fmt.Sprintf("auth-%d@t.local", time.Now().UnixNano()), role).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestIssueVerifyRevoke(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tk := Tokens{Pool: pool}
	uid := newUser(t, pool, "member")

	plain, rec, err := tk.Issue(ctx, IssueParams{Kind: KindUser, Scope: "user", Name: "cli", UserID: uid})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) < 40 || plain[:9] != "gtr_user_" {
		t.Fatalf("unexpected token shape %q", plain)
	}
	var stored []byte
	_ = pool.QueryRow(ctx, "SELECT hash FROM api_tokens WHERE id=$1", rec.ID).Scan(&stored)
	if string(stored) == plain {
		t.Fatal("plaintext must not be stored")
	}

	p, err := tk.Verify(ctx, plain)
	if err != nil || p.Kind != KindUser || p.UserID.Bytes != uid.Bytes || p.WorkspaceRole != "member" {
		t.Fatalf("verify: %+v %v", p, err)
	}
	if _, err := tk.Verify(ctx, plain+"x"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("tampered token: %v", err)
	}
	if _, err := tk.Verify(ctx, "not-a-token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("garbage: %v", err)
	}
	if err := tk.Revoke(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tk.Verify(ctx, plain); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token still valid: %v", err)
	}
}

func TestExpiredTokenIsInvalid(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tk := Tokens{Pool: pool}
	plain, rec, err := tk.Issue(ctx, IssueParams{Kind: KindRunner, Scope: "runner:x", Name: "x", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tk.Verify(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE api_tokens SET expires_at = now() - interval '1 minute' WHERE id=$1", rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tk.Verify(ctx, plain); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token: %v", err)
	}
}

func TestProjectRoles(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	az := Authorizer{Pool: pool}
	var project pgtype.UUID
	if err := pool.QueryRow(ctx, "INSERT INTO projects(slug,name) VALUES($1,'p') RETURNING id", fmt.Sprintf("rb-%d", time.Now().UnixNano())).Scan(&project); err != nil {
		t.Fatal(err)
	}
	admin := Principal{Kind: KindUser, UserID: newUser(t, pool, "admin"), WorkspaceRole: "admin"}
	viewer := Principal{Kind: KindUser, UserID: newUser(t, pool, "member")}
	stranger := Principal{Kind: KindUser, UserID: newUser(t, pool, "member")}
	if _, err := pool.Exec(ctx, "INSERT INTO memberships VALUES($1,$2,'viewer')", project, viewer.UserID); err != nil {
		t.Fatal(err)
	}
	other := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	cases := []struct {
		name string
		p    Principal
		proj pgtype.UUID
		want Role
	}{
		{"workspace admin everywhere", admin, project, RoleAdmin},
		{"viewer membership", viewer, project, RoleViewer},
		{"stranger", stranger, project, RoleNone},
		{"runner is member", Principal{Kind: KindRunner}, project, RoleMember},
		{"plugin own project", Principal{Kind: KindPlugin, ProjectID: project}, project, RoleMember},
		{"plugin other project", Principal{Kind: KindPlugin, ProjectID: project}, other, RoleNone},
	}
	for _, c := range cases {
		got, err := az.ProjectRole(ctx, c.p, c.proj)
		if err != nil || got != c.want {
			t.Errorf("%s: got %v err %v want %v", c.name, got, err, c.want)
		}
	}
	if err := az.Require(ctx, viewer, project, RoleMember); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer as member: %v", err)
	}
}
