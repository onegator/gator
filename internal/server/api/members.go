package api

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/invites"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// errLastAdmin refuses a change that would leave a project nobody can run: no one could
// invite, change roles or archive it short of a workspace admin stepping in.
var errLastAdmin = errors.New("a project needs at least one admin; make someone else admin first")

func validRole(r string) bool { return r == "admin" || r == "member" || r == "viewer" }

// changeMembership sets user's role in project, or takes them out when role is "". The
// project's admin rows are locked first, so the last-admin check holds against a concurrent
// change.
func (s *Server) changeMembership(ctx context.Context, project, user pgtype.UUID, role string) error {
	return store.InTx(ctx, s.Pool, func(q *db.Queries) error {
		admins, err := q.LockProjectAdmins(ctx, project)
		if err != nil {
			return err
		}
		wasAdmin := false
		for _, a := range admins {
			if a.Bytes == user.Bytes {
				wasAdmin = true
			}
		}
		if wasAdmin && role != "admin" && len(admins) == 1 {
			return errLastAdmin
		}
		if role == "" {
			n, err := q.DeleteMembership(ctx, db.DeleteMembershipParams{ProjectID: project, UserID: user})
			if err != nil {
				return err
			}
			if n == 0 {
				return pgx.ErrNoRows
			}
			return invites.Emit(ctx, q, "project.member_removed", project, map[string]any{"userId": uuidString(user)})
		}
		if err := q.UpsertMembership(ctx, db.UpsertMembershipParams{ProjectID: project, UserID: user, Role: role}); err != nil {
			return err
		}
		return invites.Emit(ctx, q, "project.member_set", project, map[string]any{"userId": uuidString(user), "role": role})
	})
}

func (s *Server) failMembership(w http.ResponseWriter, err error) {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, errLastAdmin):
		writeError(w, http.StatusConflict, err.Error(), "last_admin")
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		writeError(w, http.StatusNotFound, "no such person in this workspace", "not_found")
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not a member of this project", "not_found")
	default:
		s.fail(w, err)
	}
}

