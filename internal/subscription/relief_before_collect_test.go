package subscription

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
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

// The caller's context can end right after the change commits: the API
// request's timeout or a client disconnect for Cancel, the billing run
// stopping (shutdown, lost lease) for a scheduled cancel, the dunning tick
// stopping for dunning's cancel. The post-commit legs must still run in full.
// Collection already ignores the caller's cancellation (CollectInvoice); if
// the relief does not, its Issue fails on the first database call, the note
// waits for RetryPendingClawbackIssue, and collection charges the whole final
// invoice with the credit left on the balance: #1272 again (#1286 review).
var wantLegsLive = []error{nil, nil}

func TestCancel_PostCommitLegsOutliveCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc := NewService(newMemStore(), nil)
	fb := &fakeBiller{}
	svc.SetBiller(fb)
	id := seedActiveSub(t, svc, fb)
	fb.postCommit, fb.postCommitCtxErr = nil, nil
	fb.finalCancelInv = domain.Invoice{ID: "inv_final", Currency: "USD"}
	fb.draftIDs = []string{"cn_relief"}
	fb.draftCredit = 2450
	fb.onAfterCommit = cancel // the request ends as the cancel commits

	if _, _, err := svc.Cancel(ctx, "t1", id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !slices.Equal(fb.postCommit, wantReliefThenFinalize) || !slices.Equal(fb.postCommitCtxErr, wantLegsLive) {
		t.Errorf("post-commit legs %v saw ctx.Err() = %v, want %v: the caller's cancellation must not reach them", fb.postCommit, fb.postCommitCtxErr, wantLegsLive)
	}
}

func TestFireDueScheduledCancel_PostCommitLegsOutliveCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newMemStore()
	cancelAt := time.Date(2027, 8, 16, 0, 0, 0, 0, time.UTC)
	dueCancelSub(t, store, cancelAt)
	fb := &fakeBiller{
		finalCancelInv: domain.Invoice{ID: "inv_final", Currency: "USD"},
		draftIDs:       []string{"cn_relief"},
		draftCredit:    2450,
		onAfterCommit:  cancel, // the billing run stops as the cancel commits
	}
	svc := NewService(store, nil)
	svc.SetBiller(fb)

	if err := svc.FireDueScheduledCancel(ctx, "t1", "sub_due", cancelAt); err != nil {
		t.Fatalf("FireDueScheduledCancel: %v", err)
	}
	if !slices.Equal(fb.postCommit, wantReliefThenFinalize) || !slices.Equal(fb.postCommitCtxErr, wantLegsLive) {
		t.Errorf("post-commit legs %v saw ctx.Err() = %v, want %v: the caller's cancellation must not reach them", fb.postCommit, fb.postCommitCtxErr, wantLegsLive)
	}
}

func TestFinalizeCrossIntervalSwap_PostCommitLegsOutliveCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the swap's request ended after its transaction committed
	fb := &fakeBiller{}
	svc := NewService(newMemStore(), nil)
	svc.SetBiller(fb)
	inv := domain.Invoice{ID: "inv_final"}

	svc.FinalizeCrossIntervalSwap(ctx, "t1", domain.Subscription{ID: "sub_1", TenantID: "t1"}, ItemChangeResult{
		swapRelief:          domain.Relief{DraftIDs: []string{"cn_relief"}},
		crossAxisNewInvoice: &inv,
	})
	if !slices.Equal(fb.postCommit, wantReliefThenFinalize) || !slices.Equal(fb.postCommitCtxErr, wantLegsLive) {
		t.Errorf("post-commit legs %v saw ctx.Err() = %v, want %v: the caller's cancellation must not reach them", fb.postCommit, fb.postCommitCtxErr, wantLegsLive)
	}
}
