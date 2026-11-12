package invoice_test

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// An offline payment expires the invoice's open Stripe Checkout sessions, as
// a write-off does: the money has arrived, and a page the customer still has
// open would take it a second time. It is in the service, so dunning's
// "payment recovered" resolve, which records an offline payment, expires the
// page too. A refused payment expires nothing.
//
// Mutation: drop the expiry call from RecordOfflinePayment → expired is empty.
func TestRecordOfflinePayment_ExpiresCheckoutSessions(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Offline Payment Checkout Expiry")
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-OFFLINE-EXPIRE")

	rec := &stopperRecorder{}
	svc := invoice.NewService(invoice.NewPostgresStore(db), nil, nil)
	svc.SetCollectionStopper(rec)

	if _, err := svc.RecordOfflinePayment(ctx, tenantID, inv.ID, "wire"); err != nil {
		t.Fatalf("record offline payment: %v", err)
	}
	if len(rec.expired) != 1 || rec.expired[0] != inv.ID {
		t.Errorf("ExpireCheckoutSessions calls = %v, want exactly one for %s", rec.expired, inv.ID)
	}
	if len(rec.stopped) != 0 {
		t.Errorf("StopCollection calls = %v, want none: the payment is recorded, not canceled", rec.stopped)
	}

	if _, err := svc.RecordOfflinePayment(ctx, tenantID, inv.ID, "wire again"); err == nil {
		t.Fatal("second offline payment succeeded, want refused (already paid)")
	}
	if len(rec.expired) != 1 {
		t.Errorf("ExpireCheckoutSessions calls after a refused payment = %v, want still one", rec.expired)
	}
}

// The expiry runs on a context the caller cannot cancel: a request that times
// out, or a client that disconnects, after the payment committed must not
// leave the page payable.
//
// Mutation: hand the expiry the caller's context → the call sees an ended one.
func TestRecordOfflinePayment_CallerContextEnds_ExpiryStillRuns(t *testing.T) {
	db := testutil.SetupTestDB(t)
	base := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Offline Payment Caller Ends")
	inv := seedClaimableInvoice(t, db, base, tenantID, "INV-OFFLINE-ENDS")

	ctx, cancel := context.WithCancel(base)
	defer cancel()
	stopper := &endingStopper{endCaller: cancel}
	svc := invoice.NewService(invoice.NewPostgresStore(db), nil, nil)
	svc.SetCollectionStopper(stopper)
	// The caller's context ends while the audit row is written, just before
	// the expiry.
	svc.SetAuditLogger(endingAudit{AuditLogger: audit.NewLogger(db), endCaller: cancel})

	got, err := svc.RecordOfflinePayment(ctx, tenantID, inv.ID, "")
	if err != nil {
		t.Fatalf("record offline payment: %v", err)
	}
	if got.Status != domain.InvoicePaid {
		t.Fatalf("status = %s, want paid", got.Status)
	}
	if len(stopper.liveCtx) != 1 || !stopper.liveCtx[0] {
		t.Errorf("Stripe expiry calls with a live context = %v, want [true]: a call on an ended context leaves the page payable", stopper.liveCtx)
	}
}
