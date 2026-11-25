package mcpremote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/oauth2"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/plugins"
	"github.com/onegator/gator/internal/server/store/db"
)

// OAuthApp is the server's own OAuth app at a provider (GATOR_<PROVIDER>_CLIENT_ID and
// _CLIENT_SECRET, as plugins have). It is used for a remote server only at the authorization
// servers its operator named for it (GATOR_<PROVIDER>_OAUTH_HOSTS): a package that says
// "provider: github" gets no GitHub secret unless the server it names signs in at a host the
// operator listed.
type OAuthApp struct {
	ID, Secret string
	Scopes     []string // replace the server's when set
	Hosts      []string // authorization server hosts it may be used at
}

// oauthSpec is mcp_servers.oauth: what the integration package declared (the authorization server
// a person accepted, an optional provider whose app may be used there, scopes) and what discovery
// found and which client signs in.
type oauthSpec struct {
	Provider            string      `json:"provider,omitempty"`
	AuthorizationServer string      `json:"authorization_server,omitempty"`
	Scopes              []string    `json:"scopes,omitempty"`
	Discovered          *Discovered `json:"discovered,omitempty"`
	// Client is "registered" (a client of this server's own, its id and secret in its secrets)
	// or "app" (the provider's app the operator allowed at this authorization server).
	Client string `json:"client,omitempty"`
}

// Secret names of a registered client.
const (
	SecretClientID     = "client_id"
	SecretClientSecret = "client_secret"
)

// oauthStore is what connecting reads and writes. *db.Queries is one.
type oauthStore interface {
	GetMCPServerIn(ctx context.Context, arg db.GetMCPServerInParams) (db.McpServer, error)
	SetMCPServerSecrets(ctx context.Context, arg db.SetMCPServerSecretsParams) (db.McpServer, error)
	SetMCPServerOAuth(ctx context.Context, arg db.SetMCPServerOAuthParams) (db.McpServer, error)
	InsertPluginConnectState(ctx context.Context, arg db.InsertPluginConnectStateParams) error
	DeleteExpiredPluginConnectStates(ctx context.Context) error
}

// OAuth connects remote servers that sign in with OAuth, through the same /connect/callback as
// plugins (the state says it is a server's), and keeps their tokens fresh. It is Credentials too:
// a token about to expire is renewed before it goes out.
type OAuth struct {
	store     oauthStore
	roles     roles
	keys      Keyring
	apps      func(provider string) (OAuthApp, bool)
	publicURL string
	http      *http.Client
	cache     *ToolCache
	now       func() time.Time
	mu        sync.Mutex // one renewal at a time: a refresh token is often good once
}

// NewOAuth builds it.
func NewOAuth(store oauthStore, rs roles, keys Keyring, apps func(string) (OAuthApp, bool), publicURL string, hc *http.Client, cache *ToolCache) *OAuth {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	if apps == nil {
		apps = func(string) (OAuthApp, bool) { return OAuthApp{}, false }
	}
	return &OAuth{store: store, roles: rs, keys: keys, apps: apps, publicURL: strings.TrimRight(publicURL, "/"), http: hc, cache: cache, now: time.Now}
}

// ErrNotConnectable is a server this one cannot connect: it does not sign in with OAuth, or this
// server has no OAuth app or public address for it.
var ErrNotConnectable = errors.New("the server cannot be connected")

const connectTTL = 15 * time.Minute

