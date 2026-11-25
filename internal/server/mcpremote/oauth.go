package mcpremote

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
// _CLIENT_SECRET, as plugins have).
type OAuthApp struct {
	ID, Secret string
	Scopes     []string // replace the server's when set
}

// oauthSpec is mcp_servers.oauth, as the integration package wrote it.
type oauthSpec struct {
	Provider     string   `json:"provider"`
	AuthorizeURL string   `json:"authorize_url"`
	TokenURL     string   `json:"token_url"`
	Scopes       []string `json:"scopes"`
}

// oauthStore is what connecting reads and writes. *db.Queries is one.
type oauthStore interface {
	GetMCPServerIn(ctx context.Context, arg db.GetMCPServerInParams) (db.McpServer, error)
	SetMCPServerSecrets(ctx context.Context, arg db.SetMCPServerSecretsParams) (db.McpServer, error)
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
	cfg, _, err := o.config(srv)
	if err != nil {
		return "", err
	}
	_ = o.store.DeleteExpiredPluginConnectStates(ctx)
	state, verifier := randomToken(), oauth2.GenerateVerifier()
	sum := sha256.Sum256([]byte(state))
	if err := o.store.InsertPluginConnectState(ctx, db.InsertPluginConnectStateParams{Hash: sum[:], ProjectID: project, PluginName: name,
		UserID: p.UserID, Verifier: verifier, ReturnTo: returnTo, Target: plugins.TargetMCP,
		ExpiresAt: pgtype.Timestamptz{Time: o.now().Add(connectTTL), Valid: true}}); err != nil {
		return "", err
	}
	return cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), nil
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
	cfg, _, err := o.config(srv)
	if err != nil {
		return err
	}
	tok, err := cfg.Exchange(context.WithValue(ctx, oauth2.HTTPClient, o.http), code, oauth2.VerifierOption(st.Verifier))
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

// config is the OAuth flow of a server, with this server's app for its provider.
func (o *OAuth) config(srv db.McpServer) (*oauth2.Config, oauthSpec, error) {
	var spec oauthSpec
	_ = json.Unmarshal(srv.Oauth, &spec)
	if srv.Auth != AuthOAuth || spec.Provider == "" || spec.AuthorizeURL == "" || spec.TokenURL == "" {
		return nil, spec, fmt.Errorf("%w: %s does not sign in with OAuth", ErrNotConnectable, srv.Name)
	}
	app, ok := o.apps(spec.Provider)
	if !ok {
		env := "GATOR_" + strings.ToUpper(strings.ReplaceAll(spec.Provider, "-", "_"))
		return nil, spec, fmt.Errorf("%w: this server has no %s OAuth app; set %s_CLIENT_ID and %s_CLIENT_SECRET", ErrNotConnectable, spec.Provider, env, env)
	}
	if o.publicURL == "" {
		return nil, spec, fmt.Errorf("%w: set GATOR_PUBLIC_URL so the provider can send the browser back", ErrNotConnectable)
	}
	scopes := spec.Scopes
	if len(app.Scopes) > 0 {
		scopes = app.Scopes
	}
	return &oauth2.Config{ClientID: app.ID, ClientSecret: app.Secret, RedirectURL: o.publicURL + "/connect/callback", Scopes: scopes,
		Endpoint: oauth2.Endpoint{AuthURL: spec.AuthorizeURL, TokenURL: spec.TokenURL, AuthStyle: oauth2.AuthStyleInParams}}, spec, nil
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
