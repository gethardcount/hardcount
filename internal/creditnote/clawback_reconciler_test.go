package creditnote

import (
	"context"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestRetryPendingClawbackIssue proves the reconciler half of ADR-057: an
// auto-clawback DRAFT whose post-commit Issue() NEVER RAN (status='draft',
// issue_pending — the post-commit crash window) is re-issued on a scheduler
// tick, and once issued it drops out of the scan so a later tick does NOT
// double-apply the (non-idempotent) amount_due reduction.
func TestRetryPendingClawbackIssue(t *testing.T) {
	store := newMemStore()
	invoices := &memInvoiceReader{
		invoices: map[string]domain.Invoice{
			// Unpaid finalized source: Issue() takes the amount_due-reduction
			// leg (the non-idempotent one), so a second tick double-applying
			// would show as a further drop below 5000.
			"inv_1": {
				ID: "inv_1", TenantID: "t1", CustomerID: "cus_1",
				Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
				Currency: "USD", TotalAmountCents: 10000, AmountDueCents: 10000,
			},
		},
	}
	svc := NewService(store, invoices, nil)
	svc.SetNumberGenerator(&fakeCNNumbers{})
	ctx := postgres.WithLivemode(context.Background(), false)

	// Stand in for the in-tx clawback create: a draft marked issue_pending whose
	// post-commit Issue() "never ran" (the common crash-window case).
	draft, err := svc.CreateAdjustmentDraftTx(ctx, nil, "t1", "inv_1", 5000, "subscription_downgrade", "clawback")
	if err != nil {
		t.Fatalf("create clawback draft: %v", err)
	}
	if draft.Status != domain.CreditNoteDraft || !draft.IssuePending {
		t.Fatalf("draft: status=%q issue_pending=%v, want draft+true", draft.Status, draft.IssuePending)
	}

	// First tick: the reconciler finds the pending draft and issues it.
	issued, retryErrs := svc.RetryPendingClawbackIssue(ctx, 100)
	if len(retryErrs) != 0 {
		t.Fatalf("retry errors: %v", retryErrs)
	}
	if issued != 1 {
		t.Fatalf("issued: got %d, want 1", issued)
	}
	got, err := store.Get(ctx, "t1", draft.ID)
	if err != nil {
		t.Fatalf("get after issue: %v", err)
	}
	if got.Status != domain.CreditNoteIssued {
		t.Errorf("status after retry: got %q, want issued", got.Status)
	}
	if due := invoices.invoices["inv_1"].AmountDueCents; due != 5000 {
		t.Errorf("amount_due after issue: got %d, want 5000 (10000 - 5000 CN)", due)
	}

	// Second tick: the now-issued note has left status='draft', so the scan
	// drops it — no re-issue, no double amount_due reduction.
	issued2, retryErrs2 := svc.RetryPendingClawbackIssue(ctx, 100)
	if len(retryErrs2) != 0 {
		t.Fatalf("second retry errors: %v", retryErrs2)
	}
	if issued2 != 0 {
		t.Errorf("second tick issued: got %d, want 0 (issued CN must drop out of the scan)", issued2)
	}
	if due := invoices.invoices["inv_1"].AmountDueCents; due != 5000 {
		t.Errorf("amount_due after second tick: got %d, want 5000 (no double-apply)", due)
	}
}
