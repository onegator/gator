package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/plugins"
	"github.com/onegator/gator/internal/server/store/db"
)

const (
	fakePublicURL = "https://gator.test"
	fakeHooksURL  = "https://hooks.gator.test"
)

// catalogueDir is a bundled-plugins directory holding a copy of echo under a name of its own,
// new for every harness, so a test installs it from the catalogue as a server would the first
// time: plugin rows stay in a shared database for good.
func catalogueDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	name := fmt.Sprintf("echo-b%d", time.Now().UnixNano())
	if err := os.Symlink(echoPlugin(t), filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	return dir, name
}

// fakeProvider is an OAuth provider at auth.echo.test: the host's HTTP client is pointed at it.
type fakeProvider struct {
	*httptest.Server
	mu        sync.Mutex
	codes     map[string]string // code → PKCE challenge
	refreshes int
}

func newFakeProvider(t *testing.T) *fakeProvider {
	f := &fakeProvider{codes: map[string]string{}}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.token))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeProvider) client(provider string) (plugins.OAuthClient, bool) {
	if !strings.HasPrefix(provider, "echo") {
		return plugins.OAuthClient{}, false
	}
	return plugins.OAuthClient{ID: "echo-client", Secret: "echo-client-secret"}, true
}

// httpClient sends requests for auth.echo.test to the fake; anything else goes where it says.
func (f *fakeProvider) httpClient() *http.Client {
	c := *f.Client() // a copy: the fake's own client stays as it is for the next caller
	inner := c.Transport
	target, _ := url.Parse(f.URL)
	c.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "auth.echo.test" {
			return http.DefaultTransport.RoundTrip(r)
		}
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return inner.RoundTrip(r)
	})
	return &c
}

type roundTripper func(*http.Request) (*http.Response, error)

