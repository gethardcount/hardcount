package subscription

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// ADR-097 regressions for FireDueScheduledCancel — the executor the billing
// engine calls when a cancel_at fell strictly inside a billing period (the
// FLOW TC8 live find: the sub renewed straight past its own cancellation).

func dueCancelSub(t *testing.T, store *memStore, cancelAt time.Time) domain.Subscription {
	t.Helper()
	periodStart := time.Date(2027, 8, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2028, 8, 1, 0, 0, 0, 0, time.UTC)
	sub := domain.Subscription{
		ID: "sub_due", TenantID: "t1", CustomerID: "cus_1", Code: "due-cancel",
		Status:                    domain.SubscriptionActive,
		BillingTime:               domain.BillingTimeCalendar,
		CurrentBillingPeriodStart: &periodStart,
		CurrentBillingPeriodEnd:   &periodEnd,
		NextBillingAt:             &periodEnd,
		CancelAt:                  &cancelAt,
	}
	store.subs[sub.ID] = sub
	return sub
}

func TestFireDueScheduledCancel_FlipsAndBillsAtomically(t *testing.T) {
	store := newMemStore()
	cancelAt := time.Date(2027, 8, 16, 0, 0, 0, 0, time.UTC)
	dueCancelSub(t, store, cancelAt)

	biller := &fakeBiller{
		finalCancelInv: domain.Invoice{ID: "inv_final", Currency: "USD"},
		draftIDs:       []string{"cn_relief"},
		draftCredit:    27500,
	}
	svc := NewService(store, nil)
	svc.SetBiller(biller)

	if err := svc.FireDueScheduledCancel(context.Background(), "t1", "sub_due", cancelAt); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := store.subs["sub_due"]
	if got.Status != domain.SubscriptionCanceled {
		t.Errorf("status: got %q, want canceled", got.Status)
	}
	if got.CanceledAt == nil || !got.CanceledAt.Equal(cancelAt) {
		t.Errorf("canceled_at: got %v, want the contracted instant %v", got.CanceledAt, cancelAt)
	}
	if got.CancelAt != nil {
		t.Error("cancel_at must be cleared after firing")
	}
	if biller.finalCalls != 1 {
		t.Errorf("final partial invoice calls: got %d, want 1", biller.finalCalls)
	}
	if biller.draftTxCalls != 1 {
		t.Errorf("relief draft calls: got %d, want 1", biller.draftTxCalls)
	}
	if biller.finalizeCalls != 1 {
		t.Errorf("post-commit finalize calls: got %d, want 1", biller.finalizeCalls)
	}
	if biller.finishCalls != 1 || len(biller.finishedDrafts) != 1 {
		t.Errorf("post-commit FinishRelief: calls %d drafts %v, want 1 call with the relief draft", biller.finishCalls, biller.finishedDrafts)
	}
	if biller.afterCommitCalls != 1 {
		t.Errorf("post-commit unpriced count: got %d, want 1 (#981)", biller.afterCommitCalls)
	}
}

// A billing failure inside the tx must roll the flip back — the sub stays
// active with its cancel_at intact, so the scan re-returns it (self-healing
// re-entry) rather than leaving a canceled sub with an unbilled stub.
func TestFireDueScheduledCancel_BillFailureRollsBackFlip(t *testing.T) {
	store := newMemStore()
	cancelAt := time.Date(2027, 8, 16, 0, 0, 0, 0, time.UTC)
	dueCancelSub(t, store, cancelAt)

	biller := &fakeBiller{finalCancelErr: context.DeadlineExceeded}
	svc := NewService(store, nil)
	svc.SetBiller(biller)

	if err := svc.FireDueScheduledCancel(context.Background(), "t1", "sub_due", cancelAt); err == nil {
		t.Fatal("expected the billing failure to surface")
	}
	got := store.subs["sub_due"]
	if got.Status != domain.SubscriptionActive {
		t.Errorf("flip must roll back on billing failure: status %q", got.Status)
	}
	if got.CancelAt == nil || !got.CancelAt.Equal(cancelAt) {
		t.Errorf("cancel_at must survive the rollback: %v", got.CancelAt)
	}
	if biller.finalizeCalls != 0 || biller.finishCalls != 0 {
		t.Error("no post-commit legs may run when the tx failed")
	}
}

