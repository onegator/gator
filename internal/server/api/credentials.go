package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/credentials"
	"github.com/onegator/gator/internal/server/store/db"
)

// person is the signed-in person calling, for what is theirs alone: their own model logins. A
// runner, a plugin or a job's agent has none and may manage nobody's.
func (s *Server) person(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	if s.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "people's own model logins are not enabled on this server", "unavailable")
		return auth.Principal{}, false
	}
	p, ok := s.principal(w, r)
	if !ok {
		return p, false
	}
	if p.Kind != auth.KindUser || !p.UserID.Valid {
		writeError(w, http.StatusForbidden, "only a signed-in person has model logins of their own", "forbidden")
		return p, false
	}
	return p, true
}

// ListMyCredentials is the caller's logins, the kinds there are and the workspace's rule.
func (s *Server) ListMyCredentials(w http.ResponseWriter, r *http.Request) {
	p, ok := s.person(w, r)
	if !ok {
		return
	}
	views, err := s.Credentials.List(r.Context(), p.UserID)
	if err != nil {
		s.fail(w, err)
		return
	}
	lend, err := s.Credentials.RunnerLogins(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := gen.ModelCredentials{Credentials: toCredentials(views), Kinds: []gen.ModelCredentialKind{}, RunnerLogins: lend}
	rank := map[string]int{}
	for _, k := range s.Credentials.Kinds() {
		d := k.Describe()
		_, rotates := k.(credentials.Rotating)
		_, checkable := k.(credentials.Checker)
		multiline := d.Multiline
		out.Kinds = append(out.Kinds, gen.ModelCredentialKind{Kind: k.Name(), Backend: k.Backend(), Label: d.Label, Help: d.Help,
			Preference: rank[k.Backend()], Rotates: rotates, Checkable: checkable, Multiline: &multiline})
		rank[k.Backend()]++
	}
	writeJSON(w, http.StatusOK, out)
}

// PutMyCredential saves the caller's login of a kind.
func (s *Server) PutMyCredential(w http.ResponseWriter, r *http.Request, kind string) {
	p, ok := s.person(w, r)
	if !ok {
		return
	}
	var in gen.ModelCredentialSecret
	if !decode(w, r, &in) {
		return
	}
	v, err := s.Credentials.Put(r.Context(), p.UserID, kind, in.Secret)
	if err != nil {
		s.credentialError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCredential(v))
}

// DeleteMyCredential removes the caller's login of a kind.
func (s *Server) DeleteMyCredential(w http.ResponseWriter, r *http.Request, kind string) {
	p, ok := s.person(w, r)
	if !ok {
		return
	}
	if err := s.Credentials.Delete(r.Context(), p.UserID, kind); err != nil {
		s.credentialError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CheckMyCredential tries the caller's login with its provider.
func (s *Server) CheckMyCredential(w http.ResponseWriter, r *http.Request, kind string) {
	p, ok := s.person(w, r)
	if !ok {
		return
	}
	v, err := s.Credentials.Check(r.Context(), p.UserID, kind)
	if err != nil {
		s.credentialError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCredential(v))
}

// credentialError answers what the credentials service refused. Its messages never carry a value.
func (s *Server) credentialError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, credentials.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
	case errors.Is(err, credentials.ErrUnknownKind), errors.Is(err, credentials.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error(), "not_found")
	case errors.Is(err, credentials.ErrCannotCheck):
		writeError(w, http.StatusUnprocessableEntity, err.Error(), "cannot_check")
	case errors.Is(err, credentials.ErrUnavailable):
		writeError(w, http.StatusBadGateway, err.Error(), "unavailable")
	case errors.Is(err, credentials.ErrNoKeyring):
		writeError(w, http.StatusServiceUnavailable, err.Error(), "unavailable")
	default:
		s.fail(w, err)
	}
}

// ListUserCredentials is which logins a person has, for a workspace admin: never their values.
func (s *Server) ListUserCredentials(w http.ResponseWriter, r *http.Request, userId openapi_types.UUID) {
	if !s.workspaceAdmin(w, r, "a person's model logins are seen by workspace admins") {
		return
	}
	views, err := s.Credentials.List(r.Context(), fromUUID(userId))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toCredentials(views))
}

