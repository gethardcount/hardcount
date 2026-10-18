package subscription

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Issue #1272: after a cancel or cross-interval swap commits, the relief's
// credit notes are issued BEFORE the change's invoice is collected, so the
// relief's balance credit reduces that invoice (ADR-088; ADR-118 amendment of
// 2026-10-19). Collection applies the balance and charges the card in one
// call, so the other order charged the card the whole invoice.
//
// The money outcome is proven on real Postgres in
// internal/billing/cancel_relief_reduces_final_invoice_integration_test.go;
// these pin the order on each of the three callers.

var wantReliefThenFinalize = []string{"relief", "finalize:inv_final"}

func TestCancel_IssuesReliefBeforeCollectingFinalInvoice(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	fb := &fakeBiller{}
	svc.SetBiller(fb)
	id := seedActiveSub(t, svc, fb)
	fb.postCommit = nil
	fb.finalCancelInv = domain.Invoice{ID: "inv_final", Currency: "USD"}
	fb.draftIDs = []string{"cn_relief"}
	fb.draftCredit = 2450

	if _, _, err := svc.Cancel(context.Background(), "t1", id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !slices.Equal(fb.postCommit, wantReliefThenFinalize) {
		t.Errorf("post-commit legs = %v, want %v: the relief must be issued before the final invoice is collected", fb.postCommit, wantReliefThenFinalize)
	}
}

func TestFireDueScheduledCancel_IssuesReliefBeforeCollectingFinalInvoice(t *testing.T) {
	store := newMemStore()
	cancelAt := time.Date(2027, 8, 16, 0, 0, 0, 0, time.UTC)
	dueCancelSub(t, store, cancelAt)
	fb := &fakeBiller{
		finalCancelInv: domain.Invoice{ID: "inv_final", Currency: "USD"},
		draftIDs:       []string{"cn_relief"},
		draftCredit:    2450,
	}
	svc := NewService(store, nil)
	svc.SetBiller(fb)

	if err := svc.FireDueScheduledCancel(context.Background(), "t1", "sub_due", cancelAt); err != nil {
		t.Fatalf("FireDueScheduledCancel: %v", err)
	}
	if !slices.Equal(fb.postCommit, wantReliefThenFinalize) {
		t.Errorf("post-commit legs = %v, want %v: the relief must be issued before the final invoice is collected", fb.postCommit, wantReliefThenFinalize)
	}
}

// The swap already ran in this order; it now shares the helper with both
// cancels, so this keeps it there.
func TestFinalizeCrossIntervalSwap_IssuesReliefBeforeCollectingNewInvoice(t *testing.T) {
	fb := &fakeBiller{}
	svc := NewService(newMemStore(), nil)
	svc.SetBiller(fb)
	inv := domain.Invoice{ID: "inv_final"}

	svc.FinalizeCrossIntervalSwap(context.Background(), "t1", domain.Subscription{ID: "sub_1", TenantID: "t1"}, ItemChangeResult{
		swapRelief:          domain.Relief{DraftIDs: []string{"cn_relief"}},
		crossAxisNewInvoice: &inv,
	})
	if !slices.Equal(fb.postCommit, wantReliefThenFinalize) {
		t.Errorf("post-commit legs = %v, want %v", fb.postCommit, wantReliefThenFinalize)
	}
}
