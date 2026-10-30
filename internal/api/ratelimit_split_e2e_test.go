package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestE2E_DashboardBrowsingIsNotRefusedSignInStillIs drives the real router
// and Postgres with the shipped limits. One dashboard page sends about 10
// requests, so at the old general limit (100 a minute per account and mode)
// one operator was refused after about 10 pages. 200 session reads in a row,
// 20 pages, must all pass on the general bucket, while sign-in keeps its own
// 100 a minute per address: the 101st attempt is refused.
//
// CI has no Redis. Without it the general limiter lets requests through
// outside production, so this test checks which limit each route carries
// (X-RateLimit-Limit); the general count itself is exercised in
// middleware.TestDefaultLimits_DashboardBurstPassesSignInStaysAt100. The
// sign-in limiter counts in the process without Redis, so its 429 here is
// real.
func TestE2E_DashboardBrowsingIsNotRefusedSignInStillIs(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "ratelimit-split-bootstrap")
	// The test client is the trusted proxy, so X-Forwarded-For sets each
	// request's client address.
	t.Setenv("TRUST_PROXY", "127.0.0.1/32")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const email, password = "owner@ratelimit-split.test", "ratelimit-split-pw-12"
	flowBootstrap(t, ts, "ratelimit-split-bootstrap", email, password)
	owner := login(t, ts, email, password)

	send := func(method, path, body string, prepare func(*http.Request)) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		prepare(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_ = resp.Body.Close()
		return resp
	}

	for i := 1; i <= 200; i++ {
		resp := send(http.MethodGet, "/v1/customers", "", func(r *http.Request) { r.AddCookie(owner) })
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("dashboard read %d of 200: status %d, want 200", i, resp.StatusCode)
		}
		if got := resp.Header.Get("X-RateLimit-Limit"); got != "1000" {
			t.Fatalf("dashboard read %d: X-RateLimit-Limit = %q, want the general limit 1000", i, got)
		}
	}

	// An invalid body reaches the login handler, which answers 400 without
	// a password check, so the sign-in throttle (per email) never counts and
	// any 429 is the per-address limiter's.
	for i := 1; i <= 101; i++ {
		resp := send(http.MethodPost, "/v1/auth/login", "{", func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", "203.0.113.9")
		})
		if got := resp.Header.Get("X-RateLimit-Limit"); got != "100" {
			t.Fatalf("sign-in attempt %d: X-RateLimit-Limit = %q, want the auth limit 100", i, got)
		}
		want := http.StatusBadRequest
		if i == 101 {
			want = http.StatusTooManyRequests
		}
		if resp.StatusCode != want {
			t.Fatalf("sign-in attempt %d from one address: status %d, want %d", i, resp.StatusCode, want)
		}
	}
}
