package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/gethardcount/hardcount/internal/config"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestAuthLimiter_KeepsLimitingWithoutRedis covers the only login throttle:
// the per-address limiter on /v1/auth. It used to let every request through
// whenever Redis was unconfigured or a Redis call failed, so an outage turned
// password guessing unlimited. It now counts in the process instead.
func TestAuthLimiter_KeepsLimitingWithoutRedis(t *testing.T) {
	const perMin = 5
	limits := config.DefaultRateLimits()
	limits.AuthPerMinute = perMin

	// Nothing listens on port 1, and retries are off, so every Redis call
	// fails at once, as during an outage after boot.
	down := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		MaxRetries:  -1,
		DialTimeout: 200 * time.Millisecond,
	})
	t.Cleanup(func() { _ = down.Close() })

	for _, tc := range []struct {
		name string
		rdb  *redis.Client
	}{
		{"redis unreachable", down},
		{"redis not configured", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(&postgres.DB{}, nil, tc.rdb, WithRateLimits(limits))

			// An invalid body reaches the login handler, which answers 400
			// without touching the database, so 400 means "not limited".
			login := func(addr string) int {
				req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader("{"))
				req.RemoteAddr = addr
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)
				return rec.Code
			}

			for i := 1; i <= perMin; i++ {
				if got := login("198.51.100.7:4000"); got != http.StatusBadRequest {
					t.Fatalf("request %d of %d: status %d, want 400 (within the limit)", i, perMin, got)
				}
			}
			if got := login("198.51.100.7:4001"); got != http.StatusTooManyRequests {
				t.Fatalf("request %d from one address: status %d, want 429", perMin+1, got)
			}
			// Control: the count is per address, so another address is
			// still served.
			if got := login("198.51.100.8:4000"); got != http.StatusBadRequest {
				t.Fatalf("first request from another address: status %d, want 400", got)
			}
		})
	}
}

// Sign-in and first setup count per address on the auth limit, not the
// general one. The general limit was raised for the dashboard (2026-10-31);
// had /v1/auth or /v1/bootstrap kept it, password guessing would have got
// ten times looser with it.
func TestPreSignInRoutes_UseTheAuthLimit(t *testing.T) {
	limits := config.RateLimits{GeneralPerMinute: 1000, AuthPerMinute: 3, IngestPerSecond: 1000}
	srv := NewServer(&postgres.DB{}, nil, nil, WithRateLimits(limits))

	// An invalid body is refused with 400 before either handler touches the
	// database, after the limiter has run and set its headers.
	send := func(path, addr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{"))
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/v1/auth/login", "/v1/bootstrap"} {
		rec := send(path, "198.51.100.20:4000")
		if got := rec.Header().Get("X-RateLimit-Limit"); got != "3" {
			t.Errorf("%s: X-RateLimit-Limit = %q, want the auth limit 3 (the general limit is 1000)", path, got)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", path, rec.Code)
		}
	}

	// The auth limit is enforced on /v1/auth without Redis: 3 pass, the 4th
	// from that address is refused.
	for i := 1; i <= 3; i++ {
		if got := send("/v1/auth/login", "198.51.100.21:4000").Code; got != http.StatusBadRequest {
			t.Fatalf("login %d of 3: status %d, want 400", i, got)
		}
	}
	if got := send("/v1/auth/login", "198.51.100.21:4000").Code; got != http.StatusTooManyRequests {
		t.Fatalf("login 4: status %d, want 429", got)
	}
}
