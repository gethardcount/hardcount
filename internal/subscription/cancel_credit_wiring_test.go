package subscription

import (
	"context"
	"errors"
	"testing"
)

// seedActiveSub creates an active in_advance-style sub for cancel wiring tests.
func seedActiveSub(t *testing.T, svc *Service, fb *fakeBiller) string {
	t.Helper()
	created, err := svc.Create(context.Background(), "t1", CreateInput{
		Code: "sub-cancel-wire", DisplayName: "Wire", CustomerID: "cus_1",
		Items: []CreateItemInput{{PlanID: "pln_1"}}, StartNow: true,
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	// Neutralize the create-path counters so the cancel assertions are clean.
	fb.createTxCalls = 0
	fb.draftTxCalls = 0
	return created.ID
}

// TestCancel_ReliefWrittenInTxThenFinished: Cancel writes the relief in the
// cancel tx, runs its post-commit legs (FinishRelief) once with those drafts,
// and reports the relief's balance credit. There is no other relief path.
func TestCancel_ReliefWrittenInTxThenFinished(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newMemStore(), nil)
	fb := &fakeBiller{}
	svc.SetBiller(fb)
	id := seedActiveSub(t, svc, fb)

	fb.draftIDs = []string{"cn_draft_1"}
	fb.draftCredit = 5000

	_, credit, err := svc.Cancel(ctx, "t1", id)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if credit != 5000 {
		t.Errorf("proration credit = %d, want 5000 (the relief's balance credit)", credit)
	}
	if fb.draftTxCalls != 1 {
		t.Errorf("BillOnCancelDraftsTx calls = %d, want 1", fb.draftTxCalls)
	}
	if fb.finishCalls != 1 || len(fb.finishedDrafts) != 1 || fb.finishedDrafts[0] != "cn_draft_1" {
		t.Errorf("FinishRelief not run with the drafts; calls=%d ids=%v", fb.finishCalls, fb.finishedDrafts)
	}
	if fb.afterCommitCalls != 1 {
		t.Errorf("unpriced count ran %d time(s) after the committed cancel, want 1 (#981)", fb.afterCommitCalls)
	}
}

// TestCancel_DraftError_FailsCancel: a draft-create failure inside the cancel tx
// surfaces as a Cancel error (the billFn returns it → CancelAtomicWithBill rolls
// back), never a canceled sub with a silently-lost credit.
func TestCancel_DraftError_FailsCancel(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newMemStore(), nil)
	fb := &fakeBiller{}
	svc.SetBiller(fb)
	id := seedActiveSub(t, svc, fb)

	fb.draftTxErr = errors.New("draft insert failed")

	if _, _, err := svc.Cancel(ctx, "t1", id); err == nil {
		t.Fatal("a draft-create failure must fail the cancel (so the tx rolls back)")
	}
	if fb.finishCalls != 0 {
		t.Errorf("FinishRelief ran %d time(s) after a rolled-back cancel, want 0", fb.finishCalls)
	}
	if fb.afterCommitCalls != 0 {
		t.Errorf("unpriced count ran %d time(s) after a rolled-back cancel, want 0 (#981)", fb.afterCommitCalls)
	}
}
