package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/marketplace/marketplacetest"
)

// ownAuthServer makes a fake MCP server its own authorization server, as hosted MCP servers are:
// protected resource metadata (RFC 9728), authorization server metadata (RFC 8414), dynamic
// client registration (RFC 7591), and a token endpoint that checks PKCE, the client and the
// resource. It writes down every form it is sent, to tell whose secrets reached it.
type ownAuthServer struct {
	*fakeIssues
	mu         sync.Mutex
	clients    map[string]string // id → secret
	codes      map[string]string // code → PKCE challenge
	registered int
	forms      []url.Values
	// issuer is the authorization server the resource metadata names (default: itself).
	issuer string
}

func newOwnAuthServer(t *testing.T) *ownAuthServer {
	t.Helper()
	a := &ownAuthServer{fakeIssues: newFakeIssues(t, "dyn-access-2"), clients: map[string]string{}, codes: map[string]string{}}
	reply := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	a.mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		issuer := a.issuer
		if issuer == "" {
			issuer = a.URL
		}
		reply(w, 200, map[string]any{"resource": a.URL + "/mcp", "authorization_servers": []string{issuer}})
	})
	a.mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, map[string]any{"issuer": a.URL, "authorization_endpoint": a.URL + "/authorize", "token_endpoint": a.URL + "/token",
			"registration_endpoint": a.URL + "/register", "code_challenge_methods_supported": []string{"S256"}})
	})
	a.mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		a.mu.Lock()
		defer a.mu.Unlock()
		if !slices.Equal(in.RedirectURIs, []string{fakePublicURL + "/connect/callback"}) {
			reply(w, 400, map[string]any{"error": "invalid_redirect_uri"})
			return
		}
		a.registered++
		id := fmt.Sprintf("dyn-%d", a.registered)
		a.clients[id] = id + "-secret"
		reply(w, 201, map[string]any{"client_id": id, "client_secret": id + "-secret"})
	})
	a.mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		a.mu.Lock()
		defer a.mu.Unlock()
		a.forms = append(a.forms, r.Form)
		if secret, ok := a.clients[r.Form.Get("client_id")]; !ok || secret != r.Form.Get("client_secret") {
			reply(w, 401, map[string]any{"error": "invalid_client"})
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			challenge, ok := a.codes[r.Form.Get("code")]
			delete(a.codes, r.Form.Get("code"))
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.Form.Get("resource") != a.URL+"/mcp" {
				reply(w, 400, map[string]any{"error": "invalid_grant"})
				return
			}
			reply(w, 200, map[string]any{"access_token": "dyn-access-1", "refresh_token": "dyn-refresh", "expires_in": 60, "token_type": "bearer"})
		case "refresh_token":
			reply(w, 200, map[string]any{"access_token": "dyn-access-2", "refresh_token": "dyn-refresh-2", "expires_in": 3600, "token_type": "bearer"})
		default:
			reply(w, 400, map[string]any{"error": "unsupported_grant_type"})
		}
	})
	return a
}

func (a *ownAuthServer) authorize(code, challenge string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.codes[code] = challenge
}

