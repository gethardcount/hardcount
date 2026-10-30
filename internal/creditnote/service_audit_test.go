package creditnote

import (
	"context"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
)

// TestIssue_ReDerivesAllocationWhenInvoicePaidAfterCreate is the regression
// test for the frozen-allocation bug: a CN created against a still-unpaid
// (finalized) invoice skips Create's paid-invoice block, so all three
// allocation channels persist as zero. If that invoice is then PAID before
// the CN is issued, Issue sees a paid invoice but a CN with zero
// allocations — pre-fix, neither the refund leg nor the credit-grant leg
// fired and the CN issued as a silent no-op (the customer's refund/credit
// vanished). Post-fix, Issue re-derives the allocation from the current
// invoice state and defaults to a full credit-balance grant (Create's
// documented paid-invoice default).
func TestIssue_ReDerivesAllocationWhenInvoicePaidAfterCreate(t *testing.T) {
	t.Parallel()

	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			"inv_1": {
				ID:               "inv_1",
				TenantID:         "t1",
				CustomerID:       "cus_1",
				Status:           domain.InvoiceFinalized,
				PaymentStatus:    domain.PaymentPending,
				Currency:         "USD",
				TotalAmountCents: 10000,
				AmountDueCents:   10000,
			},
		},
	}
	granter := &fakeCreditGranter{}
	svc := NewService(store, invoices, nil, granter)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	ctx := context.Background()

	// Create the CN while the invoice is still unpaid → allocation frozen
	// at all-zero (the paid-invoice allocation block is skipped).
	cn, err := svc.Create(ctx, "t1", CreateInput{
		InvoiceID: "inv_1",
		Reason:    "Service credit",
		Lines:     []CreditLineInput{{Description: "adj", Quantity: 1, UnitAmountCents: 4000}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if cn.RefundAmountCents != 0 || cn.CreditAmountCents != 0 || cn.OutOfBandAmountCents != 0 {
		t.Fatalf("precondition: expected frozen-zero allocation on unpaid-invoice CN, got (%d,%d,%d)",
			cn.RefundAmountCents, cn.CreditAmountCents, cn.OutOfBandAmountCents)
	}

	// Invoice gets paid out-of-band before the CN is issued.
	inv := invoices.invoices["inv_1"]
	inv.Status = domain.InvoicePaid
	inv.PaymentStatus = domain.PaymentSucceeded
	inv.AmountPaidCents = 10000
	inv.AmountDueCents = 0
	invoices.invoices["inv_1"] = inv

	issued, err := svc.Issue(ctx, "t1", cn.ID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issued.Status != domain.CreditNoteIssued {
		t.Fatalf("status: got %q, want issued", issued.Status)
	}

	// The credit-grant leg must have fired for the full CN total. Pre-fix
	// it did not (allocation was frozen zero) and the refund/credit was
	// silently dropped.
	if len(granter.cnCalls) != 1 {
		t.Fatalf("credit-grant calls: got %d, want 1 (allocation should have been re-derived to a credit grant)", len(granter.cnCalls))
	}
	if got := granter.cnCalls[0].input.AmountCents; got != cn.TotalCents {
		t.Errorf("granted amount: got %d, want %d (full CN total)", got, cn.TotalCents)
	}
	// ADR-132: on a paid invoice only the part drawn from credit grants goes
	// back to them; the ledger needs to know the invoice was paid.
	if !granter.cnCalls[0].input.InvoicePaid {
		t.Error("credit grant on a paid invoice: InvoicePaid unset, want set")
	}

	// The persisted allocation must reflect the re-derived split so the
	// dashboard and any retry observe what Issue acted on.
	persisted, err := store.Get(ctx, "t1", cn.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if persisted.CreditAmountCents != cn.TotalCents {
		t.Errorf("persisted credit_amount_cents: got %d, want %d", persisted.CreditAmountCents, cn.TotalCents)
	}
	if persisted.RefundAmountCents != 0 || persisted.OutOfBandAmountCents != 0 {
		t.Errorf("persisted refund/oob should stay 0; got refund=%d oob=%d",
			persisted.RefundAmountCents, persisted.OutOfBandAmountCents)
	}
}

// TestVoid_RefusesCreditNoteWithExecutedRefund is the regression test for the
// double-refund bug: Void on a draft CN whose Stripe refund leg already
// executed would drop that refund from the over-refund cap (Create/CreateRefund
// sum RefundAmountCents over non-voided CNs only), letting a later CN refund the
// same money again. Void must refuse when the refund has been processed.
func TestVoid_RefusesCreditNoteWithExecutedRefund(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("refuses when StripeRefundID present", func(t *testing.T) {
		store := newMemStore()
		invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{}}
		svc := NewService(store, invoices, nil)
		svc.SetNumberGenerator(&fakeCNNumbers{})

		// Draft CN that already carries an executed Stripe refund.
		cn, err := store.Create(ctx, "t1", domain.CreditNote{
			InvoiceID:         "inv_1",
			CustomerID:        "cus_1",
			Status:            domain.CreditNoteDraft,
			Reason:            "partial issue",
			TotalCents:        5000,
			RefundAmountCents: 5000,
			RefundStatus:      domain.RefundSucceeded,
			StripeRefundID:    "re_already_done",
			Currency:          "USD",
		})
		if err != nil {
			t.Fatalf("seed CN: %v", err)
		}

		_, err = svc.Void(ctx, "t1", cn.ID)
		if err == nil {
			t.Fatal("expected Void to refuse a CN with an executed refund")
		}
		if !strings.Contains(err.Error(), "refund") {
			t.Errorf("error should explain the refund conflict: %q", err.Error())
		}

		// CN must stay un-voided so its refund keeps counting toward the cap.
		after, err := store.Get(ctx, "t1", cn.ID)
		if err != nil {
			t.Fatalf("Get after refused void: %v", err)
		}
		if after.Status == domain.CreditNoteVoided {
			t.Error("CN was voided despite carrying an executed refund — over-refund cap now under-counts")
		}
	})

	t.Run("refuses when RefundStatus succeeded without explicit id", func(t *testing.T) {
		store := newMemStore()
		invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{}}
		svc := NewService(store, invoices, nil)
		svc.SetNumberGenerator(&fakeCNNumbers{})

		cn, err := store.Create(ctx, "t1", domain.CreditNote{
			InvoiceID:         "inv_2",
			CustomerID:        "cus_1",
			Status:            domain.CreditNoteDraft,
			Reason:            "partial issue",
			TotalCents:        5000,
			RefundAmountCents: 5000,
			RefundStatus:      domain.RefundSucceeded,
			Currency:          "USD",
		})
		if err != nil {
			t.Fatalf("seed CN: %v", err)
		}

		if _, err := svc.Void(ctx, "t1", cn.ID); err == nil {
			t.Fatal("expected Void to refuse a CN whose refund_status is succeeded")
		}
	})

	t.Run("still voids a draft with no executed refund", func(t *testing.T) {
		store := newMemStore()
		invoices := &memInvoiceReader{invoices: map[string]domain.Invoice{}}
		svc := NewService(store, invoices, nil)
		svc.SetNumberGenerator(&fakeCNNumbers{})

		cn, err := store.Create(ctx, "t1", domain.CreditNote{
			InvoiceID:         "inv_3",
			CustomerID:        "cus_1",
			Status:            domain.CreditNoteDraft,
			Reason:            "mistake",
			TotalCents:        5000,
			RefundAmountCents: 5000,
			RefundStatus:      domain.RefundNone,
			Currency:          "USD",
		})
		if err != nil {
			t.Fatalf("seed CN: %v", err)
		}

		voided, err := svc.Void(ctx, "t1", cn.ID)
		if err != nil {
			t.Fatalf("Void of un-refunded draft should succeed: %v", err)
		}
		if voided.Status != domain.CreditNoteVoided {
			t.Errorf("status: got %q, want voided", voided.Status)
		}
	})
}
