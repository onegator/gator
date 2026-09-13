package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// principal returns the caller or writes 401.
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required", "unauthorized")
		return p, false
	}
	return p, true
}

// requireProject checks the caller holds at least min in project; writes 403 otherwise.
func (s *Server) requireProject(w http.ResponseWriter, r *http.Request, project pgtype.UUID, min auth.Role) (auth.Principal, bool) {
	p, ok := s.principal(w, r)
	if !ok {
		return p, false
	}
	if err := s.Authz.Require(r.Context(), p, project, min); err != nil {
		if errors.Is(err, auth.ErrForbidden) {
			writeError(w, http.StatusForbidden, "insufficient role", "forbidden")
		} else {
			s.fail(w, err)
		}
		return p, false
	}
	return p, true
}

// requireTask resolves the task's project and checks the role.
func (s *Server) requireTask(w http.ResponseWriter, r *http.Request, task pgtype.UUID, min auth.Role) (auth.Principal, bool) {
	project, err := s.Authz.TaskProject(r.Context(), task)
	if err != nil {
		s.fail(w, err)
		return auth.Principal{}, false
	}
	return s.requireProject(w, r, project, min)
}

// visibleProjects returns project ids the caller may read; nil means all (workspace admin).
func (s *Server) visibleProjects(r *http.Request, p auth.Principal) (map[[16]byte]bool, error) {
	if p.IsWorkspaceAdmin() || p.Kind == auth.KindRunner {
		return nil, nil
	}
	set := map[[16]byte]bool{}
	if p.Kind == auth.KindPlugin {
		if p.ProjectID.Valid {
			set[p.ProjectID.Bytes] = true
		}
		return set, nil
	}
	ms, err := db.New(s.Pool).ListMembershipsForUser(r.Context(), p.UserID)
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		set[m.ProjectID.Bytes] = true
	}
	return set, nil
}

// --- auth endpoints ---

func (s *Server) Me(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	scope := p.Scope
	writeJSON(w, http.StatusOK, gen.Me{Kind: string(p.Kind), Name: p.Name, WorkspaceRole: p.WorkspaceRole, UserId: toUUIDPtr(p.UserID), Scope: &scope})
}

func (s *Server) ListTokens(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	q := db.New(s.Pool)
	var rows []db.ApiToken
	var err error
	if p.IsWorkspaceAdmin() {
		runners, e1 := q.ListTokensByKind(r.Context(), "runner")
		plugins, e2 := q.ListTokensByKind(r.Context(), "plugin")
		own, e3 := q.ListTokensForUser(r.Context(), p.UserID)
		err = errors.Join(e1, e2, e3)
		rows = append(append(runners, plugins...), own...)
	} else if p.Kind == auth.KindUser {
		rows, err = q.ListTokensForUser(r.Context(), p.UserID)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]gen.Token, 0, len(rows))
	for _, t := range rows {
		out = append(out, toToken(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) CreateToken(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	var in gen.NewToken
	if !decode(w, r, &in) {
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required", "invalid")
		return
	}
	params := auth.IssueParams{Kind: auth.Kind(in.Kind), Name: in.Name}
	if in.TtlHours != nil && *in.TtlHours > 0 {
		params.TTL = time.Duration(*in.TtlHours) * time.Hour
	}
	switch params.Kind {
	case auth.KindUser:
		if p.Kind != auth.KindUser {
			writeError(w, http.StatusForbidden, "only users mint user tokens", "forbidden")
			return
		}
		params.UserID, params.Scope = p.UserID, "user"
	case auth.KindRunner:
		if !p.IsWorkspaceAdmin() {
			writeError(w, http.StatusForbidden, "runner tokens require workspace admin", "forbidden")
			return
		}
		params.Scope = "runner:" + in.Name
	case auth.KindPlugin:
		if in.ProjectId == nil {
			writeError(w, http.StatusBadRequest, "plugin tokens need projectId", "invalid")
			return
		}
		if _, ok := s.requireProject(w, r, fromUUID(*in.ProjectId), auth.RoleAdmin); !ok {
			return
		}
		params.ProjectID = fromUUID(*in.ProjectId)
		params.Scope = "plugin:" + in.ProjectId.String() + ":" + in.Name
	default:
		writeError(w, http.StatusBadRequest, "unknown token kind", "invalid")
		return
	}
	plaintext, rec, err := s.Tokens.Issue(r.Context(), params)
	if err != nil {
		s.fail(w, err)
		return
	}
	t := toToken(rec)
	writeJSON(w, http.StatusCreated, gen.IssuedToken{Id: t.Id, Kind: t.Kind, Scope: t.Scope, Name: t.Name, ProjectId: t.ProjectId,
		CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, Token: plaintext})
}

func (s *Server) RevokeToken(w http.ResponseWriter, r *http.Request, tokenId openapi_types.UUID) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	// Owners revoke their own; workspace admins revoke anything.
	if !p.IsWorkspaceAdmin() {
		own, err := db.New(s.Pool).ListTokensForUser(r.Context(), p.UserID)
		if err != nil {
			s.fail(w, err)
			return
		}
		mine := false
		for _, t := range own {
			if t.ID.Bytes == fromUUID(tokenId).Bytes {
				mine = true
			}
		}
		if !mine {
			writeError(w, http.StatusForbidden, "not your token", "forbidden")
			return
		}
	}
	if err := s.Tokens.Revoke(r.Context(), fromUUID(tokenId)); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) SetMember(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok {
		return
	}
	var in gen.Member
	if !decode(w, r, &in) {
		return
	}
	if err := db.New(s.Pool).UpsertMembership(r.Context(), db.UpsertMembershipParams{ProjectID: fromUUID(projectId), UserID: fromUUID(in.UserId), Role: string(in.Role)}); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toToken(t db.ApiToken) gen.Token {
	out := gen.Token{Id: toUUID(t.ID), Kind: t.Kind, Scope: t.Scope, Name: t.Name, ProjectId: toUUIDPtr(t.ProjectID), CreatedAt: t.CreatedAt.Time}
	if t.ExpiresAt.Valid {
		v := t.ExpiresAt.Time
		out.ExpiresAt = &v
	}
	if t.LastUsedAt.Valid {
		v := t.LastUsedAt.Time
		out.LastUsedAt = &v
	}
	return out
}
