package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/store/db"
)

func TestBootstrapAdminThenOIDCLinks(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	email := fmt.Sprintf("boot-%d@t.local", time.Now().UnixNano())

	u, token, err := BootstrapAdmin(ctx, pool, email, "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if u.WorkspaceRole != "admin" || u.OidcSubject != nil || u.Name != email {
		t.Fatalf("bootstrap user: %+v", u)
	}
	p, err := Tokens{Pool: pool}.Verify(ctx, token)
	if err != nil || !p.IsWorkspaceAdmin() {
		t.Fatalf("bootstrap token: %+v %v", p, err)
	}
	// running bootstrap again is idempotent and keeps one user
	u2, _, err := BootstrapAdmin(ctx, pool, email, "Admin", time.Hour)
	if err != nil || u2.ID != u.ID || u2.Name != "Admin" {
		t.Fatalf("second bootstrap: %+v %v", u2, err)
	}

	q := db.New(pool)
	linked, err := LinkOIDCUser(ctx, q, email, "Admin", "sub-"+email)
	if err != nil {
		t.Fatalf("first OIDC login should link the bootstrap account: %v", err)
	}
	if linked.ID != u.ID || linked.OidcSubject == nil || linked.WorkspaceRole != "admin" {
		t.Fatalf("linked user: %+v", linked)
	}
	// a different identity presenting the same email is refused
	if _, err := LinkOIDCUser(ctx, q, email, "Mallory", "other-"+email); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("takeover must be refused, got %v", err)
	}
	// the same subject with a changed email updates the profile
	newEmail := "moved-" + email
	moved, err := LinkOIDCUser(ctx, q, newEmail, "Admin", "sub-"+email)
	if err != nil || moved.ID != u.ID || moved.Email != newEmail {
		t.Fatalf("email change: %+v %v", moved, err)
	}
}

func TestIssueRunnerToken(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	token, err := IssueRunnerToken(ctx, pool, "vps-test")
	if err != nil {
		t.Fatal(err)
	}
	p, err := Tokens{Pool: pool}.Verify(ctx, token)
	if err != nil || p.Kind != KindRunner || p.Scope != "runner:vps-test" {
		t.Fatalf("runner token: %+v %v", p, err)
	}
	if _, err := IssueRunnerToken(ctx, pool, ""); err == nil {
		t.Fatal("empty name accepted")
	}
}
