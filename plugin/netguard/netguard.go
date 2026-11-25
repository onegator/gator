// Package netguard keeps outbound requests — gator-server's own, and a plugin's — — to remote MCP servers, OAuth
// authorization servers, native plugin downloads, registries — off the machine's private side: no
// loopback, private, link-local (cloud metadata), shared, multicast or unspecified address, checked
// when the connection is made, so a name that resolves to one (DNS rebinding included) is refused
// as surely as the address itself. Plain http is refused too. An operator who runs those services
// on a private network says so (GATOR_OUTBOUND_ALLOW_PRIVATE=1); tests do the same for their fakes.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrRefused is an address the policy keeps the server from reaching.
var ErrRefused = errors.New("refused: not a public address")

// Policy is what the server may reach.
type Policy struct {
	// AllowPrivate lets requests reach private and loopback addresses, and use plain http to
	// loopback: development, tests, a company network. Off in production by default.
	AllowPrivate bool
}

// FromEnv reads GATOR_OUTBOUND_ALLOW_PRIVATE.
func FromEnv(getenv func(string) string) Policy {
	return Policy{AllowPrivate: getenv("GATOR_OUTBOUND_ALLOW_PRIVATE") == "1"}
}

var blocked = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8", "64:ff9b::/96",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// Public says whether addr is one the server may reach without AllowPrivate.
func Public(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// Dialer is a dialer that refuses what the policy does, at the moment of connecting.
func (p Policy) Dialer() *net.Dialer {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if p.AllowPrivate {
		return d
	}
	d.Control = func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil || !Public(ap.Addr()) {
			return fmt.Errorf("%w: %s", ErrRefused, address)
		}
		return nil
	}
	return d
}

// Transport is an HTTP transport that dials through Dialer and refuses plain http unless the
// policy allows private addresses (and then only to them, where a test or a laptop runs things).
func (p Policy) Transport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = p.Dialer().DialContext
	t.Proxy = nil // a proxy would dial on our behalf, past the check
	return schemeGuard{policy: p, next: t}
}

// Client is an HTTP client over Transport, not following redirects to anywhere it has not checked.
func (p Policy) Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: p.Transport()}
}

type schemeGuard struct {
	policy Policy
	next   http.RoundTripper
}

func (g schemeGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := g.policy.CheckScheme(r.URL); err != nil {
		return nil, err
	}
	return g.next.RoundTrip(r)
}

// CheckScheme holds a URL to https, or to http on loopback when private addresses are allowed.
func (p Policy) CheckScheme(u *url.URL) error {
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if p.AllowPrivate && Loopback(u.Hostname()) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s must be https", ErrRefused, u.Redacted())
}

// CheckHost resolves a host and refuses it when any of its addresses is not public: for clients
// whose connection this package cannot see (git). A name that changes its answer between this
// check and the connection is the residual risk such a client keeps.
func (p Policy) CheckHost(ctx context.Context, host string) error {
	if p.AllowPrivate {
		return nil
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !Public(addr) {
			return fmt.Errorf("%w: %s", ErrRefused, host)
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if !Public(a) {
			return fmt.Errorf("%w: %s resolves to %s", ErrRefused, host, a)
		}
	}
	return nil
}

// Loopback says whether host names this machine.
func Loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

// APIHostsEnv names the hosts an operator lets plugins' API addresses point at besides each
// plugin's own (a GitHub Enterprise, a test's fake): comma separated, host or host:port. The
// server passes it on to plugins; a project admin cannot set it.
const APIHostsEnv = "GATOR_PLUGIN_API_HOSTS"

// APIBase is the API address a plugin talks to: def unless setting names another, which must be
// https (or http to loopback where private addresses are allowed) on def's host or on a host the
// operator listed in APIHostsEnv. A plugin's secret goes to this address, and the setting is a
// project admin's to change; the hosts it may point at are the operator's.
func APIBase(setting, def string, getenv func(string) string) (string, error) {
	if setting == "" || setting == def {
		return def, nil
	}
	u, err := url.Parse(setting)
	if err != nil || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("%w: the API address %q is not an address", ErrRefused, setting)
	}
	if d, err := url.Parse(def); err == nil && strings.EqualFold(d.Host, u.Host) && u.Scheme == "https" {
		return setting, nil
	}
	for h := range strings.SplitSeq(getenv(APIHostsEnv), ",") {
		if h = strings.TrimSpace(h); h != "" && (strings.EqualFold(h, u.Host) || strings.EqualFold(h, u.Hostname())) {
			if err := FromEnv(getenv).CheckScheme(u); err != nil {
				return "", err
			}
			return setting, nil
		}
	}
	return "", fmt.Errorf("%w: the API address %s is not one the operator allowed (%s)", ErrRefused, u.Host, APIHostsEnv)
}
