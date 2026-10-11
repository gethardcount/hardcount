package subscription

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// Bug B transplant wiring (2026-07-05), completed by ADR-118: the
// cross-interval swap refund is written on the swap tx (BillOnPlanSwapDraftsTx)
// for every funding invoice, paid or unpaid, and its legs run post-commit in
// FinalizeCrossIntervalSwap. There is no post-commit fallback: a crash between
// the swap commit and a post-commit refund lost the customer's unused
// prepayment for good (retry 400s on the same-plan guard; no reconciler
// re-derives a missed swap refund).

func newSwapWiringSvc(t *testing.T, fb *fakeBiller) (*Service, domain.Subscription) {
	t.Helper()
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	svc := NewService(newMemStore(), clock.NewFake(now))
	svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
		"p_yearly_adv":  {ID: "p_yearly_adv", BillingInterval: domain.BillingYearly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 120000},
		"p_monthly_adv": {ID: "p_monthly_adv", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 12000},
	}})
	svc.SetBiller(fb)
	savedErr, savedOK := fb.createTxErr, fb.createTxOK
	fb.createTxErr, fb.createTxOK = nil, true
	sub, err := svc.Create(context.Background(), "t1", CreateInput{
		Code: "s-swapwire", DisplayName: "n", CustomerID: "c",
		Items:       []CreateItemInput{{PlanID: "p_yearly_adv"}},
		BillingTime: domain.BillingTimeAnniversary,
		StartNow:    true,
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	fb.createTxErr, fb.createTxOK = savedErr, savedOK
	fb.createTxCalls, fb.swapDraftTxCalls, fb.finishCalls = 0, 0, 0
	return svc, sub
}

func TestCrossIntervalSwap_ReliefWrittenInTxThenFinished(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBiller{createTxOK: true, swapDraftIDs: []string{"cn_swap_1"}}
	svc, sub := newSwapWiringSvc(t, fb)

	res, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
		NewPlanID: "p_monthly_adv", Immediate: true,
	})
	if err != nil {
		t.Fatalf("UpdateItemTx: %v", err)
	}
	if fb.swapDraftTxCalls != 1 {
		t.Errorf("BillOnPlanSwapDraftsTx calls = %d, want 1 (the refund must ride the swap tx)", fb.swapDraftTxCalls)
	}
	if len(res.swapRelief.DraftIDs) != 1 {
		t.Fatalf("result must carry the in-tx relief: %+v", res.swapRelief)
	}

	svc.FinalizeCrossIntervalSwap(ctx, "t1", sub, res)
	if fb.finishCalls != 1 || len(fb.finishedDrafts) != 1 || fb.finishedDrafts[0] != "cn_swap_1" {
		t.Errorf("FinishRelief not run with the relief; calls=%d ids=%v", fb.finishCalls, fb.finishedDrafts)
	}
}

func TestCrossIntervalSwap_DraftError_FailsSwap(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBiller{createTxOK: true, swapDraftTxErr: errors.New("draft insert failed")}
	svc, sub := newSwapWiringSvc(t, fb)

	if _, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
		NewPlanID: "p_monthly_adv", Immediate: true,
	}); err == nil {
		t.Fatal("a draft-create failure must fail the swap (tx rolls back — never a swapped sub with a silently-lost refund)")
	}
	// Drafts run FIRST on the tx, so the plan write must not have happened.
	after, err := svc.store.Get(ctx, "t1", sub.ID)
	if err != nil {
		t.Fatalf("re-read sub: %v", err)
	}
	if after.Items[0].PlanID != "p_yearly_adv" {
		t.Errorf("plan must be unchanged after a failed swap; got %q", after.Items[0].PlanID)
	}
}
