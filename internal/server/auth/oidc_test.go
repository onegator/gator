package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"

	"github.com/onegator/gator/internal/server/invites"
)

func TestNativeRedirectAllowed(t *testing.T) {
	for uri, want := range map[string]bool{
		"gator://auth/callback":           true,
		"gator://auth":                    true,
		"http://127.0.0.1:53682/callback": true,
		"http://localhost:53682/callback": true,
		"http://[::1]:53682/callback":     true,
		"gator://evil/callback":           false,
		"http://127.0.0.1/callback":       false, // no port: not a listener the CLI owns
		"http://example.com:80/callback":  false,
		"https://example.com/callback":    false,
		"gator://auth/callback#x":         false,
		"http://u:p@127.0.0.1:1/":         false,
		"javascript:alert(1)":             false,
	} {
		if got := NativeRedirectAllowed(uri); got != want {
			t.Errorf("NativeRedirectAllowed(%q) = %v, want %v", uri, got, want)
		}
	}
}

// fakeProvider is an OIDC issuer that signs whatever identity the test sets, and checks the
// PKCE verifier the server sends upstream against the challenge it saw at /authorize.
type fakeProvider struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu         sync.Mutex
	challenges map[string]string // code -> challenge
	nonces     map[string]string // code -> nonce
	email      string
	verified   bool
	badNonce   bool
}

func newFakeProvider(t *testing.T) *fakeProvider {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{key: key, challenges: map[string]string{}, nonces: map[string]string{}, verified: true}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/authorize",
			"token_endpoint": p.srv.URL + "/token", "jwks_uri": p.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code := r.PostForm.Get("code")
		p.mu.Lock()
		challenge, nonce, email, verified, badNonce := p.challenges[code], p.nonces[code], p.email, p.verified, p.badNonce
		p.mu.Unlock()
		if challenge == "" || oauth2.S256ChallengeFromVerifier(r.PostForm.Get("code_verifier")) != challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		if badNonce {
			nonce = "someone-else's"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": p.sign(t, map[string]any{
				"iss": p.srv.URL, "aud": "gator", "sub": "sub-" + email, "email": email, "email_verified": verified,
				"name": "Test Person", "nonce": nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
			}),
		})
	})
	return p
}

