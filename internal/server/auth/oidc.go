package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/onegator/gator/internal/server/store/db"
)

// SessionCookie carries the user's session token (a user-kind bearer token) for browser
// clients such as /admin. Native clients send Authorization: Bearer instead.
const SessionCookie = "gator_session"

const (
	sessionTTL = 30 * 24 * time.Hour
	// NativeTokenTTL is how long a token handed to Gator.app or the CLI lives. Signing in
	// again is one click, so it is bounded rather than forever.
	NativeTokenTTL = 90 * 24 * time.Hour
	loginCodeTTL   = 2 * time.Minute
	flowCookie     = "gator_oidc"
)

// OIDCConfig comes from GATOR_OIDC_* variables. Empty issuer disables OIDC.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	SecureCookie bool
	// ProviderName is what the sign-in button says, e.g. "Google".
	ProviderName string
	// AllowedDomains limits who may create an account by the domain of their verified email.
	// Empty lets in anyone the provider vouches for. People who already have an account (an
	// admin created on the host, say) sign in whatever their domain.
	AllowedDomains []string
	// AdminEmails become workspace admins when they sign in. When empty, the first person to
	// sign in becomes admin, which is a race on a server reachable before its owner logs in.
	AdminEmails []string
}

// LoadOIDCConfig reads the environment.
func LoadOIDCConfig() OIDCConfig {
	c := OIDCConfig{
		Issuer:         os.Getenv("GATOR_OIDC_ISSUER"),
		ClientID:       os.Getenv("GATOR_OIDC_CLIENT_ID"),
		ClientSecret:   os.Getenv("GATOR_OIDC_CLIENT_SECRET"),
		RedirectURL:    os.Getenv("GATOR_OIDC_REDIRECT_URL"),
		SecureCookie:   os.Getenv("GATOR_INSECURE_COOKIES") != "1",
		ProviderName:   os.Getenv("GATOR_OIDC_PROVIDER_NAME"),
		AllowedDomains: splitList(os.Getenv("GATOR_OIDC_ALLOWED_DOMAINS")),
		AdminEmails:    splitList(os.Getenv("GATOR_ADMIN_EMAILS")),
	}
	if c.ProviderName == "" {
		c.ProviderName = providerName(c.Issuer)
	}
	return c
}

// Enabled reports whether OIDC is configured.
func (c OIDCConfig) Enabled() bool { return c.Issuer != "" && c.ClientID != "" }

func providerName(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return "single sign-on"
	}
	if u.Host == "accounts.google.com" {
		return "Google"
	}
	return u.Host
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.ToLower(strings.TrimSpace(part)); part != "" {
			out = append(out, part)
		}
	}
	return out
}

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

// ProviderName is what a client shows on its sign-in button.
func (o *OIDC) ProviderName() string { return o.cfg.ProviderName }

// loginFlow is what the browser carries from /auth/login to /auth/callback, in an httpOnly
// cookie: the values that bind the provider's answer to this browser and, for a native
// client, where to send it back to.
type loginFlow struct {
	State    string         `json:"s"`
	Nonce    string         `json:"n"`
	Verifier string         `json:"v"`
	Native   *nativeRequest `json:"a,omitempty"`
}

// nativeRequest is a native client's half of the flow. The client proves it started the
// login by presenting the verifier behind Challenge when it trades the code for a token.
type nativeRequest struct {
	RedirectURI string `json:"r"`
	State       string `json:"s"`
	Challenge   string `json:"c"`
}

// NativeRedirectAllowed says whether a native client may be sent back to uri: the app's own
// scheme, or a loopback address the CLI listens on (RFC 8252). Anything else would hand a
// login code to whoever owns that address.
func NativeRedirectAllowed(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "gator":
		return u.Host == "auth"
	case "http":
		ip := net.ParseIP(u.Hostname())
		return (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) && u.Port() != ""
	}
	return false
}

