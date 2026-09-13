package auth

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store/db"
)

// ErrForbidden is returned when the principal lacks the required role.
var ErrForbidden = errors.New("forbidden")

// Role is a project role. Higher values include lower ones.
type Role int

const (
	RoleNone Role = iota
	RoleViewer
	RoleMember
	RoleAdmin
)

func parseRole(s string) Role {
	switch s {
	case "admin":
		return RoleAdmin
	case "member":
		return RoleMember
	case "viewer":
		return RoleViewer
	}
	return RoleNone
}

// Authorizer answers project-scoped permission questions.
type Authorizer struct{ Pool *pgxpool.Pool }

// ProjectRole returns the effective role of p in project. Workspace admins are admins
// everywhere. Runner tokens are members of every project (they act on leased jobs only).
// Plugin tokens are members of their own project.
func (a Authorizer) ProjectRole(ctx context.Context, p Principal, project pgtype.UUID) (Role, error) {
	switch p.Kind {
	case KindRunner:
		return RoleMember, nil
	case KindPlugin:
		if p.ProjectID.Valid && p.ProjectID.Bytes == project.Bytes {
			return RoleMember, nil
		}
		return RoleNone, nil
	}
	if p.IsWorkspaceAdmin() {
		return RoleAdmin, nil
	}
	m, err := db.New(a.Pool).GetMembership(ctx, db.GetMembershipParams{ProjectID: project, UserID: p.UserID})
	if errors.Is(err, pgx.ErrNoRows) {
		return RoleNone, nil
	}
	if err != nil {
		return RoleNone, err
	}
	return parseRole(m.Role), nil
}

// Require returns ErrForbidden unless p holds at least min in project.
func (a Authorizer) Require(ctx context.Context, p Principal, project pgtype.UUID, min Role) error {
	r, err := a.ProjectRole(ctx, p, project)
	if err != nil {
		return err
	}
	if r < min {
		return ErrForbidden
	}
	return nil
}

// TaskProject resolves a task to its project.
func (a Authorizer) TaskProject(ctx context.Context, task pgtype.UUID) (pgtype.UUID, error) {
	return db.New(a.Pool).GetTaskProject(ctx, task)
}
