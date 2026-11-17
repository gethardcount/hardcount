package middleware

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/config"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/request/requesttest"
)

// With no Redis the outage mode decides: FailClosed refuses every request,
// and FailOpen lets every one through with the limit headers still set.
func TestRateLimiter_OutageModeWithoutRedis(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode OutageMode
		want int
	}{
		{"FailClosed", FailClosed, http.StatusTooManyRequests},
		{"FailOpen", FailOpen, http.StatusOK},
	} {
		rl := NewRateLimiter(nil, "test", 1, time.Minute, tc.mode)
		h := requesttest.Root(rl.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})))
		for i := 1; i <= 5; i++ {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/customers", nil))
			if rec.Code != tc.want {
				t.Fatalf("%s, request %d at a limit of 1: status %d, want %d", tc.name, i, rec.Code, tc.want)
			}
			if tc.mode == FailOpen && (rec.Header().Get("X-RateLimit-Limit") != "1" || rec.Header().Get("X-RateLimit-Remaining") == "") {
				t.Errorf("%s, request %d: X-RateLimit-Limit %q, X-RateLimit-Remaining %q; want 1 and a count",
					tc.name, i, rec.Header().Get("X-RateLimit-Limit"), rec.Header().Get("X-RateLimit-Remaining"))
			}
		}
	}
}

// Retry-After says when the next request passes, not when the whole bucket
// is full again: at 5 a minute one request refills every 12 seconds. This is
// the in-process count, which AllowKey shares with the middleware.
func TestRateLimiter_RetryAfterIsTheNextRequest(t *testing.T) {
	rl := NewRateLimiter(nil, "general", 5, time.Minute, CountLocally)
	h := requesttest.Root(rl.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))

	var rec *httptest.ResponseRecorder
	for range 6 {
		req := httptest.NewRequest(http.MethodGet, "/v1/customers", nil)
		req.RemoteAddr = "198.51.100.31:4000"
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request 6: status %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "12" {
		t.Errorf("Retry-After = %q, want 12 (one request refills every 12s)", got)
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want 0", got)
	}
	if _, _, ok := rl.AllowKey(t.Context(), "customer:c1"); !ok {
		t.Fatal("AllowKey on a fresh key: denied, want allowed")
	}
}

// rateLimitKey picks the bucket a request spends from: its API key, else its
// tenant in its mode, else its client address. Without the mode, clicking
// through the dashboard in test mode could spend the quota of a live payment
// retry. A key wins over its tenant, so one runaway integration cannot starve
// the tenant's dashboard. Behind a trusted proxy the address is the client's
// as TrustedClientIP resolved it, or every proxied client would share one
// bucket.
func TestRateLimitKey_PerModeTenantBuckets(t *testing.T) {
	trusted := TrustedClientIP(trustedNets(t, "10.0.0.0/8"))
	for _, tc := range []struct {
		name   string
		caller auth.Caller
		live   bool
		xff    string // the request comes from the trusted proxy 10.0.0.2
		want   string
	}{
		{"a tenant in test mode", auth.Caller{TenantID: "ten_abc"}, false, "", "tenant:ten_abc:test"},
		{"the same tenant in live mode", auth.Caller{TenantID: "ten_abc"}, true, "", "tenant:ten_abc:live"},
		{"an API key wins over its tenant", auth.Caller{TenantID: "ten_abc", KeyID: "hc_key_123"}, true, "", "key:hc_key_123"},
		{"a client behind the proxy", auth.Caller{}, false, "198.51.100.7", "ip:198.51.100.7"},
		{"another client behind the same proxy gets its own bucket", auth.Caller{}, false, "198.51.100.8", "ip:198.51.100.8"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/customers", nil)
		req.RemoteAddr = "10.0.0.2:443"
		if tc.xff != "" {
			req.Header.Set("X-Forwarded-For", tc.xff)
		}
		if tc.caller != (auth.Caller{}) {
			req = req.WithContext(postgres.WithLivemode(auth.WithCaller(req.Context(), tc.caller), tc.live))
		}
		var got string
		var err error
		trusted(requesttest.Root(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got, err = rateLimitKey(r)
		}))).ServeHTTP(httptest.NewRecorder(), req)
		if err != nil || got != tc.want {
			t.Errorf("%s: bucket %q (err %v), want %q", tc.name, got, err, tc.want)
		}
	}
}

// A tenant request whose ctx has no mode is a bug at its entry point. The key
// is refused, never guessed into the live bucket.
func TestRateLimitKey_TenantWithoutModeIsAnError(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/customers", nil)
	_, err := rateLimitKey(r.WithContext(auth.WithTenantID(r.Context(), "ten_abc")))
	if !errors.Is(err, postgres.ErrLivemodeUnset) {
		t.Fatalf("err = %v, want postgres.ErrLivemodeUnset", err)
	}
}

// trustedNets parses a TRUST_PROXY value with config's parser.
func trustedNets(t *testing.T, spec string) []*net.IPNet {
	t.Helper()
	nets, err := config.ParseTrustProxy(spec)
	if err != nil {
		t.Fatal(err)
	}
	return nets
}

// Two limiters keyed on the same identifier never share a Redis bucket, or a
// request to one surface would spend another's allowance.
func TestRateLimiterBucketKey_NamespacedByLimiter(t *testing.T) {
	general := NewRateLimiter(nil, "general", 100, time.Minute, FailOpen)
	hosted := NewRateLimiter(nil, "hosted_invoice", 60, time.Minute, FailOpen)

	id := "ip:1.2.3.4"
	gk := general.bucketKey(id)
	hk := hosted.bucketKey(id)

	if gk == hk {
		t.Fatalf("limiters with different names share a bucket key (%q) — GCRA state collides", gk)
	}
	if gk != "rl:general:ip:1.2.3.4" {
		t.Errorf("general key = %q, want rl:general:ip:1.2.3.4", gk)
	}
	if hk != "rl:hosted_invoice:ip:1.2.3.4" {
		t.Errorf("hosted key = %q, want rl:hosted_invoice:ip:1.2.3.4", hk)
	}
}

// An empty URL opens nothing and is no error. A malformed URL and an
// unreachable server are errors, and the ping ends with the caller's ctx, so
// boot never hangs on it.
func TestOpenRedis(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if c, err := OpenRedis(ctx, ""); c != nil || err != nil {
		t.Fatalf("empty url: want (nil, nil), got (%v, %v)", c, err)
	}
	if _, err := OpenRedis(ctx, "://not-a-url"); err == nil || !strings.Contains(err.Error(), "invalid REDIS_URL") {
		t.Fatalf("malformed url: want invalid REDIS_URL error, got %v", err)
	}
	start := time.Now()
	_, err := OpenRedis(ctx, "redis://127.0.0.1:1") // closed port
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("closed port: want unreachable error, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("unreachable Redis took %v — boot must not hang on the ping", time.Since(start))
	}
}
