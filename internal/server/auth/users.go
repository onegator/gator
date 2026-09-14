package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store/db"
)

// ErrIdentityConflict means the email already belongs to a user bound to a different OIDC
// subject. Linking it would let one identity take over another's account.
var ErrIdentityConflict = errors.New("email is bound to a different identity")

// LinkOIDCUser resolves a login to a user:
//   - a known subject updates that user's email and name;
//   - an unknown subject with an unknown email creates the user;
//   - an unknown subject whose email belongs to a user without a subject (created by
//     `gator-server admin create-admin`) links that user;
//   - an email already bound to another subject is refused.
func LinkOIDCUser(ctx context.Context, q *db.Queries, email, name, subject string) (db.User, error) {
	u, err := q.GetUserByOIDCSubject(ctx, &subject)
	if err == nil {
		if u.Email == email && u.Name == name {
			return u, nil
		}
		return q.UpdateUserProfile(ctx, db.UpdateUserProfileParams{ID: u.ID, Email: email, Name: name})
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.User{}, err
	}
	u, err = q.UpsertUserByEmail(ctx, db.UpsertUserByEmailParams{Email: email, Name: name, OidcSubject: &subject})
	if err != nil {
		return db.User{}, err
	}
	if u.OidcSubject == nil || *u.OidcSubject != subject {
		return db.User{}, ErrIdentityConflict
	}
	return u, nil
}

// BootstrapAdmin creates (or promotes) a workspace admin by email and returns a user token.
// It is the break-glass path for a fresh install before OIDC is configured; the account
// links to OIDC on its first login with the same verified email.
func BootstrapAdmin(ctx context.Context, pool *pgxpool.Pool, email, name string, ttl time.Duration) (db.User, string, error) {
	if email == "" {
		return db.User{}, "", errors.New("email is required")
	}
	if name == "" {
		name = email
	}
	q := db.New(pool)
	u, err := q.UpsertUserByEmail(ctx, db.UpsertUserByEmailParams{Email: email, Name: name})
	if err != nil {
		return db.User{}, "", err
	}
	if u.WorkspaceRole != "admin" {
		if err := q.SetWorkspaceRole(ctx, db.SetWorkspaceRoleParams{ID: u.ID, WorkspaceRole: "admin"}); err != nil {
			return db.User{}, "", err
		}
		u.WorkspaceRole = "admin"
	}
	token, _, err := Tokens{Pool: pool}.Issue(ctx, IssueParams{Kind: KindUser, Scope: "user", Name: "bootstrap", UserID: u.ID, TTL: ttl})
	if err != nil {
		return db.User{}, "", err
	}
	return u, token, nil
}

// IssueRunnerToken mints a runner token from the host, without going through the API.
func IssueRunnerToken(ctx context.Context, pool *pgxpool.Pool, name string) (string, error) {
	if name == "" {
		return "", errors.New("runner name is required")
	}
	token, _, err := Tokens{Pool: pool}.Issue(ctx, IssueParams{Kind: KindRunner, Scope: "runner:" + name, Name: name})
	return token, err
}
