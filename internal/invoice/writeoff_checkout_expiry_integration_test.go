package invoice_test

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// stopperRecorder records both Stripe legs: StopCollection (a void's) and
// ExpireCheckoutSessions (a write-off's).
type stopperRecorder struct{ stopped, expired []string }

func (r *stopperRecorder) StopCollection(_ context.Context, _ string, inv domain.Invoice) {
	r.stopped = append(r.stopped, inv.ID)
}

func (r *stopperRecorder) ExpireCheckoutSessions(_ context.Context, _ string, inv domain.Invoice) {
	r.expired = append(r.expired, inv.ID)
}

// A write-off expires the invoice's open Stripe Checkout sessions, with the
// same call and the same best-effort handling a void uses. The write-off's
// transaction already closed Velox's checkout claims, but the session itself
// stayed payable at Stripe until it expired (up to an hour), so a customer
// with the page open could still pay a written-off invoice (ADR-110 closes
// self-service payment on one). A write-off does not cancel the
// PaymentIntent: only a void does. Every write-off goes through
// Service.MarkUncollectible (the operator endpoint, dunning's final action
// and dunning's invoice_not_collectible resolve), so it is tested here.
//
// Mutation: drop the expiry call from MarkUncollectible → expired is empty.
func TestMarkUncollectible_ExpiresCheckoutSessions(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Write-off Checkout Expiry")
	store := invoice.NewPostgresStore(db)
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-WO-EXPIRE")

	rec := &stopperRecorder{}
	svc := invoice.NewService(store, nil, nil)
	svc.SetCollectionStopper(rec)

	if _, err := svc.MarkUncollectible(ctx, tenantID, inv.ID); err != nil {
		t.Fatalf("write off: %v", err)
	}
	if len(rec.expired) != 1 || rec.expired[0] != inv.ID {
		t.Errorf("ExpireCheckoutSessions calls = %v, want exactly one for %s: a written-off invoice must not stay payable on an open Checkout page", rec.expired, inv.ID)
	}
	if len(rec.stopped) != 0 {
		t.Errorf("StopCollection calls = %v, want none: a write-off does not cancel the PaymentIntent", rec.stopped)
	}

	// A refused write-off (already written off) expires nothing.
	if _, err := svc.MarkUncollectible(ctx, tenantID, inv.ID); err == nil {
		t.Fatal("second write-off succeeded, want refused")
	}
	if len(rec.expired) != 1 {
		t.Errorf("ExpireCheckoutSessions calls after a refused write-off = %v, want still one", rec.expired)
	}
}
