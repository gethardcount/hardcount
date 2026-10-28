package invoice

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// A manual invoice's issued_at and due_at anchor to the FINALIZE moment, not
// draft-create (Stripe finalized_at semantics): a draft composed at T1 and
// finalized later at T2 carries issued_at=T2 and due_at=T2+term. This stops the
// invoice-activity timeline from showing "Created" and "Finalized" at the same
// instant, and runs Net terms from issuance.
func TestFinalize_ManualInvoice_AnchorsIssuedAndDueToFinalize(t *testing.T) {
	t1 := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	t2 := t1.AddDate(0, 0, 5) // operator finalizes 5 days after composing the draft
	fake := clock.NewFake(t1)
	svc := NewService(newMemStore(), fake, newMemNumberer())
	ctx := context.Background()

	inv, err := svc.Create(ctx, "t1", CreateInput{
		CustomerID:         "cus_1",
		NetPaymentTermDays: intPtr(14),
		LineItems:          []AddLineItemInput{{Description: "Consulting", Quantity: 1, UnitAmountCents: 10000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Draft is issued at create time, due = create + term.
	if inv.IssuedAt == nil || !inv.IssuedAt.Equal(t1) {
		t.Fatalf("draft issued_at: got %v, want %v", inv.IssuedAt, t1)
	}

	// Operator finalizes later — advance the (test) clock.
	fake.Set(t2)
	final, err := svc.Finalize(ctx, "t1", inv.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if final.IssuedAt == nil || !final.IssuedAt.Equal(t2) {
		t.Errorf("finalized issued_at: got %v, want %v (re-stamped to the finalize moment)", final.IssuedAt, t2)
	}
	wantDue := t2.AddDate(0, 0, 14)
	if final.DueAt == nil || !final.DueAt.Equal(wantDue) {
		t.Errorf("finalized due_at: got %v, want %v (issued + Net 14)", final.DueAt, wantDue)
	}
}

// "Due on receipt" (0 days) survives finalize re-stamping: due_at == issued_at
// at the finalize moment.
func TestFinalize_ManualInvoice_DueOnReceiptDueEqualsIssued(t *testing.T) {
	t1 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.AddDate(0, 0, 2)
	fake := clock.NewFake(t1)
	svc := NewService(newMemStore(), fake, newMemNumberer())
	ctx := context.Background()

	inv, err := svc.Create(ctx, "t1", CreateInput{
		CustomerID:         "cus_1",
		NetPaymentTermDays: intPtr(0), // Due on receipt
		LineItems:          []AddLineItemInput{{Description: "One-off", Quantity: 1, UnitAmountCents: 5000}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	fake.Set(t2)
	final, err := svc.Finalize(ctx, "t1", inv.ID)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if final.IssuedAt == nil || final.DueAt == nil || !final.DueAt.Equal(*final.IssuedAt) {
		t.Errorf("due-on-receipt: due_at (%v) must equal issued_at (%v) at finalize", final.DueAt, final.IssuedAt)
	}
	if !final.IssuedAt.Equal(t2) {
		t.Errorf("issued_at: got %v, want %v", final.IssuedAt, t2)
	}
}