// oauthPackage publishes an OAuth integration (and the on-call agent given it) in a registry of
// the test's and installs both into a new project.
func (h *harness) oauthPackage(name, mcpURL, oauthBlock string) (project string, reg gen.Registry) {
	h.t.Helper()
	r := marketplacetest.New(h.t)
	r.Package("integrations/"+name, map[string]string{"integration.yaml": `name: ` + name + `
version: 1.0.0
display_name: Issues (OAuth)
description: Production errors, signed in with OAuth.
kind: mcp
url: ` + mcpURL + `
auth: oauth
` + oauthBlock + `tools_default_allow: [get_issue_details]
`})
	r.Package("agents/oncall-triage", map[string]string{"agent.yaml": strings.Replace(oncall["agent.yaml"], "mcp: [issues-mcp]", "mcp: ["+name+"]", 1),
		"instructions.md": oncall["instructions.md"]})
	r.Publish()
	reg, _ = h.registry(r)
	p := h.oneProject("oauth")
	project = p.Id.String()
	h.install(project, gen.NewInstallation{Kind: "integration", Name: name, RegistryId: &reg.Id, AcceptedPermissions: h.accept("integration", name, reg.Id.String())})
	h.install(project, gen.NewInstallation{Kind: "agent", Name: "oncall-triage", RegistryId: &reg.Id, AcceptedPermissions: h.accept("agent", "oncall-triage", reg.Id.String())})
	return project, reg
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// connectThrough starts connecting, signs in at whatever authorization server the address names
// (the test's authorize), and comes back through /connect/callback.
func (h *harness) connectThrough(project, server string, authorize func(code, challenge string)) (start *url.URL, back *http.Response) {
	h.t.Helper()
	var s gen.PluginConnectStart
	if code := h.do("POST", "/projects/"+project+"/mcp-servers/"+server+"/connect", gen.PluginConnectRequest{ReturnTo: ptr("gator://marketplace")}, &s); code != 200 {
		h.t.Fatalf("connect: %d", code)
	}
	u, cookie := h.browse(s.Url)
	authorize("the-code", u.Query().Get("code_challenge"))
	return u, h.callback("code=the-code&state="+url.QueryEscape(u.Query().Get("state")), cookie)
}

// A hosted MCP server that is its own authorization server is connected the way the MCP spec says:
// Gator reads its metadata, registers a client of its own there and signs in with PKCE, and the
// token it gets back is renewed before it goes out. The job reaches the server only through the
// proxy, with its own token.
func TestARemoteServerConnectsByDiscoveryAndRegistration(t *testing.T) {
	h := newHarness(t)
	as := newOwnAuthServer(t)
	project, _ := h.oauthPackage("issues-oauth", as.URL+"/mcp", "")

	var d gen.MarketplaceDetail
	h.do("GET", "/marketplace/integration/issues-oauth", nil, &d)
	host := strings.TrimPrefix(as.URL, "http://")
	if !slices.Contains(d.Manifest.Permissions, "signs in at "+host+" for the project (OAuth)") {
		t.Fatalf("a person accepts where they sign in: %v", d.Manifest.Permissions)
	}
	if code := h.asJob(h.member(project, "member"), "POST", "/projects/"+project+"/mcp-servers/issues-oauth/connect", nil, nil); code != 403 {
		t.Fatalf("a member connects: %d", code)
	}
	start, back := h.connectThrough(project, "issues-oauth", as.authorize)
	q := start.Query()
	if start.Host != host || start.Path != "/authorize" || q.Get("client_id") != "dyn-1" || q.Get("code_challenge_method") != "S256" ||
		q.Get("resource") != as.URL+"/mcp" || q.Get("redirect_uri") != fakePublicURL+"/connect/callback" {
		t.Fatalf("authorize: %s", start)
	}
	loc, _ := url.Parse(back.Header.Get("Location"))
	if back.StatusCode != 302 || loc.Query().Get("status") != "connected" || loc.Query().Get("mcp") != "issues-oauth" {
		t.Fatalf("callback: %d %s", back.StatusCode, back.Header.Get("Location"))
	}
	var srv gen.McpServer
	h.do("GET", "/projects/"+project+"/mcp-servers/issues-oauth", nil, &srv)
	if !srv.Connected || !slices.Equal(srv.SecretsSet, []string{"access_token"}) {
		t.Fatalf("connected, the client and refresh token unseen: %+v", srv)
	}

	_, job := h.replyJobOf(project, "oncall")
	cs, err := h.proxySession(h.srv.URL+"/mcp/proxy/issues-oauth", map[string]string{"Authorization": "Bearer " + h.identity(job, project)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "get_issue_details", Arguments: map[string]any{"id": "ISSUE-1"}})
	if err != nil || out.IsError || !strings.Contains(resultText(out), "NullPointerException") {
		t.Fatalf("through the proxy with the renewed token: %v %+v", err, out)
	}
	// Connecting again reuses the client registered at the same authorization server.
	h.connectThrough(project, "issues-oauth", as.authorize)
	if as.registered != 1 {
		t.Fatalf("clients registered: %d", as.registered)
	}
}

// The attack the shared apps invited: a package names a provider whose app this server has, and a
// server whose authorization server is its own. The provider's app is used only at the hosts its
// operator listed; anywhere else Gator registers a client for that server alone, and the
// provider's secret goes nowhere.
func TestAProviderAppIsUsedOnlyWhereTheOperatorAllowedIt(t *testing.T) {
	h := newHarness(t)
	evil := newOwnAuthServer(t)
	project, _ := h.oauthPackage("issues-evil", evil.URL+"/mcp", "oauth: {provider: echo-mcp}\n")
	start, back := h.connectThrough(project, "issues-evil", evil.authorize)
	if start.Query().Get("client_id") == "echo-client" || back.StatusCode != 302 {
		t.Fatalf("the echo app at an authorization server nobody allowed it at: %s %d", start, back.StatusCode)
	}
	evil.mu.Lock()
	for _, f := range evil.forms {
		if f.Get("client_secret") == "echo-client-secret" || f.Get("client_id") == "echo-client" {
			t.Fatal("the echo app's secret reached another host")
		}
	}
	evil.mu.Unlock()

	// A server whose metadata names another authorization server than its package declared is
	// not connected: the person accepted signing in at the declared one.
	other := newOwnAuthServer(t)
	other.issuer = "https://elsewhere.example"
	project2, _ := h.oauthPackage("issues-other", other.URL+"/mcp", "")
	var refusal gen.Error
	if code := h.do("POST", "/projects/"+project2+"/mcp-servers/issues-other/connect", nil, &refusal); code != 409 {
		t.Fatalf("an authorization server nobody accepted: %d %+v", code, refusal)
	}
}

// Where the operator allowed it (GATOR_ECHO_MCP_OAUTH_HOSTS=auth.echo.test, in the harness), the
// provider's app signs in at that host, its endpoints read from the host's own metadata, and its
// token is renewed as any other.
func TestAnAllowedProviderAppSignsInAtItsHost(t *testing.T) {
	h := newHarness(t)
	srv := newFakeIssues(t, "tok-second")
	srv.mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": srv.URL + "/mcp", "authorization_servers": []string{"https://auth.echo.test"}})
	})
	project, _ := h.oauthPackage("issues-app", srv.URL+"/mcp", "oauth: {provider: echo-mcp, authorization_server: \"https://auth.echo.test\", scopes: [issues.read]}\n")
	start, back := h.connectThrough(project, "issues-app", h.oauth.authorize)
	if q := start.Query(); start.Host != "auth.echo.test" || q.Get("client_id") != "echo-client" || q.Get("scope") != "issues.read" || back.StatusCode != 302 {
		t.Fatalf("authorize: %s %d", start, back.StatusCode)
	}
	if code := h.do("PUT", "/projects/"+project+"/mcp-servers/issues-app", gen.McpServerSettings{Secrets: &map[string]string{"access_token": "pasted"}}, nil); code != 400 {
		t.Fatalf("an OAuth token is not pasted: %d", code)
	}
	_, job := h.replyJobOf(project, "oncall")
	cs, err := h.proxySession(h.srv.URL+"/mcp/proxy/issues-app", map[string]string{"Authorization": "Bearer " + h.identity(job, project)})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "get_issue_details", Arguments: map[string]any{"id": "ISSUE-1"}}); err != nil || out.IsError {
		t.Fatalf("with the renewed token: %v %+v", err, out)
	}
	if h.oauth.refreshes != 1 {
		t.Fatalf("renewed once: %d", h.oauth.refreshes)
	}
	// A server that does not sign in with OAuth has nothing to connect.
	h.do("PUT", "/projects/"+project+"/mcp-servers/by-hand", gen.McpServerSettings{Url: ptr(srv.URL + "/mcp"), Auth: ptr(gen.McpServerSettingsAuthBearer)}, nil)
	if code := h.do("POST", "/projects/"+project+"/mcp-servers/by-hand/connect", nil, nil); code != 409 {
		t.Fatalf("connecting a bearer server: %d", code)
	}
}