// Login starts the authorization code flow. A native client adds redirect_uri, state and an
// S256 code_challenge; a browser adds nothing and ends up with a session cookie.
func (o *OIDC) Login(w http.ResponseWriter, r *http.Request) {
	flow := loginFlow{State: randomString(), Nonce: randomString(), Verifier: oauth2.GenerateVerifier()}
	if q := r.URL.Query(); q.Get("redirect_uri") != "" {
		native := &nativeRequest{RedirectURI: q.Get("redirect_uri"), State: q.Get("state"), Challenge: q.Get("code_challenge")}
		if !NativeRedirectAllowed(native.RedirectURI) {
			http.Error(w, "redirect_uri is not a native client address", http.StatusBadRequest)
			return
		}
		if q.Get("code_challenge_method") != "S256" || len(native.Challenge) < 43 || native.State == "" {
			http.Error(w, "a native client must send state and an S256 code_challenge", http.StatusBadRequest)
			return
		}
		flow.Native = native
	}
	raw, _ := json.Marshal(flow)
	http.SetCookie(w, &http.Cookie{
		Name: flowCookie, Value: base64.RawURLEncoding.EncodeToString(raw), Path: "/auth/",
		HttpOnly: true, Secure: o.cfg.SecureCookie, MaxAge: 600, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, o.oauth.AuthCodeURL(flow.State, oauth2.S256ChallengeOption(flow.Verifier), oidc.Nonce(flow.Nonce)), http.StatusFound)
}

func readFlow(r *http.Request) (loginFlow, bool) {
	c, err := r.Cookie(flowCookie)
	if err != nil {
		return loginFlow{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return loginFlow{}, false
	}
	var f loginFlow
	if json.Unmarshal(raw, &f) != nil || f.State == "" {
		return loginFlow{}, false
	}
	return f, true
}

// Callback exchanges the code and resolves the user. A browser gets a session cookie; a
// native client is sent back to its redirect_uri with a one-time code.
func (o *OIDC) Callback(w http.ResponseWriter, r *http.Request) {
	flow, ok := readFlow(r)
	if !ok || subtle.ConstantTimeCompare([]byte(flow.State), []byte(r.URL.Query().Get("state"))) != 1 {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: "", Path: "/auth/", MaxAge: -1})
	fail := func(status int, msg string) {
		if flow.Native != nil {
			redirectNative(w, r, flow.Native, url.Values{"error": {msg}})
			return
		}
		http.Error(w, msg, status)
	}
	if e := r.URL.Query().Get("error"); e != "" {
		// The person closed the consent screen, or the provider refused.
		fail(http.StatusUnauthorized, "sign-in was not completed: "+e)
		return
	}
	user, err := o.resolve(r.Context(), r.URL.Query().Get("code"), flow)
	var refused refusal
	switch {
	case errors.As(err, &refused):
		fail(refused.status, refused.msg)
		return
	case err != nil:
		fail(http.StatusInternalServerError, "sign-in failed")
		return
	}
	if flow.Native != nil {
		code, err := o.issueLoginCode(r.Context(), user.ID, flow.Native)
		if err != nil {
			fail(http.StatusInternalServerError, "sign-in failed")
			return
		}
		redirectNative(w, r, flow.Native, url.Values{"code": {code}})
		return
	}
	session, _, err := o.tokens.Issue(r.Context(), IssueParams{Kind: KindUser, Scope: "user", Name: "session", UserID: user.ID, TTL: sessionTTL})
	if err != nil {
		http.Error(w, "session failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: session, Path: "/", HttpOnly: true, Secure: o.cfg.SecureCookie, MaxAge: int(sessionTTL.Seconds()), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

// refusal is a sign-in the server understood and declined; its message is for the person.
type refusal struct {
	status int
	msg    string
}

func (r refusal) Error() string { return r.msg }

// resolve trades the provider's code for a verified identity and the user it belongs to.
func (o *OIDC) resolve(ctx context.Context, code string, flow loginFlow) (db.User, error) {
	tok, err := o.oauth.Exchange(ctx, code, oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		return db.User{}, refusal{http.StatusBadGateway, "the identity provider did not accept the sign-in"}
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		return db.User{}, refusal{http.StatusBadGateway, "the identity provider sent no id_token"}
	}
	idt, err := o.verifier.Verify(ctx, rawID)
	if err != nil || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(flow.Nonce)) != 1 {
		return db.User{}, refusal{http.StatusUnauthorized, "invalid id_token"}
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idt.Claims(&claims); err != nil || claims.Email == "" {
		return db.User{}, refusal{http.StatusUnauthorized, "the identity provider sent no email"}
	}
	// Accounts link by email, so an unverified email must never be trusted.
	if claims.EmailVerified != nil && !*claims.EmailVerified {
		return db.User{}, refusal{http.StatusUnauthorized, "email not verified by the identity provider"}
	}
	if claims.Name == "" {
		claims.Name = claims.Email
	}
	user, err := o.upsertUser(ctx, claims.Email, claims.Name, idt.Subject)
	if errors.Is(err, ErrIdentityConflict) {
		return db.User{}, refusal{http.StatusConflict, "this email is already linked to another identity"}
	}
	return user, err
}

// upsertUser resolves the login to a user, creating one on first sign-in.
func (o *OIDC) upsertUser(ctx context.Context, email, name, subject string) (db.User, error) {
	q := db.New(o.pool)
	if !o.mayJoin(ctx, q, email) {
		return db.User{}, refusal{http.StatusForbidden, email + " may not sign in to this workspace"}
	}
	u, err := LinkOIDCUser(ctx, q, email, name, subject)
	if err != nil {
		return db.User{}, err
	}
	return grantAdmin(ctx, q, o.cfg.AdminEmails, u)
}

// mayJoin applies AllowedDomains to people the server has not seen yet.
func (o *OIDC) mayJoin(ctx context.Context, q *db.Queries, email string) bool {
	if len(o.cfg.AllowedDomains) == 0 {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at >= 0 && containsFold(o.cfg.AllowedDomains, email[at+1:]) {
		return true
	}
	_, err := q.GetUserByEmail(ctx, email)
	return err == nil
}

// grantAdmin makes the named admins admins or, with none named, the first person in.
func grantAdmin(ctx context.Context, q *db.Queries, adminEmails []string, u db.User) (db.User, error) {
	if u.WorkspaceRole == "admin" {
		return u, nil
	}
	if len(adminEmails) > 0 {
		if !containsFold(adminEmails, u.Email) {
			return u, nil
		}
		if err := q.SetWorkspaceRole(ctx, db.SetWorkspaceRoleParams{ID: u.ID, WorkspaceRole: "admin"}); err != nil {
			return db.User{}, err
		}
		u.WorkspaceRole = "admin"
		return u, nil
	}
	promoted, err := q.PromoteIfNoAdmin(ctx, u.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, nil
	}
	return promoted, err
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

func (o *OIDC) issueLoginCode(ctx context.Context, userID pgtype.UUID, n *nativeRequest) (string, error) {
	return IssueLoginCode(ctx, db.New(o.pool), userID, n.Challenge, n.RedirectURI)
}

// IssueLoginCode leaves a one-time code for a native client to trade at /auth/token.
func IssueLoginCode(ctx context.Context, q *db.Queries, userID pgtype.UUID, challenge, redirectURI string) (string, error) {
	_ = q.DeleteExpiredLoginCodes(ctx)
	code := randomString()
	err := q.InsertLoginCode(ctx, db.InsertLoginCodeParams{
		Hash: hash(code), UserID: userID, CodeChallenge: challenge, RedirectUri: redirectURI,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(loginCodeTTL), Valid: true},
	})
	return code, err
}

func redirectNative(w http.ResponseWriter, r *http.Request, n *nativeRequest, v url.Values) {
	u, _ := url.Parse(n.RedirectURI)
	q := u.Query()
	for k, vals := range v {
		q[k] = vals
	}
	q.Set("state", n.State)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// TokenRequest is what a native client sends to trade its login code for a token.
type TokenRequest struct {
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
	RedirectURI  string `json:"redirect_uri"`
	// ClientName labels the token in the person's list of tokens, e.g. "Gator.app on mati-mbp".
	ClientName string `json:"client_name"`
}

// TokenResponse carries the new token. It is shown once.
type TokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ExchangeLoginCode trades a login code for a user token. The code is spent whatever the
// outcome, so a code seen by anyone else is worthless without the verifier only the client holds.
func ExchangeLoginCode(ctx context.Context, tokens Tokens, req TokenRequest) (TokenResponse, error) {
	lc, err := db.New(tokens.Pool).TakeLoginCode(ctx, hash(req.Code))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenResponse{}, refusal{http.StatusBadRequest, "the login code is unknown or already used"}
	}
	if err != nil {
		return TokenResponse{}, err
	}
	sum := sha256.Sum256([]byte(req.CodeVerifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	switch {
	case !lc.ExpiresAt.Valid || time.Now().After(lc.ExpiresAt.Time):
		return TokenResponse{}, refusal{http.StatusBadRequest, "the login code has expired; sign in again"}
	case lc.RedirectUri != req.RedirectURI:
		return TokenResponse{}, refusal{http.StatusBadRequest, "redirect_uri does not match the sign-in"}
	case subtle.ConstantTimeCompare([]byte(challenge), []byte(lc.CodeChallenge)) != 1:
		return TokenResponse{}, refusal{http.StatusBadRequest, "code_verifier does not match the sign-in"}
	}
	name := strings.TrimSpace(req.ClientName)
	if name == "" {
		name = "native"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	token, rec, err := tokens.Issue(ctx, IssueParams{Kind: KindUser, Scope: "user", Name: name, UserID: lc.UserID, TTL: NativeTokenTTL})
	if err != nil {
		return TokenResponse{}, err
	}
	return TokenResponse{Token: token, ExpiresAt: rec.ExpiresAt.Time}, nil
}

// Token is the HTTP face of ExchangeLoginCode: POST /auth/token.
func (o *OIDC) Token(w http.ResponseWriter, r *http.Request) {
	var req TokenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Code == "" || req.CodeVerifier == "" {
		writeErr(w, http.StatusBadRequest, "code and code_verifier are required")
		return
	}
	resp, err := ExchangeLoginCode(r.Context(), o.tokens, req)
	var refused refusal
	switch {
	case errors.As(err, &refused):
		writeErr(w, refused.status, refused.msg)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "sign-in failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
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
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ErrOIDCDisabled is returned by handlers when OIDC is not configured.
var ErrOIDCDisabled = errors.New("oidc not configured")