// GetModelCredentialPolicy says whether jobs whose payer has no login run on the runner's.
func (s *Server) GetModelCredentialPolicy(w http.ResponseWriter, r *http.Request) {
	if s.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "people's own model logins are not enabled on this server", "unavailable")
		return
	}
	if _, ok := s.principal(w, r); !ok {
		return
	}
	on, err := s.Credentials.RunnerLogins(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, gen.ModelCredentialPolicy{RunnerLogins: on})
}

// SetModelCredentialPolicy sets it.
func (s *Server) SetModelCredentialPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.workspaceAdmin(w, r, "whether jobs may run on the runners' own logins is a workspace admin's call") {
		return
	}
	var in gen.ModelCredentialPolicy
	if !decode(w, r, &in) {
		return
	}
	if err := s.Credentials.SetRunnerLogins(r.Context(), in.RunnerLogins); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

// GetMyModelUsage is what the caller's own logins paid for.
func (s *Server) GetMyModelUsage(w http.ResponseWriter, r *http.Request, params gen.GetMyModelUsageParams) {
	p, ok := s.person(w, r)
	if !ok {
		return
	}
	since := usageSince(params.Since)
	rows, err := db.New(s.Pool).UsageByCredential(r.Context(), db.UsageByCredentialParams{UserID: p.UserID, Since: ts(since)})
	if err != nil {
		s.fail(w, err)
		return
	}
	out := gen.ModelUsage{Since: since, Items: make([]gen.ModelUsageRow, 0, len(rows))}
	for _, u := range rows {
		out.Items = append(out.Items, gen.ModelUsageRow{CredentialKind: u.CredentialKind, Jobs: u.Jobs, InputTokens: u.InputTokens,
			OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
			CostUsd: u.CostUsd, DurationMs: u.DurationMs})
	}
	writeJSON(w, http.StatusOK, out)
}

// GetWorkspaceModelUsage is what every login paid for, by person, for a workspace admin.
func (s *Server) GetWorkspaceModelUsage(w http.ResponseWriter, r *http.Request, params gen.GetWorkspaceModelUsageParams) {
	if !s.workspaceAdmin(w, r, "the workspace's usage by person is seen by workspace admins") {
		return
	}
	since := usageSince(params.Since)
	rows, err := db.New(s.Pool).UsageByPayer(r.Context(), ts(since))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := gen.ModelUsage{Since: since, Items: make([]gen.ModelUsageRow, 0, len(rows))}
	for _, u := range rows {
		row := gen.ModelUsageRow{UserId: toUUIDPtr(u.PaidBy), CredentialKind: u.CredentialKind, Jobs: u.Jobs, InputTokens: u.InputTokens,
			OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
			CostUsd: u.CostUsd, DurationMs: u.DurationMs}
		if u.PaidBy.Valid {
			row.Name, row.Email = ptr(u.Name), ptr(u.Email)
		}
		out.Items = append(out.Items, row)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) workspaceAdmin(w http.ResponseWriter, r *http.Request, why string) bool {
	if s.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "people's own model logins are not enabled on this server", "unavailable")
		return false
	}
	p, ok := s.principal(w, r)
	if !ok {
		return false
	}
	if !p.IsWorkspaceAdmin() {
		writeError(w, http.StatusForbidden, why, "forbidden")
		return false
	}
	return true
}

func usageSince(since *time.Time) time.Time {
	if since != nil {
		return *since
	}
	return time.Now().AddDate(0, 0, -30)
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func toCredentials(views []credentials.View) []gen.ModelCredential {
	out := make([]gen.ModelCredential, 0, len(views))
	for _, v := range views {
		out = append(out, toCredential(v))
	}
	return out
}

func toCredential(v credentials.View) gen.ModelCredential {
	return gen.ModelCredential{Kind: v.Kind, Backend: v.Backend, Label: v.Label, Hint: v.Hint, Status: gen.ModelCredentialStatus(v.Status),
		LastError: v.LastError, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, CheckedAt: v.CheckedAt, LastUsedAt: v.LastUsedAt}
}
