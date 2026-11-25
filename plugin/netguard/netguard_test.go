package netguard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

func TestOnlyPublicAddressesArePublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.20.0.1": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.100.100.200": false, "0.0.0.0": false, "::1": false, "fd00::1": false, "fe80::1": false, "::ffff:127.0.0.1": false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: public %v", addr, got)
		}
	}
}

// The check is made when the connection is, so a name (or a redirect) that lands on a private
// address is refused as surely as the address.
func TestTheDialerRefusesWhatThePolicyDoes(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	strict := Policy{}
	tr := strict.Transport().(schemeGuard).next.(*http.Transport)
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	c := &http.Client{Timeout: 5 * time.Second, Transport: schemeGuard{policy: strict, next: tr}}
	if _, err := c.Get(srv.URL); !errors.Is(err, ErrRefused) {
		t.Fatalf("loopback over https: %v", err)
	}
	if _, err := c.Get("http://127.0.0.1:1/"); !errors.Is(err, ErrRefused) {
		t.Fatalf("plain http: %v", err)
	}
	if err := strict.CheckScheme(&url.URL{Scheme: "http", Host: "localhost"}); err == nil {
		t.Fatal("plain http to localhost outside development")
	}
	open := Policy{AllowPrivate: true}
	if err := open.CheckScheme(&url.URL{Scheme: "http", Host: "127.0.0.1:8080"}); err != nil {
		t.Fatal(err)
	}
	if err := open.CheckScheme(&url.URL{Scheme: "http", Host: "example.com"}); err == nil {
		t.Fatal("plain http to a remote host, even in development")
	}
	if err := strict.CheckHost(t.Context(), "169.254.169.254"); !errors.Is(err, ErrRefused) {
		t.Fatalf("metadata: %v", err)
	}
	if err := strict.CheckHost(t.Context(), "localhost"); !errors.Is(err, ErrRefused) {
		t.Fatalf("a name for loopback: %v", err)
	}
}

func TestAPIBaseKeepsSecretsOnTheirHosts(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	none := env(nil)
	for _, c := range []struct {
		setting string
		getenv  func(string) string
		ok      bool
	}{
		{"", none, true},
		{"https://slack.com/api", none, true},
		{"https://evil.example/api", none, false},
		{"http://slack.com/api", none, false},
		{"https://user:pw@slack.com/api", none, false},
		{"https://ghe.corp/api/v3", env(map[string]string{APIHostsEnv: "ghe.corp"}), true},
		{"http://127.0.0.1:9/api", env(map[string]string{APIHostsEnv: "127.0.0.1"}), false},
		{"http://127.0.0.1:9/api", env(map[string]string{APIHostsEnv: "127.0.0.1", "GATOR_OUTBOUND_ALLOW_PRIVATE": "1"}), true},
	} {
		_, err := APIBase(c.setting, "https://slack.com/api", c.getenv)
		if (err == nil) != c.ok {
			t.Errorf("%q: %v", c.setting, err)
		}
	}
}
