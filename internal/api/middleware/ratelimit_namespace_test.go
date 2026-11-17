package middleware

import (
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