func (s *Server) SetMember(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok {
		return
	}
	var in gen.Member
	if !decode(w, r, &in) {
		return
	}
	if !validRole(string(in.Role)) {
		writeError(w, http.StatusBadRequest, "role is admin, member or viewer", "invalid")
		return
	}
	if err := s.changeMembership(r.Context(), fromUUID(projectId), fromUUID(in.UserId), string(in.Role)); err != nil {
		s.failMembership(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RemoveMember takes a person out of a project. An admin may remove anyone; anyone may leave.
func (s *Server) RemoveMember(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, userId openapi_types.UUID) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	need := auth.RoleAdmin
	if p.Kind == auth.KindUser && p.UserID.Bytes == userId {
		need = auth.RoleViewer
	}
	if _, ok := s.requireProject(w, r, fromUUID(projectId), need); !ok {
		return
	}
	if err := s.changeMembership(r.Context(), fromUUID(projectId), fromUUID(userId), ""); err != nil {
		s.failMembership(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListProjectCandidates lets a project admin find someone already in the workspace without
// seeing the whole workspace: a search of at least two characters, ten answers, and only the
// name and email needed to pick one.
func (s *Server) ListProjectCandidates(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, params gen.ListProjectCandidatesParams) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok {
		return
	}
	q := strings.TrimSpace(params.Q)
	if len([]rune(q)) < 2 {
		writeError(w, http.StatusBadRequest, "search with at least two characters", "invalid")
		return
	}
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	rows, err := db.New(s.Pool).SearchProjectCandidates(r.Context(), db.SearchProjectCandidatesParams{Pattern: "%" + escaped + "%", ProjectID: fromUUID(projectId)})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(rows, func(u db.SearchProjectCandidatesRow) gen.ProjectCandidate {
		return gen.ProjectCandidate{UserId: toUUID(u.ID), Name: u.Name, Email: u.Email}
	}))
}

// --- invitations ---

func (s *Server) ListInvitations(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok {
		return
	}
	rows, err := db.New(s.Pool).ListOpenInvitations(r.Context(), fromUUID(projectId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(rows, func(i db.ListOpenInvitationsRow) gen.Invitation {
		out := toInvitation(db.ProjectInvitation{ID: i.ID, ProjectID: i.ProjectID, Email: i.Email, Role: i.Role,
			InvitedBy: i.InvitedBy, CreatedAt: i.CreatedAt, ExpiresAt: i.ExpiresAt})
		if i.InvitedByName != "" {
			out.InvitedByName = &i.InvitedByName
		}
		return out
	}))
}

func (s *Server) CreateInvitation(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	p, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin)
	if !ok {
		return
	}
	var in gen.NewInvitation
	if !decode(w, r, &in) {
		return
	}
	email, err := invites.NormalizeEmail(in.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is not an email address", in.Email), "invalid")
		return
	}
	if !validRole(string(in.Role)) {
		writeError(w, http.StatusBadRequest, "role is admin, member or viewer", "invalid")
		return
	}
	project := fromUUID(projectId)
	member, err := db.New(s.Pool).IsProjectMemberByEmail(r.Context(), db.IsProjectMemberByEmailParams{ProjectID: project, Email: email})
	if err != nil {
		s.fail(w, err)
		return
	}
	if member {
		writeError(w, http.StatusConflict, email+" is already in this project; change their role instead", "conflict")
		return
	}
	token, hash := invites.NewToken()
	var inv db.ProjectInvitation
	err = store.InTx(r.Context(), s.Pool, func(q *db.Queries) error {
		if err := q.CloseOpenInvitation(r.Context(), db.CloseOpenInvitationParams{ProjectID: project, Email: email}); err != nil {
			return err
		}
		var err error
		inv, err = q.InsertInvitation(r.Context(), db.InsertInvitationParams{
			ProjectID: project, Email: email, Role: string(in.Role), TokenHash: hash, InvitedBy: p.UserID,
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(invites.TTL), Valid: true},
		})
		if err != nil {
			return err
		}
		return invites.Emit(r.Context(), q, "project.invited", project, map[string]any{"invitationId": uuidString(inv.ID), "role": inv.Role})
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		writeError(w, http.StatusConflict, "someone else invited "+email+" just now", "conflict")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respondIssued(w, r, http.StatusCreated, p, inv, token)
}

// RenewInvitation gives an open invitation a new token and a new expiry. Only the hash of the
// old token is known, so this is also how an admin gets a link to copy again.
func (s *Server) RenewInvitation(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, invitationId gen.InvitationId) {
	p, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin)
	if !ok {
		return
	}
	q := db.New(s.Pool)
	inv, err := q.GetOpenInvitation(r.Context(), db.GetOpenInvitationParams{ID: fromUUID(invitationId), ProjectID: fromUUID(projectId)})
	if err != nil {
		s.fail(w, err)
		return
	}
	token, hash := invites.NewToken()
	inv, err = q.RenewInvitation(r.Context(), db.RenewInvitationParams{ID: inv.ID, TokenHash: hash,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(invites.TTL), Valid: true}})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.respondIssued(w, r, http.StatusOK, p, inv, token)
}

func (s *Server) RevokeInvitation(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId, invitationId gen.InvitationId) {
	if _, ok := s.requireProject(w, r, fromUUID(projectId), auth.RoleAdmin); !ok {
		return
	}
	project := fromUUID(projectId)
	err := store.InTx(r.Context(), s.Pool, func(q *db.Queries) error {
		n, err := q.RevokeInvitation(r.Context(), db.RevokeInvitationParams{ID: fromUUID(invitationId), ProjectID: project})
		if err != nil {
			return err
		}
		if n == 0 {
			return pgx.ErrNoRows
		}
		return invites.Emit(r.Context(), q, "project.invitation_revoked", project, map[string]any{"invitationId": invitationId.String()})
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// respondIssued hands the inviter the links, after offering them to the mailer.
func (s *Server) respondIssued(w http.ResponseWriter, r *http.Request, status int, p auth.Principal, inv db.ProjectInvitation, token string) {
	project, err := db.New(s.Pool).GetProject(r.Context(), inv.ProjectID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := gen.IssuedInvitation{Invitation: toInvitation(inv), Link: invites.WebLink(s.baseURL(r), token), AppLink: invites.AppLink(token)}
	mailer := s.Mailer
	if mailer == nil {
		mailer = invites.LogMailer{Log: s.Log}
	}
	sent, err := mailer.SendInvitation(r.Context(), invites.Message{
		To: inv.Email, ProjectName: project.Name, InviterName: p.Name, Role: inv.Role,
		Link: out.Link, AppLink: out.AppLink, ExpiresAt: inv.ExpiresAt.Time,
	})
	// The invitation stands either way; a failed send only means the inviter passes it on.
	if err != nil {
		s.Log.Warn("invitation email failed", "err", err)
	}
	out.Emailed = sent && err == nil
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, out)
}

// AcceptInvitation is the signed-in person opening their invitation link.
func (s *Server) AcceptInvitation(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if p.Kind != auth.KindUser {
		writeError(w, http.StatusForbidden, "only a person can accept an invitation", "forbidden")
		return
	}
	var in gen.AcceptInvitation
	if !decode(w, r, &in) {
		return
	}
	user, err := db.New(s.Pool).GetUser(r.Context(), p.UserID)
	if err != nil {
		s.fail(w, err)
		return
	}
	inv, err := invites.Accept(r.Context(), s.Pool, strings.TrimSpace(in.Token), user)
	switch {
	case errors.Is(err, invites.ErrWrongEmail):
		writeError(w, http.StatusForbidden, err.Error()+"; sign in with that one to accept it", "wrong_email")
		return
	case errors.Is(err, invites.ErrUnknown):
		writeError(w, http.StatusNotFound, err.Error()+"; ask for a new link", "not_found")
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	pr, err := db.New(s.Pool).GetProject(r.Context(), inv.ProjectID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, withRole(toProject(pr), s.roleIn(r, p, pr.ID)))
}

// baseURL is where this server is reached from outside, for links people pass around.
// GATOR_PUBLIC_URL names it; without it, the address the inviter used is the best guess.
func (s *Server) baseURL(r *http.Request) string {
	if s.PublicURL != "" {
		return strings.TrimRight(s.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

var invitePage = template.Must(template.New("invite").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Gator invitation</title>
<style>body{font:16px/1.5 -apple-system,system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 1rem;color:#1d1d1f}
a.open{display:inline-block;margin:1rem 0;padding:.6rem 1.2rem;border-radius:8px;background:#2f7d32;color:#fff;text-decoration:none}
@media (prefers-color-scheme:dark){body{background:#1c1c1e;color:#f2f2f7}}</style></head>
<body><h1>You are invited to a Gator project</h1>
<p>Open the invitation in Gator and sign in with the address it was sent to.</p>
<p><a class="open" href="{{.}}">Open in Gator</a></p>
<p>No Gator yet? Install it, sign in with that address, and you will find the project waiting.</p>
</body></html>`))

// invitePageHandler answers the https form of an invitation link: a page that hands the token
// on to the app. It reads nothing and changes nothing; only the app accepts.
func invitePageHandler(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if !inviteToken.MatchString(token) {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Robots-Tag", "noindex")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	_ = invitePage.Execute(w, template.URL(invites.AppLink(token)))
}

var inviteToken = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func toInvitation(i db.ProjectInvitation) gen.Invitation {
	return gen.Invitation{Id: toUUID(i.ID), ProjectId: toUUID(i.ProjectID), Email: i.Email, Role: gen.InvitationRole(i.Role),
		InvitedBy: toUUIDPtr(i.InvitedBy), CreatedAt: i.CreatedAt.Time, ExpiresAt: i.ExpiresAt.Time}
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return openapi_types.UUID(u.Bytes).String()
}
