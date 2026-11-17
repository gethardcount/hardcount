package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRateLimiterBucketKey_NamespacedByLimiter covers the pass-3 low audit
// finding: distinct limiters (general / hosted-invoice / setup-link) keyed on
// the same identifier (e.g. "ip:1.2.3.4") collided on one Redis bucket, so a
// request to one surface consumed another's GCRA allowance. The per-limiter
// name now namespaces the key.
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

// TestSplitRateLimit_RoutesByPath: the ingest surface must ride its own
// (much larger) bucket — the general 100/min CRUD bucket 429'd LiteLLM
// callbacks at >1.7 calls/s, and LiteLLM only retries on 5xx, so every
// 429 was a silently dropped revenue event. The split is observable via
// the X-RateLimit-Limit header each limiter stamps.
func TestSplitRateLimit_RoutesByPath(t *testing.T) {
	general := NewRateLimiter(nil, "general", 100, time.Minute, FailOpen)
	ingest := NewRateLimiter(nil, "ingest", 1000, time.Second, FailOpen)
	isIngest := func(r *http.Request) bool {
		return strings.HasPrefix(r.URL.Path, "/v1/usage-events") ||
			strings.HasPrefix(r.URL.Path, "/v1/integrations/litellm")
	}
	h := SplitRateLimit(isIngest, ingest, general)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	limitFor := func(path string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		return rec.Header().Get("X-RateLimit-Limit")
	}

	for path, want := range map[string]string{
		"/v1/usage-events":               "1000",
		"/v1/usage-events/batch":         "1000",
		"/v1/usage-events/backfill":      "1000",
		"/v1/integrations/litellm/spend": "1000",
		"/v1/customers":                  "100",
		"/v1/invoices":                   "100",
	} {
		if got := limitFor(path); got != want {
			t.Errorf("%s: X-RateLimit-Limit = %q, want %q", path, got, want)
		}
	}
}
