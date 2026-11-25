package e2e

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/marketplace/marketplacetest"
)

// A remote server that signs in with OAuth is connected the way a plugin is: Gator sends the
// person's browser to the provider, the provider sends it back to /connect/callback with a code,
// and the token is kept on the server, renewed before it expires. The job reaches the server
// through the proxy with its own token; the server gets the renewed one.
func TestARemoteServerConnectsWithOAuthThroughTheSameCallback(t *testing.T) {
	h := newHarness(t)
	fake := newFakeIssues(t, "tok-second") // what the token is once renewed
	r := marketplacetest.New(t)
	r.Package("integrations/issues-oauth", map[string]string{"integration.yaml": `name: issues-oauth
version: 1.0.0
display_name: Issues (OAuth)
description: Production errors, signed in with OAuth.
kind: mcp
url: ` + fake.URL + `/mcp
auth: oauth
oauth: {provider: echo-mcp, authorize_url: "https://auth.echo.test/authorize", token_url: "https://auth.echo.test/token", scopes: [issues.read]}
tools_default_allow: [get_issue_details]
`})
	agent := map[string]string{"agent.yaml": strings.Replace(oncall["agent.yaml"], "mcp: [issues-mcp]", "mcp: [issues-oauth]", 1), "instructions.md": oncall["instructions.md"]}
	r.Package("agents/oncall-triage", agent)
	r.Publish()
	reg, _ := h.registry(r)
	p := h.oneProject("oauth")
	project := p.Id.String()
	h.install(project, gen.NewInstallation{Kind: "integration", Name: "issues-oauth", RegistryId: &reg.Id, AcceptedPermissions: h.accept("integration", "issues-oauth", reg.Id.String())})
	h.install(project, gen.NewInstallation{Kind: "agent", Name: "oncall-triage", RegistryId: &reg.Id, AcceptedPermissions: h.accept("agent", "oncall-triage", reg.Id.String())})

	var srv gen.McpServer
	h.do("GET", "/projects/"+project+"/mcp-servers/issues-oauth", nil, &srv)
	if srv.Connected || srv.Auth != "oauth" {
		t.Fatalf("before connecting: %+v", srv)
	}
	path := "/projects/" + project + "/mcp-servers/issues-oauth/connect"
	if code := h.asJob(h.member(project, "member"), "POST", path, nil, nil); code != 403 {
		t.Fatalf("a member connects: %d", code)
	}
	if code := h.do("POST", path, gen.PluginConnectRequest{ReturnTo: ptr("https://evil.example/")}, nil); code != 400 {
		t.Fatalf("a foreign return address: %d", code)
	}
	var start gen.PluginConnectStart
	if code := h.do("POST", path, gen.PluginConnectRequest{ReturnTo: ptr("gator://marketplace")}, &start); code != 200 {
		t.Fatalf("connect: %d", code)
	}
	u, _ := url.Parse(start.Url)
	q := u.Query()
	if u.Host != "auth.echo.test" || q.Get("client_id") != "echo-client" || q.Get("scope") != "issues.read" ||
		q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != fakePublicURL+"/connect/callback" {
		t.Fatalf("authorize: %s", start.Url)
	}
	h.oauth.authorize("mcp-code", q.Get("code_challenge"))
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	callback := func() *http.Response {
		res, err := noRedirect.Get(h.srv.URL + "/connect/callback?code=mcp-code&state=" + url.QueryEscape(q.Get("state")))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	res := callback()
	loc, _ := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != 302 || loc.Query().Get("status") != "connected" || loc.Query().Get("mcp") != "issues-oauth" || loc.Query().Get("project") != project {
		t.Fatalf("callback: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if res := callback(); res.StatusCode != 400 {
		t.Fatalf("a replayed callback: %d", res.StatusCode)
	}
	h.do("GET", "/projects/"+project+"/mcp-servers/issues-oauth", nil, &srv)
	if !srv.Connected || !slices.Equal(srv.SecretsSet, []string{"access_token"}) {
		t.Fatalf("connected, the refresh token unseen: %+v", srv)
	}
	if code := h.do("PUT", "/projects/"+project+"/mcp-servers/issues-oauth", gen.McpServerSettings{Secrets: &map[string]string{"access_token": "pasted"}}, nil); code != 400 {
		t.Fatalf("an OAuth token is not pasted: %d", code)
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
	if h.oauth.refreshes != 1 {
		t.Fatalf("the token about to expire was renewed once: %d", h.oauth.refreshes)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if slices.ContainsFunc(fake.auth, func(a string) bool { return a != "Bearer tok-second" }) {
		t.Fatalf("the server saw only the renewed token: %v", fake.auth)
	}

	// A server that does not sign in with OAuth has nothing to connect.
	h.do("PUT", "/projects/"+project+"/mcp-servers/by-hand", gen.McpServerSettings{Url: ptr(fake.URL + "/mcp"), Auth: ptr(gen.McpServerSettingsAuthBearer)}, nil)
	if code := h.do("POST", "/projects/"+project+"/mcp-servers/by-hand/connect", nil, nil); code != 409 {
		t.Fatalf("connecting a bearer server: %d", code)
	}
}
