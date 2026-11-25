package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/marketplace"
	"github.com/onegator/gator/internal/server/store/db"
)

// marketplaceCaller is the caller of a marketplace endpoint, or a written answer: the module is
// optional, and a job's identity never reaches it.
func (s *Server) marketplaceCaller(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	if s.Marketplace == nil {
		writeError(w, http.StatusServiceUnavailable, "the marketplace is not running on this server", "unavailable")
		return auth.Principal{}, false
	}
	return s.principal(w, r)
}

func (s *Server) failMarketplace(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, marketplace.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found", "not_found")
	case errors.Is(err, marketplace.ErrForbidden):
		writeError(w, http.StatusForbidden, "not allowed", "forbidden")
	case errors.Is(err, marketplace.ErrPermissions):
		writeError(w, http.StatusConflict, err.Error(), "permissions_not_accepted")
	case errors.Is(err, marketplace.ErrConflict):
		writeError(w, http.StatusConflict, err.Error(), "conflict")
	case errors.Is(err, marketplace.ErrInvalid), errors.Is(err, marketplace.ErrSignature), errors.Is(err, marketplace.ErrChecksum):
		writeError(w, http.StatusBadRequest, err.Error(), "invalid")
	default:
		s.fail(w, err)
	}
}

func (s *Server) ListRegistries(w http.ResponseWriter, r *http.Request) {
	p, ok := s.marketplaceCaller(w, r)
	if !ok {
		return
	}
	rows, err := s.Marketplace.Catalog.Registries(r.Context(), p)
	if err != nil {
		s.failMarketplace(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mapAll(rows, toRegistry))
}

func (s *Server) AddRegistry(w http.ResponseWriter, r *http.Request) {
	p, ok := s.marketplaceCaller(w, r)
	if !ok {
		return
	}
	var in gen.NewRegistry
	if !decode(w, r, &in) {
		return
	}
	reg, err := s.Marketplace.Catalog.AddRegistry(r.Context(), p, in.Name, in.Url, deref(in.PublicKey))
	if err != nil {
		s.failMarketplace(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRegistry(reg))
}

func (s *Server) DeleteRegistry(w http.ResponseWriter, r *http.Request, registryId gen.RegistryId) {
	p, ok := s.marketplaceCaller(w, r)
	if !ok {
		return
	}
	if err := s.Marketplace.Catalog.DeleteRegistry(r.Context(), p, fromUUID(registryId)); err != nil {
		s.failMarketplace(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) FetchRegistry(w http.ResponseWriter, r *http.Request, registryId gen.RegistryId) {
	p, ok := s.marketplaceCaller(w, r)
	if !ok {
		return
	}
	res, err := s.Marketplace.Catalog.Refresh(r.Context(), p, fromUUID(registryId))
	if err != nil {
		s.failMarketplace(w, err)
		return
	}
	out := gen.RegistryFetch{Registry: toRegistry(res.Registry), Fetched: []gen.PackageRef{}, Rejected: []gen.RejectedPackage{}}
	for _, it := range res.Fetched {
		out.Fetched = append(out.Fetched, gen.PackageRef{Kind: gen.PackageRefKind(it.Kind), Name: it.Name, Version: it.Version})
	}
	for _, rej := range res.Rejected {
		out.Rejected = append(out.Rejected, gen.RejectedPackage{Kind: rej.Item.Kind, Name: rej.Item.Name, Version: rej.Item.Version, Reason: rej.Reason})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) ListMarketplace(w http.ResponseWriter, r *http.Request, params gen.ListMarketplaceParams) {
	p, ok := s.marketplaceCaller(w, r)
	if !ok {
		return
	}
	f := marketplace.Filter{Query: deref(params.Q), Phase: deref(params.Phase)}
	if params.Kind != nil {
		f.Kind = string(*params.Kind)
	}
	var project pgtype.UUID
	if params.ProjectId != nil {
		project = fromUUID(*params.ProjectId)
		if _, ok := s.requireProject(w, r, project, auth.RoleViewer); !ok {
			return
		}
	}
	list, err := s.Marketplace.Catalog.List(r.Context(), p, f)
	if err != nil {
		s.failMarketplace(w, err)
		return
	}
	out := make([]gen.MarketplaceItem, 0, len(list))
	for _, l := range list {
		item := gen.MarketplaceItem{RegistryId: toUUID(l.RegistryID), RegistryName: l.RegistryName,
			Manifest: toManifest(l.Manifest, false), Checksum: l.Checksum, Versions: l.Versions}
		s.withProjectState(r, p, project, l.Manifest, &item.Installed, &item.Missing)
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) GetMarketplacePackage(w http.ResponseWriter, r *http.Request, kind gen.GetMarketplacePackageParamsKind, name string, params gen.GetMarketplacePackageParams) {
	p, ok := s.marketplaceCaller(w, r)
	if !ok {
		return
	}
	var registry, project pgtype.UUID
	if params.RegistryId != nil {
		registry = fromUUID(*params.RegistryId)
	}
	if params.ProjectId != nil {
		project = fromUUID(*params.ProjectId)
		if _, ok := s.requireProject(w, r, project, auth.RoleViewer); !ok {
			return
		}
	}
	versions, err := s.Marketplace.Catalog.Versions(r.Context(), p, string(kind), name, registry)
	if err != nil {
		s.failMarketplace(w, err)
		return
	}
	chosen := versions[0]
	for _, v := range versions {
		if params.Version != nil && v.Manifest.Version == *params.Version {
			chosen = v
			break
		}
		if params.Version == nil && v.Listed {
			chosen = v
			break
		}
	}
	if params.Version != nil && chosen.Manifest.Version != *params.Version {
		writeError(w, http.StatusNotFound, "no such version", "not_found")
		return
	}
	pkg, err := s.Marketplace.Catalog.Package(r.Context(), chosen.RegistryID, string(kind), name, chosen.Manifest.Version)
	if err != nil {
		s.failMarketplace(w, err)
		return
	}
	out := gen.MarketplaceDetail{RegistryId: toUUID(chosen.RegistryID), RegistryName: chosen.RegistryName,
		Manifest: toManifest(pkg.Manifest, true), Checksum: chosen.Checksum, Files: []string{}, Versions: []gen.PackageVersion{}}
	for _, f := range pkg.Files {
		out.Files = append(out.Files, f.Path)
	}
	for _, v := range versions {
		if v.RegistryID != chosen.RegistryID {
			continue
		}
		out.Versions = append(out.Versions, gen.PackageVersion{Version: v.Manifest.Version, Checksum: v.Checksum, Listed: v.Listed,
			Permissions: orEmpty(v.Manifest.Permissions)})
	}
	s.withProjectState(r, p, project, pkg.Manifest, &out.Installed, &out.Missing)
	writeJSON(w, http.StatusOK, out)
}

func toRegistry(r db.MarketplaceRegistry) gen.Registry {
	out := gen.Registry{Id: toUUID(r.ID), Name: r.Name, Url: r.Url, Signed: r.PublicKey != "", Builtin: r.Builtin,
		FetchError: r.FetchError, CreatedAt: r.CreatedAt.Time}
	if r.PublicKey != "" {
		out.PublicKey = &r.PublicKey
	}
	if r.FetchedAt.Valid {
		out.FetchedAt = &r.FetchedAt.Time
	}
	return out
}

func toManifest(m marketplace.Manifest, full bool) gen.PackageManifest {
	out := gen.PackageManifest{Kind: gen.PackageManifestKind(m.Kind), Name: m.Name, Version: m.Version, DisplayName: m.DisplayName,
		Description: m.Description, Permissions: orEmpty(m.Permissions),
		Requires: gen.PackageRequires{Capabilities: nilIfEmpty(m.Requires.Capabilities), Mcp: nilIfEmpty(m.Requires.MCP), Skills: nilIfEmpty(m.Requires.Skills)}}
	if len(m.Phases) > 0 {
		out.Phases = &m.Phases
	}
	if m.Icon != "" {
		out.Icon = &m.Icon
	}
	if m.ExamplePrompt != "" {
		out.ExamplePrompt = &m.ExamplePrompt
	}
	if full {
		if m.Readme != "" {
			out.Readme = &m.Readme
		}
		var d map[string]any
		if json.Unmarshal(m.Detail, &d) == nil && len(d) > 0 {
			out.Detail = &d
		}
	}
	return out
}

func nilIfEmpty(s []string) *[]string {
	if len(s) == 0 {
		return nil
	}
	return &s
}
