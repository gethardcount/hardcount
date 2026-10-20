package middleware

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis_rate/v10"
)

var t0 = time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

// The in-process count must allow what the Redis GCRA allows: the burst at
// once, then one request per period/rate, with the same remaining and reset
// values in the response headers.
func TestLocalLimiter_MatchesRedisLimit(t *testing.T) {
	l := newLocalLimiter(redis_rate.PerMinute(5)) // one request per 12s, burst 5

	for i := 1; i <= 5; i++ {
		remaining, resetAt, ok := l.allow("ip:a", t0)
		if !ok {
			t.Fatalf("request %d: denied, want allowed", i)
		}
		if remaining != 5-i {
			t.Errorf("request %d: remaining %d, want %d", i, remaining, 5-i)
		}
		if want := t0.Add(time.Duration(i) * 12 * time.Second); !resetAt.Equal(want) {
			t.Errorf("request %d: reset %v, want %v", i, resetAt, want)
		}
	}
	if remaining, _, ok := l.allow("ip:a", t0); ok || remaining != 0 {
		t.Fatalf("request 6: allowed=%v remaining=%d, want denied with 0 left", ok, remaining)
	}
	if _, _, ok := l.allow("ip:a", t0.Add(11900*time.Millisecond)); ok {
		t.Fatal("11.9s later: allowed, want denied until a token refills at 12s")
	}
	if _, _, ok := l.allow("ip:a", t0.Add(12100*time.Millisecond)); !ok {
		t.Fatal("12.1s later: denied, want one refilled token")
	}
	if _, _, ok := l.allow("ip:b", t0.Add(12100*time.Millisecond)); !ok {
		t.Fatal("another key: denied, want its own full bucket")
	}
}

// sweep drops only buckets that have refilled. A bucket still refilling keeps
// its count, so the sweep never hands out extra requests.
func TestLocalLimiter_SweepDropsOnlyFullBuckets(t *testing.T) {
	l := newLocalLimiter(redis_rate.PerMinute(5))

	l.allow("ip:idle", t0) // refilled 12s later
	for range 5 {
		l.allow("ip:busy", t0.Add(50*time.Second)) // empty; refills at 110s
	}
	l.allow("ip:new", t0.Add(61*time.Second)) // a period after the first sweep: sweeps

	keys := make([]string, 0, len(l.buckets))
	for k := range l.buckets {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"ip:busy", "ip:new"}; !slices.Equal(keys, want) {
		t.Fatalf("buckets after sweep: %v, want %v", keys, want)
	}
	if _, _, ok := l.allow("ip:busy", t0.Add(61*time.Second)); ok {
		t.Fatal("ip:busy after the sweep: allowed, want still limited")
	}
}

// Concurrent requests for one key share one count: exactly the burst passes.
func TestLocalLimiter_ConcurrentRequestsShareOneCount(t *testing.T) {
	l := newLocalLimiter(redis_rate.PerMinute(100))

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 10 {
				if _, _, ok := l.allow("ip:a", t0); ok {
					allowed.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if got := allowed.Load(); got != 100 {
		t.Fatalf("allowed %d of 200 concurrent requests, want 100", got)
	}
}

// With SetLocalFallback and no Redis, the middleware limits and answers 429
// with the usual headers. It replaces fail-closed, so the first requests
// still pass.
func TestRateLimiter_LocalFallbackWithoutRedis(t *testing.T) {
	rl := NewRateLimiter(nil, "general", 2, time.Minute)
	rl.SetFailClosed(true)
	rl.SetLocalFallback()
	h := rl.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		req.RemoteAddr = "198.51.100.7:4000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for i := 1; i <= 2; i++ {
		if rec := send(); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i, rec.Code)
		}
	}
	rec := send()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request 3: status %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want 0", got)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}

	// AllowKey shares the decision, so a handler-side bucket gets the same
	// fallback.
	if _, _, ok := rl.AllowKey(t.Context(), "customer:c1"); !ok {
		t.Fatal("AllowKey on a fresh key: denied, want allowed")
	}
}
