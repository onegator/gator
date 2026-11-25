package e2e

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/marketplace"
	"github.com/onegator/gator/internal/server/mcpremote"
	"github.com/onegator/gator/internal/server/netguard"
)

// A server that keeps to the production policy reaches no private address: a project's admin
// cannot point a remote MCP server at loopback, a private network or the cloud's metadata service,
// over https or plain http, and neither can a package's download or a registry.
func TestOutboundRequestsStayOffPrivateAddresses(t *testing.T) {
	h := newHarness(t)
	p := h.oneProject("ssrf")
	strict := mcpremote.New(h.pool, mcpremote.Deps{Net: netguard.Policy{}})
	admin, err := auth.Tokens{Pool: h.pool}.Verify(t.Context(), h.admin)
	if err != nil {
		t.Fatal(err)
	}
	project := pgUUID(p.Id.String())
	if _, err := strict.Servers.Put(t.Context(), admin, project, "local", mcpremote.Settings{URL: ptr("http://127.0.0.1:9/mcp"), Auth: ptr("none")}); err == nil {
		t.Fatal("plain http to loopback in production")
	}
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer tls.Close()
	for name, u := range map[string]string{"loopback": tls.URL + "/mcp", "metadata": "https://169.254.169.254/mcp", "private": "https://10.0.0.1/mcp"} {
		if _, err := strict.Servers.Put(t.Context(), admin, project, name, mcpremote.Settings{URL: ptr(u), Auth: ptr("none")}); err != nil {
			t.Fatalf("%s: an https address is stored: %v", name, err)
		}
		if _, err := strict.Servers.Tools(t.Context(), admin, project, name); err == nil || !strings.Contains(err.Error(), "not a public address") {
			t.Fatalf("%s is reached: %v", name, err)
		}
	}
	dl := marketplace.HTTPDownloader{Net: netguard.Policy{}}
	if _, err := dl.Download(t.Context(), tls.URL+"/build"); err == nil {
		t.Fatal("a download from loopback")
	}
	if _, err := dl.Download(t.Context(), "file:///etc/passwd"); err == nil {
		t.Fatal("a file:// download without the operator's word")
	}
	if _, err := (marketplace.Git{}).Fetch(t.Context(), "https://127.0.0.1/registry.git"); err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("a registry on loopback: %v", err)
	}
	if _, err := (marketplace.Git{}).Fetch(t.Context(), "git://example.com/registry.git"); err == nil {
		t.Fatal("the git protocol")
	}
}

func pgUUID(s string) pgtype.UUID {
	var u pgtype.UUID
	_ = u.Scan(s)
	return u
}
