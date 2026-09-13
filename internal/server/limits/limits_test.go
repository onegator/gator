package limits

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiterPerKey(t *testing.T) {
	l := NewLimiter(1, 2)
	if !l.Allow("a") || !l.Allow("a") {
		t.Fatal("burst of 2 should pass")
	}
	if l.Allow("a") {
		t.Fatal("third immediate call should be limited")
	}
	if !l.Allow("b") {
		t.Fatal("other key has its own bucket")
	}
}

func TestMiddlewareReturns429(t *testing.T) {
	l := NewLimiter(1, 1)
	limited := 0
	h := Middleware(l, ByBearerOrIP, func(*http.Request) { limited++ })(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer x")
	for i, want := range []int{200, 429} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("call %d: %d want %d", i, rec.Code, want)
		}
	}
	if limited != 1 {
		t.Fatalf("onLimited called %d times", limited)
	}
}

func TestBreakerTripsCoolsAndRecovers(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewBreaker(3, time.Minute)
	b.now = func() time.Time { return now }
	trips := 0
	b.OnTrip = func() { trips++ }
	boom := errors.New("boom")

	for i := 0; i < 3; i++ {
		if err := b.Do(func() error { return boom }); !errors.Is(err, boom) {
			t.Fatal(err)
		}
	}
	if b.State() != Open || trips != 1 {
		t.Fatalf("after 3 failures: %s trips=%d", b.State(), trips)
	}
	if err := b.Do(func() error { return nil }); !errors.Is(err, ErrOpen) {
		t.Fatalf("open breaker ran fn: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if b.State() != HalfOpen {
		t.Fatalf("after cooldown: %s", b.State())
	}
	if err := b.Do(func() error { return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if b.State() != Open {
		t.Fatalf("failed probe should reopen: %s", b.State())
	}
	now = now.Add(2 * time.Minute)
	if err := b.Do(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if b.State() != Closed {
		t.Fatalf("successful probe should close: %s", b.State())
	}
}
