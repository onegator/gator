package mcpremote

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/netguard"
	"github.com/onegator/gator/internal/server/store/db"
)

// serverStore is what keeping servers reads and writes. *db.Queries is one.
type serverStore interface {
	ListMCPServersVisibleIn(ctx context.Context, projectID pgtype.UUID) ([]db.McpServer, error)
	GetMCPServerIn(ctx context.Context, arg db.GetMCPServerInParams) (db.McpServer, error)
	CreateMCPServer(ctx context.Context, arg db.CreateMCPServerParams) (db.McpServer, error)
	UpdateMCPServer(ctx context.Context, arg db.UpdateMCPServerParams) (db.McpServer, error)
	SetMCPServerSecrets(ctx context.Context, arg db.SetMCPServerSecretsParams) (db.McpServer, error)
	ListMCPCalls(ctx context.Context, arg db.ListMCPCallsParams) ([]db.McpCall, error)
	ListWorkspaceMCPServers(ctx context.Context) ([]db.McpServer, error)
}

// roles answers a person's role in a project. auth.Authorizer is one.
type roles interface {
	ProjectRole(ctx context.Context, p auth.Principal, project pgtype.UUID) (auth.Role, error)
}

// Servers keeps a project's remote MCP servers (and the workspace's): who sees them, who changes
// them, their credentials and their allowlists. Everyone in a project sees its servers, never a
// secret; its admins change them.
type Servers struct {
	store    serverStore
	roles    roles
	keys     Keyring
	dial     Dialer
	cache    *ToolCache
	breakers *Breakers
	net      netguard.Policy
}

// NewServers builds it; net is what addresses a server may have.
func NewServers(store serverStore, rs roles, keys Keyring, dial Dialer, cache *ToolCache, breakers *Breakers, net netguard.Policy) *Servers {
	return &Servers{store: store, roles: rs, keys: keys, dial: dial, cache: cache, breakers: breakers, net: net}
}

// View is a server as people see it: never its secrets, only which are set.
type View struct {
	Server     db.McpServer
	SecretsSet []string
	Connected  bool   // the server has what it needs to be called
	Breaker    string // closed, open, half-open
}

// Settings change a server; nil leaves a field as it is. A server that is not there is made from
// them (URL and Auth then required).
type Settings struct {
	DisplayName  *string
	URL          *string
	Auth         *string
	AllowedTools *[]string
	Enabled      *bool
	// Secrets set the server's credentials; a key with an empty value removes it. Keys left out
	// keep their value. A new address drops every credential the old one had.
	Secrets map[string]string
}

// List answers the servers a project's agents may be given (the workspace's ones it does not
// shadow), or the workspace's own for project zero.
func (s *Servers) List(ctx context.Context, p auth.Principal, project pgtype.UUID) ([]View, error) {
	if err := s.can(ctx, p, project, auth.RoleViewer); err != nil {
		return nil, err
	}
	var rows []db.McpServer
	var err error
	if project.Valid {
		rows, err = s.store.ListMCPServersVisibleIn(ctx, project)
	} else {
		rows, err = s.store.ListWorkspaceMCPServers(ctx)
	}
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.view(r))
	}
	return out, nil
}

// Get answers one server of a scope.
func (s *Servers) Get(ctx context.Context, p auth.Principal, project pgtype.UUID, name string) (View, error) {
	if err := s.can(ctx, p, project, auth.RoleViewer); err != nil {
		return View{}, err
	}
	srv, err := s.own(ctx, project, name)
	if err != nil {
		return View{}, err
	}
	return s.view(srv), nil
}

// Put changes a scope's own server, or makes one. A project's admins only (workspace admins for
// the workspace's).
func (s *Servers) Put(ctx context.Context, p auth.Principal, project pgtype.UUID, name string, in Settings) (View, error) {
	if err := s.can(ctx, p, project, auth.RoleAdmin); err != nil {
		return View{}, err
	}
	srv, err := s.own(ctx, project, name)
	switch {
	case errors.Is(err, ErrNotFound):
		if srv, err = s.create(ctx, project, name, in); err != nil {
			return View{}, err
		}
	case err != nil:
		return View{}, err
	}
	next := db.UpdateMCPServerParams{ID: srv.ID, Url: srv.Url, AllowedTools: srv.AllowedTools, Enabled: srv.Enabled}
	moved := false
	if in.URL != nil && *in.URL != srv.Url {
		if err := CheckURL(*in.URL, s.net); err != nil {
			return View{}, err
		}
		next.Url, moved = *in.URL, true
	}
	if in.AllowedTools != nil {
		tools := []string{}
		for _, t := range *in.AllowedTools {
			if !ValidToolName(t) {
				return View{}, fmt.Errorf("%w: %q is not a tool's name", ErrInvalid, t)
			}
			if !slices.Contains(tools, t) {
				tools = append(tools, t)
			}
		}
		next.AllowedTools = tools
	}
	if in.Enabled != nil {
		next.Enabled = *in.Enabled
	}
	if srv, err = s.store.UpdateMCPServer(ctx, next); err != nil {
		return View{}, err
	}
	if moved || in.Secrets != nil {
		if srv, err = s.setSecrets(ctx, srv, in.Secrets, moved); err != nil {
			return View{}, err
		}
	}
	s.cache.Forget(srv.ID)
	s.breakers.Reset(srv.ID)
	return s.view(srv), nil
}