// Connect starts connecting a project's server: it remembers who asked, for which project and
// server, and answers the provider's address for the person's browser. A project's admins only.
func (o *OAuth) Connect(ctx context.Context, p auth.Principal, project pgtype.UUID, name, returnTo string) (string, error) {
	if p.Kind != auth.KindUser {
		return "", ErrForbidden
	}
	if role, err := o.roles.ProjectRole(ctx, p, project); err != nil {
		return "", err
	} else if role < auth.RoleViewer {
		return "", ErrNotFound
	} else if role < auth.RoleAdmin {
		return "", ErrForbidden
	}
	if !plugins.ReturnAllowed(returnTo) {
		return "", fmt.Errorf("%w: return_to must be a gator:// or loopback address", ErrInvalid)
	}
	srv, err := o.store.GetMCPServerIn(ctx, db.GetMCPServerInParams{ProjectID: project, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if srv, err = o.prepare(ctx, srv); err != nil {
		return "", err
	}
	cfg, spec, err := o.config(srv)
	if err != nil {
		return "", err
	}
	state, verifier := randomToken(), oauth2.GenerateVerifier()
	authorize := cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("resource", spec.Discovered.Resource))
	return plugins.SaveConnect(ctx, o.store, o.publicURL, state, db.InsertPluginConnectStateParams{ProjectID: project, PluginName: name,
		UserID: p.UserID, Verifier: verifier, ReturnTo: returnTo, Target: plugins.TargetMCP, AuthorizeUrl: authorize})
}

// CompleteConnect implements plugins.Connector: the provider sent the browser back with a code,
// which is traded for the server's token.
func (o *OAuth) CompleteConnect(ctx context.Context, st db.PluginConnectState, code string) error {
	if code == "" {
		return errors.New("the provider sent no code")
	}
	srv, err := o.store.GetMCPServerIn(ctx, db.GetMCPServerInParams{ProjectID: st.ProjectID, Name: st.PluginName})
	if err != nil {
		return err
	}
	cfg, spec, err := o.config(srv)
	if err != nil {
		return err
	}
	tok, err := cfg.Exchange(context.WithValue(ctx, oauth2.HTTPClient, o.http), code, oauth2.VerifierOption(st.Verifier),
		oauth2.SetAuthURLParam("resource", spec.Discovered.Resource))
	if err != nil {
		return plugins.DescribeOAuthErr(err)
	}
	if tok.AccessToken == "" {
		return errors.New("the provider answered without a token")
	}
	_, err = o.store1(ctx, srv, tok)
	return err
}

// Authorize implements Credentials: an OAuth token about to expire is renewed first.
func (o *OAuth) Authorize(ctx context.Context, srv db.McpServer, h http.Header) error {
	if srv.Auth == AuthOAuth && srv.TokenExpiresAt.Valid && srv.TokenExpiresAt.Time.Before(o.now().Add(2*time.Minute)) {
		fresh, err := o.renew(ctx, srv)
		if err != nil {
			return err
		}
		srv = fresh
	}
	return o.keys.Authorize(ctx, srv, h)
}

// renew trades the refresh token for a new access token. Another request may have renewed it a
// moment ago: the server's row is read again under the lock.
func (o *OAuth) renew(ctx context.Context, srv db.McpServer) (db.McpServer, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cur, err := o.store.GetMCPServerIn(ctx, db.GetMCPServerInParams{ProjectID: srv.ProjectID, Name: srv.Name})
	if err != nil {
		return srv, err
	}
	if !cur.TokenExpiresAt.Valid || cur.TokenExpiresAt.Time.After(o.now().Add(2*time.Minute)) {
		return cur, nil
	}
	sec, err := o.keys.Secrets(cur)
	if err != nil {
		return cur, err
	}
	if sec[SecretRefreshToken] == "" {
		return cur, nil
	}
	cfg, _, err := o.config(cur)
	if err != nil {
		return cur, err
	}
	old := &oauth2.Token{RefreshToken: sec[SecretRefreshToken], Expiry: time.Unix(1, 0)}
	tok, err := cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, o.http), old).Token()
	if err != nil {
		return cur, plugins.DescribeOAuthErr(err)
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = old.RefreshToken
	}
	return o.store1(ctx, cur, tok)
}

// store1 keeps a token as the server's credential. Only a token that can be renewed gets an
// expiry; one that cannot simply stops working, and the proxy's calls say so.
func (o *OAuth) store1(ctx context.Context, srv db.McpServer, tok *oauth2.Token) (db.McpServer, error) {
	sec, err := o.keys.Secrets(srv)
	if err != nil {
		return srv, err
	}
	sec[SecretAccessToken] = tok.AccessToken
	delete(sec, SecretRefreshToken)
	if tok.RefreshToken != "" {
		sec[SecretRefreshToken] = tok.RefreshToken
	}
	enc, err := o.keys.Seal(sec)
	if err != nil {
		return srv, err
	}
	var exp pgtype.Timestamptz
	if tok.RefreshToken != "" && !tok.Expiry.IsZero() {
		exp = pgtype.Timestamptz{Time: tok.Expiry, Valid: true}
	}
	out, err := o.store.SetMCPServerSecrets(ctx, db.SetMCPServerSecretsParams{ID: srv.ID, Secrets: enc, TokenExpiresAt: exp})
	if err == nil && o.cache != nil {
		o.cache.Forget(srv.ID)
	}
	return out, err
}

