package limits

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Proxies are the peers allowed to say who the client is. Anyone else could claim any
// address in a header, and a rate limit keyed on a claim is no limit at all.
type Proxies []netip.Prefix

// ParseProxies reads a comma-separated list of addresses and CIDR prefixes.
func ParseProxies(s string) (Proxies, error) {
	var out Proxies
	for f := range strings.SplitSeq(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", f, err)
			}
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", f, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func (p Proxies) trusts(a netip.Addr) bool {
	a = a.Unmap()
	for _, pre := range p {
		if pre.Contains(a) {
			return true
		}
	}
	return false
}

// RealIP sets r.RemoteAddr to the client's address when the request came through a trusted
// proxy. The client is the rightmost X-Forwarded-For hop that is not itself a trusted proxy:
// everything to the left of it was written by the client and proves nothing. X-Real-IP and
// True-Client-IP are ignored, because a proxy that does not set them passes a client's on.
func (p Proxies) RealIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip, ok := p.clientIP(r); ok {
			r.RemoteAddr = ip.String()
		}
		next.ServeHTTP(w, r)
	})
}

func (p Proxies) clientIP(r *http.Request) (netip.Addr, bool) {
	peer, err := addrOf(r.RemoteAddr)
	if err != nil || !p.trusts(peer) {
		return netip.Addr{}, false
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return netip.Addr{}, false
		}
		if !p.trusts(a) {
			return a.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

func addrOf(remote string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	return netip.ParseAddr(host)
}
