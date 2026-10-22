package subscription

import (
	"context"
	"database/sql"
	"sort"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// ListCustomerMeterCoverageEnds is the fact the usage window's floor is built
// from (ADR-012 amendment 2026-10-22): where the customer's OTHER
// subscriptions stopped covering each meter inside a window. Every row below
// is written by the real writers (create, remove, plan change, cancel); the
// query has to tell the endings that count from those that do not.
func TestListCustomerMeterCoverageEnds(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Coverage Ends")
	cust := ivCustomer(t, db, ctx, tenantID, "cus_cov")
	otherCust := ivCustomer(t, db, ctx, tenantID, "cus_cov_other")
	plan := func(code string, meters ...string) string {
		t.Helper()
		p, err := pricing.NewPostgresStore(db).CreatePlan(ctx, tenantID, domain.Plan{
			Code: code, Name: code, Currency: "USD", BillingInterval: domain.BillingMonthly,
			BaseBillTiming: domain.BillInArrears, Status: domain.PlanActive, MeterIDs: meters,
		})
		if err != nil {
			t.Fatalf("create plan %s: %v", code, err)
		}
		return p.ID
	}
	planM := plan("cov-m", "mtr_m")
	planN := plan("cov-n", "mtr_n")
	planNone := plan("cov-none")
	store := NewPostgresStore(db)
	day := func(n int) time.Time { return ivPS.Add(time.Duration(n) * 24 * time.Hour) }
	at := func(n int) context.Context { return clock.WithEffectiveNow(ctx, day(n)) }
	items := func(planIDs ...string) []domain.SubscriptionItem {
		var out []domain.SubscriptionItem
		for _, id := range planIDs {
			out = append(out, domain.SubscriptionItem{PlanID: id, Quantity: 1})
		}
		return out
	}
	cancel := func(subID string, n int) {
		t.Helper()
		if _, err := store.CancelAtomic(at(n), tenantID, subID); err != nil {
			t.Fatalf("cancel %s: %v", subID, err)
		}
	}

	// The subscription asking: its own ends never count.
	self := ivActiveSub(t, store, ctx, tenantID, cust, "cov-self", items(planM))
	cancel(self.ID, 5)

	// Counted: an item removal (day 3), an immediate cancel (day 7), a plan
	// change away from the meter (day 9), a never-activated trial canceled
	// (day 11).
	rm := ivActiveSub(t, store, ctx, tenantID, cust, "cov-rm", items(planM, planNone))
	if err := store.RemoveItem(at(3), tenantID, rm.Items[0].ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	cxl := ivActiveSub(t, store, ctx, tenantID, cust, "cov-cxl", items(planM))
	cancel(cxl.ID, 7)
	swap := ivActiveSub(t, store, ctx, tenantID, cust, "cov-swap", items(planM))
	if _, err := store.ApplyItemPlanImmediately(ctx, tenantID, swap.Items[0].ID, planN, day(9)); err != nil {
		t.Fatalf("plan change: %v", err)
	}
	trialEnd := ivPE.AddDate(0, 0, 3)
	trial, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "cov-trial", DisplayName: "cov-trial", CustomerID: cust,
		Status: domain.SubscriptionTrialing, BillingTime: domain.BillingTimeAnniversary,
		TrialStartAt: &ivPS, TrialEndAt: &trialEnd, StartedAt: &ivPS,
		CurrentBillingPeriodStart: &trialEnd, CurrentBillingPeriodEnd: bivPE(trialEnd), NextBillingAt: bivPE(trialEnd),
		Items: items(planM),
	})
	if err != nil {
		t.Fatalf("create trial: %v", err)
	}
	cancel(trial.ID, 11)

	// Not counted: a still-open interval; a canceled draft (it never covered
	// anything); another customer's cancel; a cancel past the period end
	// (sealed AT the period end, the window's bound); a zero-width row (an
	// item added on day 10 under a cancel backdated to day 8).
	ivActiveSub(t, store, ctx, tenantID, cust, "cov-open", items(planN))
	draft, err := store.Create(ctx, tenantID, domain.Subscription{
		Code: "cov-draft", DisplayName: "cov-draft", CustomerID: cust,
		Status: domain.SubscriptionDraft, BillingTime: domain.BillingTimeCalendar, Items: items(planM),
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	cancel(draft.ID, 12)
	other := ivActiveSub(t, store, ctx, tenantID, otherCust, "cov-other", items(planM))
	cancel(other.ID, 6)
	late := ivActiveSub(t, store, ctx, tenantID, cust, "cov-late", items(planM))
	cancel(late.ID, 40)
	zw := ivActiveSub(t, store, ctx, tenantID, cust, "cov-zw", items(planNone))
	if _, err := store.AddItem(at(10), tenantID, domain.SubscriptionItem{SubscriptionID: zw.ID, PlanID: planM, Quantity: 1}); err != nil {
		t.Fatalf("add item: %v", err)
	}
	fresh, err := store.Get(ctx, tenantID, zw.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := store.WithTenantTx(ctx, tenantID, func(tx *sql.Tx) error {
		_, err := store.FireScheduledCancellationTx(ctx, tx, tenantID, zw.ID, SnapshotOf(fresh), day(8))
		return err
	}); err != nil {
		t.Fatalf("backdated cancel: %v", err)
	}

	read := func(from, to time.Time) []string {
		t.Helper()
		got, err := store.ListCustomerMeterCoverageEnds(ctx, tenantID, cust, self.ID, from, to)
		if err != nil {
			t.Fatalf("list coverage ends: %v", err)
		}
		var out []string
		for _, e := range got {
			out = append(out, e.MeterID+"@"+e.EndsAt.UTC().Format("Jan 2"))
		}
		sort.Strings(out)
		return out
	}
	want := []string{"mtr_m@May 10", "mtr_m@May 12", "mtr_m@May 4", "mtr_m@May 8"}
	if got := read(ivPS, ivPE); !equalStrings(got, want) {
		t.Errorf("coverage ends in [%v, %v) = %v, want %v", ivPS, ivPE, got, want)
	}
	// The window is open at both ends: an end exactly at `from` is not in it.
	if got := read(day(7), ivPE); !equalStrings(got, []string{"mtr_m@May 10", "mtr_m@May 12"}) {
		t.Errorf("coverage ends from the day-7 cancel's own instant = %v, want the day-9 and day-11 ends only", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