// prepare follows the server's metadata to its authorization server and makes sure there is a
// client to sign in with: the provider's app where the operator allowed it at that host, else one
// registered for this server alone (kept encrypted in its secrets, reused while the
// authorization server stays the same).
func (o *OAuth) prepare(ctx context.Context, srv db.McpServer) (db.McpServer, error) {
	spec, err := specOf(srv)
	if err != nil {
		return srv, err
	}
	if o.publicURL == "" {
		return srv, fmt.Errorf("%w: set GATOR_PUBLIC_URL so the provider can send the browser back", ErrNotConnectable)
	}
	found, err := discover(ctx, o.http, srv.Url, spec.AuthorizationServer)
	if err != nil {
		return srv, err
	}
	moved := spec.Discovered == nil || spec.Discovered.Issuer != found.Issuer
	spec.Discovered = &found
	sec, err := o.keys.Secrets(srv)
	if err != nil {
		return srv, err
	}
	switch {
	case o.appAllowed(spec):
		spec.Client = "app"
	case !moved && spec.Client == "registered" && sec[SecretClientID] != "":
	case found.RegistrationEndpoint != "":
		c, err := register(ctx, o.http, found.RegistrationEndpoint, o.redirect())
		if err != nil {
			return srv, err
		}
		sec[SecretClientID], sec[SecretClientSecret] = c.ClientID, c.ClientSecret
		if c.ClientSecret == "" {
			delete(sec, SecretClientSecret)
		}
		enc, err := o.keys.Seal(sec)
		if err != nil {
			return srv, err
		}
		if srv, err = o.store.SetMCPServerSecrets(ctx, db.SetMCPServerSecretsParams{ID: srv.ID, Secrets: enc, TokenExpiresAt: srv.TokenExpiresAt}); err != nil {
			return srv, err
		}
		spec.Client = "registered"
	default:
		return srv, fmt.Errorf("%w: %s registers no clients, and this server has no app the operator allowed there (GATOR_<PROVIDER>_OAUTH_HOSTS)",
			ErrNotConnectable, hostOf(found.Issuer))
	}
	raw, _ := json.Marshal(spec)
	return o.store.SetMCPServerOAuth(ctx, db.SetMCPServerOAuthParams{ID: srv.ID, Oauth: raw})
}

// appAllowed says whether the provider's app may sign in at the server's authorization server.
func (o *OAuth) appAllowed(spec oauthSpec) bool {
	if spec.Provider == "" || spec.Discovered == nil {
		return false
	}
	app, ok := o.apps(spec.Provider)
	host := hostOf(spec.Discovered.Issuer)
	return ok && slices.ContainsFunc(app.Hosts, func(h string) bool { return strings.EqualFold(h, host) })
}

// config is the OAuth flow of a server, from what discovery found and with the client prepare chose.
func (o *OAuth) config(srv db.McpServer) (*oauth2.Config, oauthSpec, error) {
	spec, err := specOf(srv)
	if err != nil {
		return nil, spec, err
	}
	d := spec.Discovered
	if d == nil || spec.Client == "" {
		return nil, spec, fmt.Errorf("%w: %s has not been connected yet", ErrNotConnectable, srv.Name)
	}
	cfg := &oauth2.Config{RedirectURL: o.redirect(), Scopes: spec.Scopes,
		Endpoint: oauth2.Endpoint{AuthURL: d.AuthorizationEndpoint, TokenURL: d.TokenEndpoint, AuthStyle: oauth2.AuthStyleInParams}}
	switch spec.Client {
	case "app":
		if !o.appAllowed(spec) {
			return nil, spec, fmt.Errorf("%w: the %s app is no longer allowed at %s", ErrNotConnectable, spec.Provider, hostOf(d.Issuer))
		}
		app, _ := o.apps(spec.Provider)
		cfg.ClientID, cfg.ClientSecret = app.ID, app.Secret
		if len(app.Scopes) > 0 {
			cfg.Scopes = app.Scopes
		}
	default:
		sec, err := o.keys.Secrets(srv)
		if err != nil {
			return nil, spec, err
		}
		cfg.ClientID, cfg.ClientSecret = sec[SecretClientID], sec[SecretClientSecret]
	}
	return cfg, spec, nil
}

func specOf(srv db.McpServer) (oauthSpec, error) {
	var spec oauthSpec
	_ = json.Unmarshal(srv.Oauth, &spec)
	if srv.Auth != AuthOAuth {
		return spec, fmt.Errorf("%w: %s does not sign in with OAuth", ErrNotConnectable, srv.Name)
	}
	return spec, nil
}

func (o *OAuth) redirect() string { return o.publicURL + "/connect/callback" }

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
