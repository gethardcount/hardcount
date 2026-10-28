package subscription

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestActivate_StoreUpdate_GuardsAgainstConcurrentCancel is the real-Postgres proof
// of the Activate lost-update guard. Service.Activate reads a draft (store.Get),
// checks status==draft, then persists the transition via ActivateDraftWithBill in a
// SEPARATE step. That writer's UPDATE carries `AND status = 'draft'` so a
// draft→canceled cancel that commits in the TOCTOU window is NOT clobbered back to
// active — which would resurrect a terminated subscription into a live billing state
// with fresh period bounds and fire subscription.activated on it. (The guard
// originally lived in the store's Update method; when Update lost its last caller
// and was deleted, this test retargeted to the live writer.)
//
// Only a real DB proves it: the in-memory test fake replaces the whole struct and
// cannot model the WHERE predicate, so a unit test would pass with or without the
// guard.
func TestActivate_StoreUpdate_GuardsAgainstConcurrentCancel(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Activate Guard")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_act_guard", DisplayName: "Act Guard",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
		Code: "act-guard-monthly-adv", Name: "Act Guard", Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance,
		BaseAmountCents: 5000, Status: domain.PlanActive,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	store := NewPostgresStore(db)

	newDraft := func(code string) domain.Subscription {
		s, err := store.Create(ctx, tenantID, domain.Subscription{
			Code: code, DisplayName: code, CustomerID: cust.ID,
			Status: domain.SubscriptionDraft, BillingTime: domain.BillingTimeCalendar,
			Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		})
		if err != nil {
			t.Fatalf("create draft %s: %v", code, err)
		}
		return s
	}

	at := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	pe := time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)

	// Positive: a genuine draft still activates. The guard must not block the valid
	// transition — and the in-memory fake can't prove this since it ignores the WHERE.
	valid := newDraft("sub-act-ok")
	activated, err := store.ActivateDraftWithBill(ctx, tenantID, valid.ID, at, at, pe, 1, nil)
	if err != nil {
		t.Fatalf("ActivateDraftWithBill on a genuine draft must succeed: %v", err)
	}
	if activated.Status != domain.SubscriptionActive {
		t.Fatalf("draft must activate: status=%q, want active", activated.Status)
	}

	// Guard: a draft canceled out-of-band (the TOCTOU window between Activate's Get and
	// this write) must NOT be resurrected to active.
	raced := newDraft("sub-act-raced")
	if _, err := store.CancelAtomic(ctx, tenantID, raced.ID); err != nil {
		t.Fatalf("cancel the draft: %v", err)
	}
	if _, err := store.ActivateDraftWithBill(ctx, tenantID, raced.ID, at, at, pe, 1, nil); err == nil {
		t.Fatal("activating a concurrently-canceled sub must FAIL, not clobber it back to active")
	} else if !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("want an InvalidState conflict, got %v", err)
	}

	after, err := store.Get(ctx, tenantID, raced.ID)
	if err != nil {
		t.Fatalf("get after guarded update: %v", err)
	}
	if after.Status != domain.SubscriptionCanceled {
		t.Fatalf("a concurrently-canceled sub must STAY canceled, not be resurrected; status=%q", after.Status)
	}
}
