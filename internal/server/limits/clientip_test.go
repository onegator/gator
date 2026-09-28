package limits

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRealIPTrustsOnlyItsProxies(t *testing.T) {
	proxies, err := ParseProxies("127.0.0.0/8, ::1, 10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, remote string
		headers      map[string][]string
		want         string
	}{
		{"direct client keeps its address", "203.0.113.7:5000",
			map[string][]string{"X-Forwarded-For": {"198.51.100.1"}, "X-Real-Ip": {"198.51.100.2"}, "True-Client-Ip": {"198.51.100.3"}},
			"203.0.113.7:5000"},
		{"proxy names the client", "127.0.0.1:4000",
			map[string][]string{"X-Forwarded-For": {"203.0.113.7"}}, "203.0.113.7"},
		{"a client's own claim to the left is ignored", "127.0.0.1:4000",
			map[string][]string{"X-Forwarded-For": {"198.51.100.1, 203.0.113.7"}}, "203.0.113.7"},
		{"trusted hops are skipped", "127.0.0.1:4000",
			map[string][]string{"X-Forwarded-For": {"198.51.100.1, 203.0.113.7, 10.1.2.3"}}, "203.0.113.7"},
		{"repeated headers read as one list", "127.0.0.1:4000",
			map[string][]string{"X-Forwarded-For": {"198.51.100.1", "203.0.113.7"}}, "203.0.113.7"},
		{"X-Real-IP is not a proxy's word", "127.0.0.1:4000",
			map[string][]string{"X-Real-Ip": {"198.51.100.2"}, "True-Client-Ip": {"198.51.100.3"}}, "127.0.0.1:4000"},
		{"garbage where the client should be", "127.0.0.1:4000",
			map[string][]string{"X-Forwarded-For": {"203.0.113.7, not-an-ip"}}, "127.0.0.1:4000"},
		{"ipv6 loopback proxy", "[::1]:4000",
			map[string][]string{"X-Forwarded-For": {"2001:db8::1"}}, "2001:db8::1"},
		{"ipv4-mapped proxy", "[::ffff:127.0.0.1]:4000",
			map[string][]string{"X-Forwarded-For": {"203.0.113.7"}}, "203.0.113.7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			h := proxies.RealIP(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = c.remote
			for k, vs := range c.headers {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != c.want {
				t.Fatalf("RemoteAddr = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseProxiesRejectsNonsense(t *testing.T) {
	if _, err := ParseProxies("127.0.0.1, localhost"); err == nil {
		t.Fatal("a hostname is not an address")
	}
	if p, err := ParseProxies(""); err != nil || len(p) != 0 {
		t.Fatalf("empty list: %v %v", p, err)
	}
}

func TestSpoofedHeaderDoesNotBuyANewBucket(t *testing.T) {
	proxies, _ := ParseProxies("127.0.0.1")
	lim := NewLimiter(0.001, 1)
	h := proxies.RealIP(Middleware(lim, ByBearerOrIP, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	codes := []int{}
	for _, spoof := range []string{"198.51.100.1", "198.51.100.2"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "203.0.113.7:5000"
		req.Header.Set("X-Forwarded-For", spoof)
		req.Header.Set("X-Real-Ip", spoof)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		codes = append(codes, rec.Code)
	}
	if codes[1] != http.StatusTooManyRequests {
		t.Fatalf("second request from the same client got %v; a header should not reset its limit", codes)
	}
}
