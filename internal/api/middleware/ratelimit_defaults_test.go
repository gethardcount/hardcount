package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/config"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// The shipped limits, counted for real. CI has no Redis, so the in-process
// count (SetLocalFallback) stands in for it: it allows what Redis allows.
//
// One dashboard page sends about 10 requests, and every operator of an
// account shares its per-mode bucket. At the old general default, 100 a
// minute, one operator was refused after about 10 pages. 200 requests in a
// burst (20 pages) must pass now; sign-in keeps 100 a minute per address.
func TestDefaultLimits_DashboardBurstPassesSignInStaysAt100(t *testing.T) {
	limits := config.DefaultRateLimits()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	general := NewRateLimiter(nil, "general", limits.GeneralPerMinute, time.Minute)
	general.SetLocalFallback()
	dashboard := general.Middleware()(ok)
	for i := 1; i <= 200; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/customers", nil)
		ctx := auth.WithTenantID(req.Context(), "ten_dash")
		req = req.WithContext(postgres.WithLivemode(ctx, false))
		rec := httptest.NewRecorder()
		dashboard.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("dashboard request %d of 200: status %d, want 200", i, rec.Code)
		}
	}

	signIn := NewRateLimiter(nil, "auth", limits.AuthPerMinute, time.Minute)
	signIn.SetLocalFallback()
	login := signIn.Middleware()(ok)
	for i := 1; i <= 101; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
		req.RemoteAddr = "198.51.100.30:4000"
		rec := httptest.NewRecorder()
		login.ServeHTTP(rec, req)
		want := http.StatusOK
		if i == 101 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("sign-in attempt %d from one address: status %d, want %d", i, rec.Code, want)
		}
	}
}

// Retry-After says when the next request passes, not when the whole bucket
// is full again. At 5 a minute one request refills every 12 seconds; the
// header used to say 61 (the period plus one), so a client that honored it
// waited a minute for a request it could have sent after 12 seconds.
func TestRateLimiter_RetryAfterIsTheNextRequest(t *testing.T) {
	rl := NewRateLimiter(nil, "general", 5, time.Minute)
	rl.SetLocalFallback()
	h := rl.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

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
}
