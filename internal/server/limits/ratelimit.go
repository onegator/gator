// Package limits holds request rate limiting and the circuit breaker used around plugins.
package limits

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is a keyed token bucket: one bucket per caller (token or IP).
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*entry
	rate    rate.Limit
	burst   int
	ttl     time.Duration
}

type entry struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewLimiter allows `perSecond` sustained with `burst` headroom per key.
func NewLimiter(perSecond float64, burst int) *Limiter {
	l := &Limiter{buckets: map[string]*entry{}, rate: rate.Limit(perSecond), burst: burst, ttl: 10 * time.Minute}
	go l.sweep()
	return l
}

// Allow reports whether key may proceed now.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	e, ok := l.buckets[key]
	if !ok {
		e = &entry{lim: rate.NewLimiter(l.rate, l.burst)}
		l.buckets[key] = e
	}
	e.seen = time.Now()
	l.mu.Unlock()
	return e.lim.Allow()
}

func (l *Limiter) sweep() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		l.mu.Lock()
		for k, e := range l.buckets {
			if time.Since(e.seen) > l.ttl {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}

// KeyFunc derives the bucket key from a request.
type KeyFunc func(r *http.Request) string

// ByBearerOrIP keys on the bearer token when present, otherwise the client IP.
func ByBearerOrIP(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return "tok:" + h[7:]
	}
	if c, err := r.Cookie("gator_session"); err == nil {
		return "tok:" + c.Value
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "ip:" + host
}

// Middleware rejects with 429 when the key's bucket is empty. onLimited is optional.
func Middleware(l *Limiter, key KeyFunc, onLimited func(r *http.Request)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.Allow(key(r)) {
				if onLimited != nil {
					onLimited(r)
				}
				w.Header().Set("Retry-After", "1")
				http.Error(w, `{"error":"rate limited","code":"rate_limited"}`, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