func (p *fakeProvider) sign(t *testing.T, claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// authorize plays the person consenting: it reads what Login sent the browser to and
// answers with a code, the way the provider would redirect back.
func (p *fakeProvider) authorize(t *testing.T, location string) (code, state string) {
	u, err := url.Parse(location)
	if err != nil || !strings.HasPrefix(location, p.srv.URL+"/authorize") {
		t.Fatalf("login did not send the browser to the provider: %q", location)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" {
		t.Fatalf("upstream request lacks PKCE or nonce: %v", q)
	}
	code = randomString()
	p.mu.Lock()
	p.challenges[code], p.nonces[code] = q.Get("code_challenge"), q.Get("nonce")
	p.mu.Unlock()
	return code, q.Get("state")
}

type loginHarness struct {
	t        *testing.T
	o        *OIDC
	provider *fakeProvider
}

func newLoginHarness(t *testing.T, cfg OIDCConfig) *loginHarness {
	pool := testPool(t)
	p := newFakeProvider(t)
	cfg.Issuer, cfg.ClientID, cfg.RedirectURL = p.srv.URL, "gator", "https://gator.test/auth/callback"
	o, err := NewOIDC(context.Background(), cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	return &loginHarness{t: t, o: o, provider: p}
}

// run goes from /auth/login to the callback's answer, carrying the flow cookie between them.
func (h *loginHarness) run(loginQuery url.Values) *http.Response {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.o.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login?"+loginQuery.Encode(), nil))
	if rec.Code != http.StatusFound {
		h.t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	code, state := h.provider.authorize(h.t, rec.Header().Get("Location"))
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?"+url.Values{"code": {code}, "state": {state}}.Encode(), nil)
	for _, c := range rec.Result().Cookies() {
		cb.AddCookie(c)
	}
	out := httptest.NewRecorder()
	h.o.Callback(out, cb)
	return out.Result()
}

func pkce() (verifier, challenge string) {
	verifier = oauth2.GenerateVerifier()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func uniqueEmail(domain string) string {
	return fmt.Sprintf("login-%d@%s", time.Now().UnixNano(), domain)
}

func TestNativeLoginTradesACodeForAToken(t *testing.T) {
	h := newLoginHarness(t, OIDCConfig{})
	h.provider.email = uniqueEmail("t.local")
	verifier, challenge := pkce()
	const redirect = "gator://auth/callback"
	resp := h.run(url.Values{"redirect_uri": {redirect}, "state": {"app-state"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}})
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback: %d", resp.StatusCode)
	}
	back, _ := url.Parse(resp.Header.Get("Location"))
	if back.Scheme != "gator" || back.Query().Get("state") != "app-state" || back.Query().Get("code") == "" {
		t.Fatalf("not sent back to the app: %s", back)
	}
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookie {
			t.Fatal("a native login must not leave a browser session behind")
		}
	}
	code := back.Query().Get("code")
	ctx := context.Background()

	if _, err := ExchangeLoginCode(ctx, h.o.tokens, TokenRequest{Code: code, CodeVerifier: "wrong" + verifier, RedirectURI: redirect}); err == nil {
		t.Fatal("a wrong verifier must not buy a token")
	}
	// The failed attempt spent the code: whoever saw it cannot keep guessing.
	if _, err := ExchangeLoginCode(ctx, h.o.tokens, TokenRequest{Code: code, CodeVerifier: verifier, RedirectURI: redirect}); err == nil {
		t.Fatal("a code must be good for one attempt")
	}

	// A fresh login, done right.
	verifier, challenge = pkce()
	resp = h.run(url.Values{"redirect_uri": {redirect}, "state": {"s2"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}})
	back, _ = url.Parse(resp.Header.Get("Location"))
	got, err := ExchangeLoginCode(ctx, h.o.tokens, TokenRequest{Code: back.Query().Get("code"), CodeVerifier: verifier, RedirectURI: redirect, ClientName: "Gator.app"})
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(got.ExpiresAt) < NativeTokenTTL-time.Minute {
		t.Fatalf("native token expiry: %v", got.ExpiresAt)
	}
	p, err := h.o.tokens.Verify(ctx, got.Token)
	if err != nil || p.Kind != KindUser || p.Name != "Test Person" {
		t.Fatalf("token does not act as the person: %+v %v", p, err)
	}
}

func TestNativeLoginRefusesStrangeRequests(t *testing.T) {
	h := newLoginHarness(t, OIDCConfig{})
	_, challenge := pkce()
	for name, q := range map[string]url.Values{
		"foreign redirect": {"redirect_uri": {"https://evil.example/cb"}, "state": {"s"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}},
		"plain pkce":       {"redirect_uri": {"gator://auth/callback"}, "state": {"s"}, "code_challenge": {challenge}, "code_challenge_method": {"plain"}},
		"no state":         {"redirect_uri": {"gator://auth/callback"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}},
	} {
		rec := httptest.NewRecorder()
		h.o.Login(rec, httptest.NewRequest(http.MethodGet, "/auth/login?"+q.Encode(), nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, rec.Code)
		}
	}
}

func TestLoginChecksNonceAndDomain(t *testing.T) {
	h := newLoginHarness(t, OIDCConfig{AllowedDomains: []string{"allowed.test"}})
	_, challenge := pkce()
	native := url.Values{"redirect_uri": {"gator://auth/callback"}, "state": {"s"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	errorOf := func(resp *http.Response) string {
		u, _ := url.Parse(resp.Header.Get("Location"))
		return u.Query().Get("error")
	}

	h.provider.email = uniqueEmail("elsewhere.test")
	if e := errorOf(h.run(native)); !strings.Contains(e, "may not sign in") {
		t.Fatalf("a stranger's domain was let in: %q", e)
	}

	h.provider.email = uniqueEmail("allowed.test")
	h.provider.badNonce = true
	if e := errorOf(h.run(native)); e != "invalid id_token" {
		t.Fatalf("an id_token minted for another login was accepted: %q", e)
	}

	h.provider.badNonce = false
	h.provider.verified = false
	if e := errorOf(h.run(native)); !strings.Contains(e, "not verified") {
		t.Fatalf("an unverified email was accepted: %q", e)
	}

	h.provider.verified = true
	if e := errorOf(h.run(native)); e != "" {
		t.Fatalf("an allowed person was refused: %q", e)
	}
}

func TestBrowserLoginStillSetsASession(t *testing.T) {
	h := newLoginHarness(t, OIDCConfig{})
	h.provider.email = uniqueEmail("t.local")
	resp := h.run(url.Values{})
	if resp.Header.Get("Location") != "/admin/" {
		t.Fatalf("browser login went to %q", resp.Header.Get("Location"))
	}
	var session string
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookie {
			session = c.Value
		}
	}
	if _, err := h.o.tokens.Verify(context.Background(), session); err != nil {
		t.Fatalf("session cookie: %v", err)
	}
}

func TestAdminEmailsBecomeAdmins(t *testing.T) {
	boss := uniqueEmail("t.local")
	h := newLoginHarness(t, OIDCConfig{AdminEmails: []string{strings.ToUpper(boss)}})
	ctx := context.Background()
	u, err := h.o.upsertUser(ctx, boss, "Boss", "sub-"+boss)
	if err != nil || u.WorkspaceRole != "admin" {
		t.Fatalf("named admin: %+v %v", u, err)
	}
	other := uniqueEmail("t.local")
	u, err = h.o.upsertUser(ctx, other, "Other", "sub-"+other)
	if err != nil || u.WorkspaceRole != "member" {
		t.Fatalf("with admins named, nobody else is promoted: %+v %v", u, err)
	}
}

func TestStaleSessionCookieIsAnonymousNotRefused(t *testing.T) {
	tokens := Tokens{Pool: testPool(t)}
	var sawPrincipal bool
	h := Authenticate(tokens, false, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawPrincipal = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "gtr_user_expired"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || sawPrincipal {
		t.Fatalf("stale cookie: %d principal=%v", rec.Code, sawPrincipal)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer gtr_user_expired")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a bad bearer token must still be refused, got %d", rec.Code)
	}
}

func TestSigningInWithAnInvitedAddressJoinsTheProject(t *testing.T) {
	// The domain list would refuse this person; the invitation lets them in.
	h := newLoginHarness(t, OIDCConfig{AllowedDomains: []string{"allowed.test"}})
	ctx := context.Background()
	pool := h.o.pool
	email := uniqueEmail("elsewhere.test")
	var project string
	if err := pool.QueryRow(ctx, "INSERT INTO projects(slug,name) VALUES($1,'Invited') RETURNING id", "inv"+randomString()[:12]).Scan(&project); err != nil {
		t.Fatal(err)
	}
	_, tokenHash := invites.NewToken()
	if _, err := pool.Exec(ctx, "INSERT INTO project_invitations(project_id,email,role,token_hash,expires_at) VALUES($1,$2,'member',$3,now()+interval '1 day')",
		project, strings.ToUpper(email), tokenHash); err != nil {
		t.Fatal(err)
	}

	h.provider.email = email
	_, challenge := pkce()
	resp := h.run(url.Values{"redirect_uri": {"gator://auth/callback"}, "state": {"s"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}})
	if back, _ := url.Parse(resp.Header.Get("Location")); back.Query().Get("error") != "" || back.Query().Get("code") == "" {
		t.Fatalf("the invited person was refused: %s", back)
	}
	var role string
	var accepted bool
	if err := pool.QueryRow(ctx, `SELECT m.role, i.accepted_at IS NOT NULL FROM memberships m
		JOIN users u ON u.id = m.user_id JOIN project_invitations i ON i.project_id = m.project_id
		WHERE m.project_id = $1 AND u.email = $2`, project, email).Scan(&role, &accepted); err != nil {
		t.Fatalf("no membership after signing in: %v", err)
	}
	if role != "member" || !accepted {
		t.Fatalf("role %q accepted %v", role, accepted)
	}

	// Once taken, the invitation no longer opens the door to anyone else at that domain.
	h.provider.email = uniqueEmail("elsewhere.test")
	resp = h.run(url.Values{"redirect_uri": {"gator://auth/callback"}, "state": {"s"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}})
	if back, _ := url.Parse(resp.Header.Get("Location")); !strings.Contains(back.Query().Get("error"), "may not sign in") {
		t.Fatalf("an uninvited stranger: %s", back)
	}
}
