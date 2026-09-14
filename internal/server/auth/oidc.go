package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/onegator/gator/internal/server/store/db"
)

// SessionCookie carries the user's session token (a user-kind bearer token) for browser
// clients such as /admin. Native clients send Authorization: Bearer instead.
const SessionCookie = "gator_session"

const sessionTTL = 30 * 24 * time.Hour

// OIDCConfig comes from GATOR_OIDC_* variables. Empty issuer disables OIDC.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	SecureCookie bool
}

// LoadOIDCConfig reads the environment.
func LoadOIDCConfig() OIDCConfig {
	return OIDCConfig{
		Issuer:       os.Getenv("GATOR_OIDC_ISSUER"),
		ClientID:     os.Getenv("GATOR_OIDC_CLIENT_ID"),
		ClientSecret: os.Getenv("GATOR_OIDC_CLIENT_SECRET"),
		RedirectURL:  os.Getenv("GATOR_OIDC_REDIRECT_URL"),
		SecureCookie: os.Getenv("GATOR_INSECURE_COOKIES") != "1",
	}
}

// Enabled reports whether OIDC is configured.
func (c OIDCConfig) Enabled() bool { return c.Issuer != "" && c.ClientID != "" }

// OIDC implements the login and callback handlers.
type OIDC struct {
	cfg      OIDCConfig
	pool     *pgxpool.Pool
	tokens   Tokens
	provider *oidc.Provider
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewOIDC discovers the provider. Call only when cfg.Enabled().
func NewOIDC(ctx context.Context, cfg OIDCConfig, pool *pgxpool.Pool) (*OIDC, error) {
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	return &OIDC{
		cfg: cfg, pool: pool, tokens: Tokens{Pool: pool}, provider: provider,
		oauth: oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
			Endpoint: provider.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

// Login starts the authorization code flow.
func (o *OIDC) Login(w http.ResponseWriter, r *http.Request) {
	state := randomString()
	http.SetCookie(w, &http.Cookie{Name: "gator_oidc_state", Value: state, Path: "/", HttpOnly: true, Secure: o.cfg.SecureCookie, MaxAge: 600, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, o.oauth.AuthCodeURL(state), http.StatusFound)
}

// Callback exchanges the code, upserts the user and sets the session cookie.
func (o *OIDC) Callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("gator_oidc_state")
	if err != nil || c.Value == "" || c.Value != r.URL.Query().Get("state") {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	tok, err := o.oauth.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "exchange failed", http.StatusBadGateway)
		return
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		http.Error(w, "no id_token", http.StatusBadGateway)
		return
	}
	idt, err := o.verifier.Verify(r.Context(), rawID)
	if err != nil {
		http.Error(w, "invalid id_token", http.StatusUnauthorized)
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idt.Claims(&claims); err != nil || claims.Email == "" {
		http.Error(w, "missing email claim", http.StatusUnauthorized)
		return
	}
	// Accounts link by email, so an unverified email must never be trusted.
	if claims.EmailVerified != nil && !*claims.EmailVerified {
		http.Error(w, "email not verified by the identity provider", http.StatusUnauthorized)
		return
	}
	if claims.Name == "" {
		claims.Name = claims.Email
	}
	user, err := o.upsertUser(r.Context(), claims.Email, claims.Name, idt.Subject)
	if errors.Is(err, ErrIdentityConflict) {
		http.Error(w, "this email is already linked to another identity", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "user upsert failed", http.StatusInternalServerError)
		return
	}
	session, _, err := o.tokens.Issue(r.Context(), IssueParams{Kind: KindUser, Scope: "user", Name: "session", UserID: user.ID, TTL: sessionTTL})
	if err != nil {
		http.Error(w, "session failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: session, Path: "/", HttpOnly: true, Secure: o.cfg.SecureCookie, MaxAge: int(sessionTTL.Seconds()), SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: "gator_oidc_state", Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

// upsertUser resolves the login to a user. The very first user becomes workspace admin.
func (o *OIDC) upsertUser(ctx context.Context, email, name, subject string) (db.User, error) {
	q := db.New(o.pool)
	n, err := q.CountUsers(ctx)
	if err != nil {
		return db.User{}, err
	}
	u, err := LinkOIDCUser(ctx, q, email, name, subject)
	if err != nil {
		return db.User{}, err
	}
	if n == 0 && u.WorkspaceRole != "admin" {
		if err := q.SetWorkspaceRole(ctx, db.SetWorkspaceRoleParams{ID: u.ID, WorkspaceRole: "admin"}); err != nil {
			return db.User{}, err
		}
		u.WorkspaceRole = "admin"
	}
	return u, nil
}

// Logout revokes the session token and clears the cookie.
func Logout(tokens Tokens) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p, ok := FromContext(r.Context()); ok && p.TokenID.Valid {
			_ = tokens.Revoke(r.Context(), p.TokenID)
		}
		http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1})
		w.WriteHeader(http.StatusNoContent)
	}
}

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ErrOIDCDisabled is returned by handlers when OIDC is not configured.
var ErrOIDCDisabled = errors.New("oidc not configured")

var _ = pgtype.UUID{}
