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

// step is one call after the commit: which, and whether its context was live.
type step struct {
	name string
	live bool
}

type stepResolver struct {
	steps       *[]step
	resolutions []domain.DunningResolution
}

func (r *stepResolver) ResolveByInvoice(ctx context.Context, _, _ string, resolution domain.DunningResolution) error {
	*r.steps = append(*r.steps, step{"resolve dunning", ctx.Err() == nil})
	r.resolutions = append(r.resolutions, resolution)
	return nil
}

type stepStopper struct{ steps *[]step }

func (*stepStopper) StopCollection(context.Context, string, domain.Invoice) {}

func (s *stepStopper) ExpireCheckoutSessions(ctx context.Context, _ string, _ domain.Invoice) {
	*s.steps = append(*s.steps, step{"expire checkout", ctx.Err() == nil})
}

// An offline payment closes the invoice's dunning run itself, before the
// Stripe expiry, which can take minutes, and on a context the caller cannot
// cancel. An escalated run is closed by nothing else, so a run left open on a
// paid invoice would stay escalated.
//
// Mutations: drop the resolve → no "resolve dunning" step; resolve on the
// caller's context → the step sees an ended one; resolve after the expiry →
// the order is wrong.
func TestRecordOfflinePayment_ClosesDunningBeforeTheExpiry(t *testing.T) {
	db := testutil.SetupTestDB(t)
	base := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Offline Payment Closes Dunning")
	inv := seedClaimableInvoice(t, db, base, tenantID, "INV-OFFLINE-DUNNING")

	ctx, cancel := context.WithCancel(base)
	defer cancel()
	var steps []step
	resolver := &stepResolver{steps: &steps}
	svc := invoice.NewService(invoice.NewPostgresStore(db), nil, nil)
	svc.SetDunningResolver(resolver)
	svc.SetCollectionStopper(&stepStopper{steps: &steps})
	// The caller's context ends while the audit row is written, after the
	// commit and before both steps.
	svc.SetAuditLogger(endingAudit{AuditLogger: audit.NewLogger(db), endCaller: cancel})

	if _, err := svc.RecordOfflinePayment(ctx, tenantID, inv.ID, ""); err != nil {
		t.Fatalf("record offline payment: %v", err)
	}
	want := []step{{"resolve dunning", true}, {"expire checkout", true}}
	if len(steps) != len(want) || steps[0] != want[0] || steps[1] != want[1] {
		t.Errorf("steps after the commit = %+v, want %+v", steps, want)
	}
	if len(resolver.resolutions) != 1 || resolver.resolutions[0] != domain.ResolutionPaymentRecovered {
		t.Errorf("resolutions = %v, want [payment_recovered]", resolver.resolutions)
	}
}