func (rt roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

func (f *fakeProvider) authorize(code, challenge string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes[code] = challenge
}

func (f *fakeProvider) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	reply := func(status int, v map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if r.URL.Path == "/.well-known/oauth-authorization-server" {
		reply(200, map[string]any{"issuer": "https://auth.echo.test", "authorization_endpoint": "https://auth.echo.test/authorize",
			"token_endpoint": "https://auth.echo.test/token", "code_challenge_methods_supported": []string{"S256"}})
		return
	}
	if r.URL.Path != "/token" || r.Form.Get("client_id") != "echo-client" || r.Form.Get("client_secret") != "echo-client-secret" {
		reply(401, map[string]any{"error": "invalid_client"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		challenge, ok := f.codes[r.Form.Get("code")]
		delete(f.codes, r.Form.Get("code"))
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.Form.Get("redirect_uri") != fakePublicURL+"/connect/callback" {
			reply(400, map[string]any{"error": "invalid_grant", "error_description": "bad code or verifier"})
			return
		}
		reply(200, map[string]any{"access_token": "tok-first", "refresh_token": "ref-1", "expires_in": 60, "token_type": "bearer"})
	case "refresh_token":
		if r.Form.Get("refresh_token") != "ref-1" {
			reply(400, map[string]any{"error": "invalid_grant"})
			return
		}
		f.refreshes++
		reply(200, map[string]any{"access_token": "tok-second", "refresh_token": "ref-2", "expires_in": 3600, "token_type": "bearer"})
	default:
		reply(400, map[string]any{"error": "unsupported_grant_type"})
	}
}

// person signs in someone who is not a workspace admin.
func (h *harness) person() string {
	h.t.Helper()
	ctx := context.Background()
	u, err := db.New(h.pool).UpsertUserByEmail(ctx, db.UpsertUserByEmailParams{Email: fmt.Sprintf("p-%d@t.local", time.Now().UnixNano()), Name: "Person"})
	if err != nil {
		h.t.Fatal(err)
	}
	tok, _, err := auth.Tokens{Pool: h.pool}.Issue(ctx, auth.IssueParams{Kind: auth.KindUser, Scope: "user", Name: "e2e", UserID: u.ID, TTL: time.Hour})
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

func (h *harness) uiMarkdown(projectID string) string {
	h.t.Helper()
	var task gen.Task
	h.do("POST", "/projects/"+projectID+"/tasks", gen.NewTask{Kind: "chore", Title: "UI"}, &task)
	var out gen.PluginUI
	h.do("GET", "/tasks/"+task.Id.String()+"/plugin-ui", nil, &out)
	for _, tab := range out.Tabs {
		if tab.Plugin == "echo" {
			return tab.Markdown
		}
	}
	return ""
}

// A person who started a project switches on a plugin the server ships, without anybody
// typing a command line, and the server knows where its webhooks arrive.
func TestABundledPluginIsOneClickAway(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	bundled := h.bundled
	person := h.person()
	var cat []gen.CataloguePlugin
	if code := h.asJob(person, "GET", "/plugins/catalogue", nil, &cat); code != 200 {
		t.Fatalf("catalogue: %d", code)
	}
	i := slices.IndexFunc(cat, func(c gen.CataloguePlugin) bool { return c.Name == bundled })
	if i < 0 || cat[i].Installed || cat[i].Description == nil || cat[i].Connect == nil || cat[i].Connect.Title != "Echo" || !cat[i].Connect.Available {
		t.Fatalf("catalogue entry: %+v", cat)
	}

	var p gen.Project
	if code := h.asJob(person, "POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("cat%d", time.Now().UnixNano()), Name: "Catalogue"}, &p); code != 201 {
		t.Fatalf("project: %d", code)
	}
	t.Cleanup(func() {
		_, _ = h.pool.Exec(ctx, "UPDATE project_plugins SET enabled = false WHERE project_id = $1", p.Id.String())
	})
	cfg := map[string]any{"greeting": "hi"}
	var v gen.ProjectPlugin
	if code := h.asJob(person, "PUT", "/projects/"+p.Id.String()+"/plugins/"+bundled, gen.ProjectPluginSettings{Enabled: true, Config: &cfg}, &v); code != 200 {
		t.Fatalf("switching on a bundled plugin: %d %+v", code, v)
	}
	want := fakeHooksURL + "/hooks/" + p.Slug + "/" + bundled
	if v.WebhookUrl == nil || *v.WebhookUrl != want || v.Connect == nil || v.Connect.Connected {
		t.Fatalf("view: %+v", v)
	}
	// The plugin hears the same address, so it can register the webhook itself.
	h.until("the plugin to learn its webhook address", func() bool {
		return h.count(`SELECT count(*) FROM plugin_kv kv JOIN project_plugins pp ON pp.id = kv.project_plugin_id
			WHERE pp.project_id = $1 AND kv.key = 'webhook_url' AND kv.value = to_jsonb($2::text)`, p.Id.String(), want) == 1
	})
	// A plugin that is not in the catalogue still needs a workspace admin.
	if code := h.asJob(person, "PUT", "/projects/"+p.Id.String()+"/plugins/nonesuch", gen.ProjectPluginSettings{Enabled: true}, nil); code != 404 {
		t.Fatalf("an unknown plugin: %d", code)
	}
	if code := h.asJob(person, "PUT", "/plugins/"+bundled, gen.InstallPlugin{Command: []string{"/bin/sh"}}, nil); code != 403 {
		t.Fatalf("a person installing a command: %d", code)
	}
}

// "Connect" runs the provider's sign-in with state and PKCE, stores the token as the
// project's secret, and the server renews it before it runs out.
func TestConnectingAPluginStoresAndRenewsItsToken(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	path := "/projects/" + p.id + "/plugins/echo/connect"

	var bad map[string]any
	if code := h.do("POST", path, gen.PluginConnectRequest{ReturnTo: ptr("https://evil.example/")}, &bad); code != 400 {
		t.Fatalf("a foreign return address: %d", code)
	}
	var start gen.PluginConnectStart
	if code := h.do("POST", path, gen.PluginConnectRequest{ReturnTo: ptr("gator://plugins")}, &start); code != 200 {
		t.Fatalf("connect: %d", code)
	}
	u, cookie := h.browse(start.Url)
	if u.Host != "auth.echo.test" || u.Path != "/authorize" {
		t.Fatalf("authorize url: %s", u)
	}
	q := u.Query()
	if q.Get("client_id") != "echo-client" || q.Get("scope") != "read write" || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != fakePublicURL+"/connect/callback" || q.Get("state") == "" {
		t.Fatalf("authorize query: %v", q)
	}
	h.oauth.authorize("good-code", q.Get("code_challenge"))
	query := "code=good-code&state=" + url.QueryEscape(q.Get("state"))

	// A browser that did not start the connection — somebody handed the provider's link on — is
	// refused, and the state is still there for the one that did.
	if res := h.callback(query, nil); res.StatusCode != 400 {
		t.Fatalf("a callback without the start's cookie: %d", res.StatusCode)
	}
	if res := h.callback(query, &http.Cookie{Name: "gator_connect", Value: "another-state"}); res.StatusCode != 400 {
		t.Fatalf("a callback with another connection's cookie: %d", res.StatusCode)
	}
	res := h.callback(query, cookie)
	loc, _ := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != 302 || loc.Scheme != "gator" || loc.Query().Get("status") != "connected" || loc.Query().Get("plugin") != "echo" || loc.Query().Get("project") != p.id {
		t.Fatalf("callback: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// The state is spent.
	if res := h.callback(query, cookie); res.StatusCode != 400 {
		t.Fatalf("a replayed callback: %d", res.StatusCode)
	}

	v := h.echoState(p.id)
	if v.Connect == nil || !v.Connect.Connected || !slices.Equal(v.SecretsSet, []string{"hook_secret", "token"}) {
		t.Fatalf("after connecting: %+v %+v", v.Connect, v.SecretsSet)
	}
	if v.Config["greeting"] != "hi" || !v.Enabled {
		t.Fatalf("connecting must leave the settings and the switch alone: %+v", v)
	}
	h.until("the plugin to restart with the token", func() bool { return h.uiMarkdown(p.id) == "hi token=tok-first" })
	// The refresh token stays with the server; the plugin's audit never sees a token.
	leaks := h.count(`SELECT count(*) FROM plugin_calls c JOIN project_plugins pp ON pp.id = c.project_plugin_id
		WHERE pp.project_id = $1 AND concat(c.payload::text, c.result::text, c.error) LIKE '%ref-1%'`, p.id)
	if leaks != 0 {
		t.Fatalf("the refresh token reached the plugin's calls %d times", leaks)
	}

	// The token expires within the minute, so the next sweep renews it.
	h.plugins.RefreshTokens(context.Background())
	h.until("the plugin to restart with the renewed token", func() bool { return h.uiMarkdown(p.id) == "hi token=tok-second" })

	// A pasted token replaces the connection, and the server stops renewing it.
	if _, code := h.configureEcho(p.id, map[string]any{"greeting": "hi", "token": "pasted"}); code != 200 {
		t.Fatalf("paste: %d", code)
	}
	if n := h.count(`SELECT count(*) FROM project_plugins WHERE project_id = $1 AND token_expires_at IS NOT NULL`, p.id); n != 0 {
		t.Fatalf("a pasted token is still being renewed")
	}

	// A provider that says no reaches the person, not a stack trace.
	if code := h.do("POST", path, nil, &start); code != 200 {
		t.Fatalf("connect without a body: %d", code)
	}
	u, cookie = h.browse(start.Url)
	res = h.callback("error=access_denied&error_description=The+user+said+no&state="+url.QueryEscape(u.Query().Get("state")), cookie)
	if res.StatusCode != 400 {
		t.Fatalf("a refusal: %d", res.StatusCode)
	}

	// A state that expired is refused at both doors, and a browser signed in to Gator as somebody
	// else cannot carry another person's connection.
	h.do("POST", path, nil, &start)
	u, cookie = h.browse(start.Url)
	stranger := h.person()
	req, _ := http.NewRequest("GET", h.srv.URL+"/connect/start?state="+url.QueryEscape(u.Query().Get("state")), nil)
	req.Header.Set("Authorization", "Bearer "+stranger)
	if res, err := noRedirect.Do(req); err != nil || res.StatusCode != 403 {
		t.Fatalf("another person's browser: %v %d", err, res.StatusCode)
	}
	if _, err := h.pool.Exec(context.Background(), `UPDATE plugin_connect_states SET expires_at = now() - interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if res := h.callback("code=x&state="+url.QueryEscape(u.Query().Get("state")), cookie); res.StatusCode != 400 {
		t.Fatalf("an expired state: %d", res.StatusCode)
	}
	if res, _ := noRedirect.Get(h.srv.URL + "/connect/start?state=" + url.QueryEscape(u.Query().Get("state"))); res.StatusCode != 400 {
		t.Fatalf("an expired start: %d", res.StatusCode)
	}
}

func TestConnectNeedsAnOAuthApp(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi"})
	var e gen.Error
	if code := h.do("POST", "/projects/"+p.id+"/plugins/nonesuch/connect", nil, &e); code != 404 {
		t.Fatalf("an unknown plugin: %d", code)
	}
	// A viewer cannot start a connection for someone else's project.
	if code := h.asJob(h.person(), "POST", "/projects/"+p.id+"/plugins/echo/connect", nil, nil); code != 403 {
		t.Fatalf("a stranger connecting: %d", code)
	}
}

// A tracker switched on brings in what it already holds, once, and the task follows the issue
// it came from: closed, reopened and edited there means the same here.
func TestATrackerImportsAndFollowsItsIssues(t *testing.T) {
	h := newHarness(t)
	p := h.echoProject(map[string]any{"greeting": "hi", "import": "First,Second"})
	imported := func() int {
		return h.count(`SELECT count(*) FROM tasks WHERE project_id = $1 AND external_refs ? 'echo_import'`, p.id)
	}
	h.until("the import on switching on", func() bool { return imported() == 2 })
	var res gen.PluginImport
	if code := h.do("POST", "/projects/"+p.id+"/plugins/echo/import", nil, &res); code != 200 || res.Created != 0 || res.Existing != 2 {
		t.Fatalf("importing again: %d %+v", code, res)
	}
	if imported() != 2 {
		t.Fatalf("an import twice made %d tasks", imported())
	}
	if _, code := h.configureEcho(p.id, map[string]any{"greeting": "hi"}); code != 200 {
		t.Fatal(code)
	}
	var e gen.Error
	if code := h.do("POST", "/projects/"+p.id+"/plugins/echo/import", nil, &e); code != 400 || !strings.Contains(e.Error, "nothing to import") {
		t.Fatalf("a plugin's refusal: %d %+v", code, e)
	}

	var task gen.Task
	h.do("POST", "/projects/"+p.id+"/tasks", gen.NewTask{Kind: "chore", Title: "Old title"}, &task)
	id := task.Id.String()
	send := func(n int, body map[string]any) {
		t.Helper()
		if code, out := h.hook(p.slug, fmt.Sprintf("follow-%s-%d", id, n), jsonBody(body), nil); code != 200 {
			t.Fatalf("hook %v: %d %v", body, code, out)
		}
	}
	send(1, map[string]any{"retitle": map[string]string{"task": id, "title": "New title", "description": "Edited there"}})
	h.do("GET", "/tasks/"+id, nil, &task)
	if task.Title != "New title" || task.Description == nil || *task.Description != "Edited there" {
		t.Fatalf("edited: %+v", task)
	}
	send(2, map[string]any{"close": id})
	h.until("the closed task's hook", func() bool {
		return h.count(`SELECT count(*) FROM plugin_kv kv JOIN project_plugins pp ON pp.id = kv.project_plugin_id
			WHERE pp.project_id = $1 AND kv.key = 'closed_upstream' AND kv.value = to_jsonb($2::text)`, p.id, id+" closed upstream") == 1
	})
	send(3, map[string]any{"reopen": id})
	if n := h.count(`SELECT count(*) FROM tasks WHERE id = $1 AND closed_at IS NULL`, id); n != 1 {
		t.Fatal("the task did not reopen")
	}
	if n := h.count(`SELECT count(*) FROM phase_transitions WHERE task_id = $1 AND kind IN ('close', 'reopen') AND actor_kind = 'plugin'`, id); n != 2 {
		t.Fatalf("the audit trail has %d plugin closings and reopenings", n)
	}
}

func ptr[T any](v T) *T { return &v }

// browse follows a connection's start link as the person's browser does: Gator sets its cookie and
// sends the browser to the provider. It answers the provider's address and the cookie the browser
// now holds.
func (h *harness) browse(start string) (*url.URL, *http.Cookie) {
	h.t.Helper()
	u, err := url.Parse(start)
	if err != nil || u.Path != "/connect/start" {
		h.t.Fatalf("a connection starts at Gator: %s", start)
	}
	res, err := noRedirect.Get(h.srv.URL + u.RequestURI())
	if err != nil {
		h.t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		h.t.Fatalf("start: %d", res.StatusCode)
	}
	var cookie *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == "gator_connect" {
			cookie = c
		}
	}
	provider, _ := url.Parse(res.Header.Get("Location"))
	if cookie == nil || !cookie.HttpOnly {
		h.t.Fatalf("the start ties the browser: %+v", res.Cookies())
	}
	return provider, cookie
}

// callback comes back from the provider, with the browser's cookie (or none).
func (h *harness) callback(query string, cookie *http.Cookie) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.srv.URL+"/connect/callback?"+query, nil)
	if cookie != nil {
		req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	res, err := noRedirect.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	res.Body.Close()
	return res
}
