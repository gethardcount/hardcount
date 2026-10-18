package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// TestAuthLimiter_KeepsLimitingWithoutRedis covers the only login throttle:
// the per-address limiter on /v1/auth. It used to let every request through
// whenever Redis was unconfigured or a Redis call failed, so an outage turned
// password guessing unlimited. It now counts in the process instead.
func TestAuthLimiter_KeepsLimitingWithoutRedis(t *testing.T) {
	const perMin = 5
	t.Setenv("VELOX_RATE_LIMIT_GENERAL_PER_MIN", "5")

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
			srv := NewServer(&postgres.DB{}, nil, tc.rdb)

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
