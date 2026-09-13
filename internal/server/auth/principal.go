// Package auth owns identity (OIDC sessions, bearer tokens), authorization (workspace and
// project roles) and the audit of every mutation.
package auth

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

// Kind is the type of authenticated subject.
type Kind string

const (
	KindUser   Kind = "user"
	KindRunner Kind = "runner"
	KindPlugin Kind = "plugin"
)

// Principal is the authenticated subject of a request.
type Principal struct {
	Kind          Kind
	UserID        pgtype.UUID // set for users
	TokenID       pgtype.UUID // set when authenticated by token
	Scope         string      // runner:<id> | plugin:<project>:<name> | user
	ProjectID     pgtype.UUID // set for plugin tokens
	WorkspaceRole string      // admin | member, users only
	Name          string
}

type principalKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the principal, or ok=false for anonymous requests.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// IsWorkspaceAdmin reports whether p administers the whole instance.
func (p Principal) IsWorkspaceAdmin() bool { return p.Kind == KindUser && p.WorkspaceRole == "admin" }