// Benign races return nil and leave state to whichever writer won:
// (a) schedule changed under us (unschedule / re-schedule) — CAS on
// cancel_at equality defeats the fire, sub stays active;
// (b) already terminated (operator immediate-cancel won) — no double bill.
func TestFireDueScheduledCancel_BenignRaces(t *testing.T) {
	t.Run("unscheduled concurrently", func(t *testing.T) {
		store := newMemStore()
		cancelAt := time.Date(2027, 8, 16, 0, 0, 0, 0, time.UTC)
		sub := dueCancelSub(t, store, cancelAt)
		// Operator unschedules before the fire lands.
		s := store.subs[sub.ID]
		s.CancelAt = nil
		store.subs[sub.ID] = s

		biller := &fakeBiller{}
		svc := NewService(store, nil)
		svc.SetBiller(biller)

		if err := svc.FireDueScheduledCancel(context.Background(), "t1", "sub_due", cancelAt); err != nil {
			t.Fatalf("benign race must return nil, got: %v", err)
		}
		if got := store.subs["sub_due"].Status; got != domain.SubscriptionActive {
			t.Errorf("sub must stay active after a defeated fire: %q", got)
		}
		if biller.finalCalls != 0 || biller.draftTxCalls != 0 {
			t.Error("no billing may run when the CAS lost")
		}
	})

	t.Run("already canceled by operator", func(t *testing.T) {
		store := newMemStore()
		cancelAt := time.Date(2027, 8, 16, 0, 0, 0, 0, time.UTC)
		sub := dueCancelSub(t, store, cancelAt)
		now := time.Date(2027, 9, 1, 0, 0, 0, 0, time.UTC)
		s := store.subs[sub.ID]
		s.Status = domain.SubscriptionCanceled
		s.CanceledAt = &now
		store.subs[sub.ID] = s

		biller := &fakeBiller{}
		svc := NewService(store, nil)
		svc.SetBiller(biller)

		if err := svc.FireDueScheduledCancel(context.Background(), "t1", "sub_due", cancelAt); err != nil {
			t.Fatalf("benign race must return nil, got: %v", err)
		}
		if biller.finalCalls != 0 {
			t.Error("the CAS guarantees exactly one biller — the loser must not bill")
		}
	})
}

// A relief refusal is an InvalidState too, but not a race: it must surface
// (the scan reports it and retries) and the flip must roll back. It used to be
// swallowed as a "benign race" — the subscription stayed active past its
// cancel_at with nothing but an INFO line.
func TestFireDueScheduledCancel_ReliefRefusalSurfaces(t *testing.T) {
	store := newMemStore()
	cancelAt := time.Date(2027, 8, 16, 0, 0, 0, 0, time.UTC)
	dueCancelSub(t, store, cancelAt)

	biller := &fakeBiller{draftTxErr: errs.InvalidState("invoice changed while it was being voided; retry").WithCode("invoice_changed")}
	svc := NewService(store, nil)
	svc.SetBiller(biller)

	if err := svc.FireDueScheduledCancel(context.Background(), "t1", "sub_due", cancelAt); err == nil {
		t.Fatal("a relief refusal must surface, not be skipped as a benign race")
	}
	if got := store.subs["sub_due"]; got.Status != domain.SubscriptionActive || got.CancelAt == nil {
		t.Errorf("flip must roll back on a relief refusal: status %q cancel_at %v", got.Status, got.CancelAt)
	}
	// The billing run retries a refused cancel on backoff (ADR-119). The
	// unpriced-usage count runs only after a commit, so a retry does not add
	// the same usage again (#981).
	if biller.afterCommitCalls != 0 {
		t.Errorf("unpriced count ran %d time(s) for a rolled-back cancel, want 0", biller.afterCommitCalls)
	}
}