func (s *Servers) create(ctx context.Context, project pgtype.UUID, name string, in Settings) (db.McpServer, error) {
	if !ValidName(name) {
		return db.McpServer{}, fmt.Errorf("%w: a server's name is lowercase letters, digits and dashes, and not gator", ErrInvalid)
	}
	if in.URL == nil || in.Auth == nil {
		return db.McpServer{}, ErrNotFound
	}
	if err := CheckURL(*in.URL, s.net); err != nil {
		return db.McpServer{}, err
	}
	switch *in.Auth {
	case AuthBearer, AuthNone:
	default:
		return db.McpServer{}, fmt.Errorf("%w: a server made by hand signs in with a token (bearer) or not at all (none); OAuth comes with its package", ErrInvalid)
	}
	display := name
	if in.DisplayName != nil {
		display = strings.TrimSpace(*in.DisplayName)
	}
	srv, err := s.store.CreateMCPServer(ctx, db.CreateMCPServerParams{ProjectID: project, Name: name, DisplayName: display, Url: *in.URL,
		Auth: *in.Auth, AllowedTools: []string{}, Oauth: []byte("{}"), Source: []byte(`{"kind":"custom"}`)})
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok && pe.Code == "23505" {
		return srv, fmt.Errorf("%w: there is a server named %s", ErrInvalid, name)
	}
	return srv, err
}

// setSecrets merges secrets into a server's; moved drops what the old address had.
func (s *Servers) setSecrets(ctx context.Context, srv db.McpServer, in map[string]string, moved bool) (db.McpServer, error) {
	sec := map[string]string{}
	if !moved {
		var err error
		if sec, err = s.keys.Secrets(srv); err != nil {
			return srv, err
		}
	}
	for k, v := range in {
		if k == SecretRefreshToken || k == SecretAccessToken || k == SecretClientID || k == SecretClientSecret {
			return srv, fmt.Errorf("%w: an OAuth token or client comes from connecting, not from a request", ErrInvalid)
		}
		if v == "" {
			delete(sec, k)
		} else {
			sec[k] = v
		}
	}
	enc, err := s.keys.Seal(sec)
	if err != nil {
		return srv, err
	}
	var exp pgtype.Timestamptz
	if !moved {
		exp = srv.TokenExpiresAt
	}
	return s.store.SetMCPServerSecrets(ctx, db.SetMCPServerSecretsParams{ID: srv.ID, Secrets: enc, TokenExpiresAt: exp})
}

// ToolView is one of a server's tools as its admin sees it: whether agents may call it, and
// whether the server marks it read-only (the only ones chat agents get).
type ToolView struct {
	Name, Description string
	ReadOnly, Allowed bool
}

// Tools asks the server what tools it has, for its admin to choose from.
func (s *Servers) Tools(ctx context.Context, p auth.Principal, project pgtype.UUID, name string) ([]ToolView, error) {
	if err := s.can(ctx, p, project, auth.RoleAdmin); err != nil {
		return nil, err
	}
	srv, err := s.own(ctx, project, name)
	if err != nil {
		return nil, err
	}
	up, closeUp, err := s.dial.Dial(ctx, srv)
	if err != nil {
		return nil, err
	}
	defer closeUp()
	tools, err := guarded{Upstream: up, breaker: s.breakers.For(srv.ID)}.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ToolView, 0, len(tools))
	for _, t := range tools {
		out = append(out, ToolView{Name: t.Name, Description: t.Description, ReadOnly: t.Annotations != nil && t.Annotations.ReadOnlyHint,
			Allowed: slices.Contains(srv.AllowedTools, t.Name)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Calls are the server's calls from a scope, newest first, for its admins.
func (s *Servers) Calls(ctx context.Context, p auth.Principal, project pgtype.UUID, name string, limit int) ([]db.McpCall, error) {
	if err := s.can(ctx, p, project, auth.RoleAdmin); err != nil {
		return nil, err
	}
	srv, err := s.own(ctx, project, name)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.store.ListMCPCalls(ctx, db.ListMCPCallsParams{McpServerID: srv.ID, ProjectID: project, MaxRows: int32(limit)})
}

func (s *Servers) own(ctx context.Context, project pgtype.UUID, name string) (db.McpServer, error) {
	srv, err := s.store.GetMCPServerIn(ctx, db.GetMCPServerInParams{ProjectID: project, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return srv, ErrNotFound
	}
	return srv, err
}

func (s *Servers) view(srv db.McpServer) View {
	v := View{Server: srv, Breaker: s.breakers.State(srv.ID).String(), SecretsSet: []string{}}
	sec, err := s.keys.Secrets(srv)
	if err == nil {
		for k := range sec {
			if k != SecretRefreshToken && k != SecretClientID && k != SecretClientSecret {
				v.SecretsSet = append(v.SecretsSet, k)
			}
		}
		sort.Strings(v.SecretsSet)
	}
	switch srv.Auth {
	case AuthNone:
		v.Connected = true
	case AuthBearer:
		v.Connected = sec[SecretToken] != ""
	case AuthOAuth:
		v.Connected = sec[SecretAccessToken] != ""
	}
	srv.Secrets = ""
	v.Server = srv
	return v
}

func (s *Servers) can(ctx context.Context, p auth.Principal, project pgtype.UUID, min auth.Role) error {
	if p.Kind != auth.KindUser {
		return ErrForbidden
	}
	if !project.Valid {
		if min <= auth.RoleViewer || p.IsWorkspaceAdmin() {
			return nil
		}
		return ErrForbidden
	}
	role, err := s.roles.ProjectRole(ctx, p, project)
	if err != nil {
		return err
	}
	switch {
	case role < auth.RoleViewer:
		return ErrNotFound
	case role < min:
		return ErrForbidden
	}
	return nil
}