// A server's address and its credentials change together: whoever reads the row while an admin
// points the server elsewhere never sees the new address with the old token, and a sign-in started
// for the old address does not finish for the new one.
func TestANewAddressTakesTheOldCredentialsAtOnce(t *testing.T) {
	h := newHarness(t)
	as := newOwnAuthServer(t)
	project, _ := h.oauthPackage("issues-move", as.URL+"/mcp", "")
	var s gen.PluginConnectStart
	h.do("POST", "/projects/"+project+"/mcp-servers/issues-move/connect", nil, &s)
	u, cookie := h.browse(s.Url)
	elsewhere := newOwnAuthServer(t)
	if code := h.do("PUT", "/projects/"+project+"/mcp-servers/issues-move", gen.McpServerSettings{Url: ptr(elsewhere.URL + "/mcp")}, nil); code != 200 {
		t.Fatalf("move: %d", code)
	}
	as.authorize("the-code", u.Query().Get("code_challenge"))
	if res := h.callback("code=the-code&state="+url.QueryEscape(u.Query().Get("state")), cookie); res.StatusCode != 400 {
		t.Fatalf("a sign-in for the old address finished: %d", res.StatusCode)
	}

	// A bearer server, its token set, moved back and forth while somebody reads it.
	a, b := "https://a.example/mcp", "https://b.example/mcp"
	h.do("PUT", "/projects/"+project+"/mcp-servers/moving", gen.McpServerSettings{Url: &a, Auth: ptr(gen.McpServerSettingsAuthBearer),
		Secrets: &map[string]string{"token": "for-a"}}, nil)
	done := make(chan struct{})
	var bad int
	go func() {
		defer close(done)
		for range 200 {
			var addr, sec string
			_ = h.pool.QueryRow(context.Background(), `SELECT url, secrets FROM mcp_servers WHERE name = 'moving' AND project_id = $1`, project).Scan(&addr, &sec)
			if addr == b && sec != "" {
				bad++
			}
		}
	}()
	for i := range 10 {
		target := b
		if i%2 == 1 {
			target = a
		}
		h.do("PUT", "/projects/"+project+"/mcp-servers/moving", gen.McpServerSettings{Url: &target}, nil)
		if target == a {
			h.do("PUT", "/projects/"+project+"/mcp-servers/moving", gen.McpServerSettings{Secrets: &map[string]string{"token": "for-a"}}, nil)
		}
	}
	<-done
	if bad != 0 {
		t.Fatalf("the new address was seen with the old token %d times", bad)
	}
}
