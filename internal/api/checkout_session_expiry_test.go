package api

import (
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/payment"
)

// TestCheckoutSessionExpiry_FixedAtClaimCreation: every re-drive of a claim
// replays the same Stripe idempotency key, and Stripe rejects a replay whose
// params changed. The expiry therefore depends on the claim alone, never on
// when the call runs.
func TestCheckoutSessionExpiry_FixedAtClaimCreation(t *testing.T) {
	created := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	claim := payment.CheckoutClaim{ID: "vlx_cks_1", CreatedAt: created}
	if got, want := checkoutSessionExpiry(claim), created.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("expiry = %v, want claim creation + 1h (%v)", got, want)
	}
}
