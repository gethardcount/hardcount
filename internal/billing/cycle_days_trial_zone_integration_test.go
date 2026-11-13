package billing_test

import (
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// The worked case for the on-create lines (R3): a trial's first paid period
// is written at create, in the tenant's zone then, and billed at trial end in
// the zone in force now. A trial in Asia/Kolkata ending at midnight on Mar 15
// 2027 has the period [Mar 15, Apr 15) IST on anchor 15, which is
// [Mar 14 18:30, Apr 14 18:30) UTC. The tenant then moves to UTC. The period
// is 31 days, one full month from its start, so the trial-end invoice bills
// the whole fee. Before, the cycle was advanced on the stored anchor in UTC,
// to Apr 15 18:30, 32 days, and the invoice billed 31/32 of the fee.
func TestCycleDays_TrialEndAfterAZoneChange_BillsTheFullFee(t *testing.T) {
	r := newCancelRig(t, "trial end after a zone change")
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	trialEnd := time.Date(2027, 3, 15, 0, 0, 0, 0, ist)
	periodEnd := time.Date(2027, 4, 15, 0, 0, 0, 0, ist)
	r.exec(t, `UPDATE plans SET base_bill_timing = 'in_advance' WHERE id = $1`, r.f.planID)
	r.exec(t, `DELETE FROM billing_intervals WHERE subscription_item_id = $1`, r.f.itemID)
	r.exec(t, `UPDATE subscriptions SET status = 'trialing', billing_time = 'anniversary', billing_anchor_day = 15,
		trial_start_at = $1, trial_end_at = $2, activated_at = NULL,
		current_billing_period_start = $2, current_billing_period_end = $3, next_billing_at = $3 WHERE id = $4`,
		trialEnd.AddDate(0, 0, -14), trialEnd, periodEnd, r.f.subID)
	if _, err := r.f.settings.Update(r.base, r.f.tenantID, func(ts *domain.TenantSettings) error {
		ts.Timezone = "UTC"
		return nil
	}); err != nil {
		t.Fatalf("set the zone to UTC: %v", err)
	}

	if n, errs := r.f.subSvc.ProcessExpiredTrials(r.at(trialEnd.Add(time.Hour)), 100); n != 1 || len(errs) != 0 {
		t.Fatalf("trial expiry: flipped=%d errs=%v, want 1 and none", n, errs)
	}
	invs, lines := r.liveInvoices(t)
	inv := invoiceFor(t, invs, trialEnd, periodEnd)
	var base []int64
	for _, li := range lines[inv.ID] {
		if li.LineType == domain.LineTypeBaseFee {
			base = append(base, li.AmountCents)
		}
	}
	if len(base) != 1 || base[0] != cancelFixtureBaseCents {
		t.Errorf("trial-end base lines = %v, want one of the full fee, %d (before: %d × 31/32)", base, cancelFixtureBaseCents, cancelFixtureBaseCents)
	}
}
