package api

import (
	"errors"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/bridges"
	"github.com/onegator/gator/internal/server/store/db"
)

func (s *Server) bridgeCaller(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	if s.Bridges == nil {
		writeError(w, http.StatusServiceUnavailable, "messaging bridges are not configured on this server", "unavailable")
		return auth.Principal{}, false
	}
	return s.principal(w, r)
}

func (s *Server) failBridge(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, bridges.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found", "not_found")
	case errors.Is(err, bridges.ErrForbidden):
		writeError(w, http.StatusForbidden, "not allowed", "forbidden")
	case errors.Is(err, bridges.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
	case errors.Is(err, bridges.ErrTooMany):
		writeError(w, http.StatusTooManyRequests, err.Error(), "rate_limited")
	default:
		s.fail(w, err)
	}
}

func (s *Server) CreateExternalLinkCode(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	p, ok := s.bridgeCaller(w, r)
	if !ok {
		return
	}
	var in gen.NewExternalLink
	if !decode(w, r, &in) {
		return
	}
	code, err := s.Bridges.NewLinkCode(r.Context(), p, fromUUID(projectId), in.Provider)
	if err != nil {
		s.failBridge(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, gen.ExternalLinkCode{Code: code.Code, Provider: code.Provider, ExpiresAt: code.ExpiresAt})
}

func (s *Server) ListExternalIdentities(w http.ResponseWriter, r *http.Request, projectId gen.ProjectId) {
	p, ok := s.bridgeCaller(w, r)
	if !ok {
		return
	}
	list, err := s.Bridges.Identities(r.Context(), p, fromUUID(projectId))
	if err != nil {
		s.failBridge(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(list, toExternalIdentity))
}

func (s *Server) UnlinkExternalIdentity(w http.ResponseWriter, r *http.Request, identityId openapi_types.UUID) {
	p, ok := s.bridgeCaller(w, r)
	if !ok {
		return
	}
	who, err := s.Bridges.Unlink(r.Context(), p, fromUUID(identityId))
	if err != nil {
		s.failBridge(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toExternalIdentity(who))
}

func toExternalIdentity(e db.ExternalIdentity) gen.ExternalIdentity {
	return gen.ExternalIdentity{Id: toUUID(e.ID), ProjectId: toUUID(e.ProjectID), Provider: e.Provider, ExternalId: e.ExternalID,
		DisplayName: e.DisplayName, Linked: e.UserID.Valid, UserId: toUUIDPtr(e.UserID), LinkedAt: timePtr(e.LinkedAt),
		CreatedAt: e.CreatedAt.Time}
}
