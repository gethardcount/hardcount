package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/subscription"
)

// seedUsageAt stores one usage event of qty stamped at ts, as an event that
// reaches Hardcount whenever the test calls this.
func seedUsageAt(t *testing.T, f *thresholdFixture, ctx context.Context, qty int64, ts time.Time) {
	t.Helper()
	tx, err := f.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`SET LOCAL app.livemode = 'off'`); err != nil {
		t.Fatalf("set local livemode: %v", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, timestamp, livemode)
		VALUES ($1, $2, $3, $4, $5, $6, false)
	`, postgres.NewID("hc_ue"), f.tenantID, f.customerID, f.meterID, qty, ts.UTC().Truncate(time.Microsecond)); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit usage: %v", err)
	}
}

// Usage reaches Hardcount some time after it happens. A period's invoice waits
// INVOICE_GRACE_PERIOD for it (ADR-120). A spend-threshold invoice waited
// nothing: it billed usage up to the scan instant and made that instant the
// watermark, and every close bills sum usage only from the watermark on. So an
// event stamped just before the fire that arrived just after it was on no
// invoice, at every fire, while the customer was spending fastest.
//
// The fire now reads and bills usage up to one grace before the scan. An
// event stamped inside that grace is after the watermark, and the close bills
// it, whether it was stored before the fire or after.
func TestThresholdFire_UsageInFlightIsBilledAtTheClose(t *testing.T) {
	f := newThresholdFixture(t, "Threshold Fire Grace")
	ctx := postgres.WithLivemode(context.Background(), false)
	const grace = time.Hour
	f.engine.SetInvoiceGrace(grace)

	if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, subscription.BillingThresholdsInput{AmountGTE: 5000}); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}
	f.ingestUsage(t, ctx, 6, 1000) // 6,000 calls at 1c = $60 >= $50, stamped days ago
	// On time: stamped half an hour before the scan and already stored at
	// the fire. It is inside the grace, so the fire leaves it to the close.
	seedUsageAt(t, f, ctx, 300, time.Now().Add(-30*time.Minute))

	scanAt := time.Now().UTC()
	fired, errs := f.engine.ScanThresholds(ctx, 50)
	if len(errs) > 0 || fired != 1 {
		t.Fatalf("threshold scan: fired=%d errs=%v, want 1 fire", fired, errs)
	}
	invs := f.listInvoices(t, ctx)
	if len(invs) != 1 {
		t.Fatalf("fire invoices: got %d, want 1", len(invs))
	}
	fire := invs[0]
	if fireTotal, _ := usageLineTotal(t, f, ctx, fire.ID); fireTotal != 6000 {
		t.Fatalf("fire usage = %d, want 6000", fireTotal)
	}
	wantEnd := scanAt.Add(-grace)
	if fire.BillingPeriodEnd.Before(wantEnd.Add(-time.Second)) || fire.BillingPeriodEnd.After(wantEnd.Add(10*time.Second)) {
		t.Errorf("the fire's window ends at %s, want one grace before the scan (about %s)", fire.BillingPeriodEnd.UTC().Format(time.RFC3339), wantEnd.Format(time.RFC3339))
	}

	// In flight at the fire: stamped half an hour before the scan, stored
	// after it. And one event after the fire, so the close always has a line.
	seedUsageAt(t, f, ctx, 500, scanAt.Add(-30*time.Minute))
	seedUsageAt(t, f, ctx, 100, time.Now())

	time.Sleep(50 * time.Millisecond) // the last event is before the period's end
	endPeriodNow(t, ctx, f, f.subID)
	f.engine.SetInvoiceGrace(0) // close the ended period at once
	generated, failures := f.engine.RunCycleForTenant(ctx, f.tenantID, 50)
	if len(failures) > 0 || generated != 1 {
		t.Fatalf("cycle: generated=%d failures=%v, want 1", generated, failures)
	}
	var closeTotal int64 = -1
	for _, inv := range f.listInvoices(t, ctx) {
		if inv.ID != fire.ID {
			closeTotal, _ = usageLineTotal(t, f, ctx, inv.ID)
		}
	}
	if closeTotal != 900 {
		t.Errorf("close usage = %d, want 900: the 300 calls stored before the fire but inside its grace, the 500 in flight at it, and the 100 after it. Each call is on exactly one invoice (6,000 + 900 = all 6,900)", closeTotal)
	}
}

// A crossing made by usage that is still inside the grace is not seen yet:
// the fire reads usage one grace late. With no grace the same subscription
// fires at once, so the wait is the grace and nothing else. By the amount cap
// and by an item cap, in a period that began days ago; and in a period that
// began inside the grace, whose window has not opened at all. (The scan skips
// that last case before reading anything; the evaluation would cross nothing
// there anyway, so the case does not pin the skip.)
func TestThresholdFire_ACrossingInsideTheGraceWaits(t *testing.T) {
	amount := func(*thresholdFixture) subscription.BillingThresholdsInput {
		return subscription.BillingThresholdsInput{AmountGTE: 5000}
	}
	item := func(f *thresholdFixture) subscription.BillingThresholdsInput {
		return subscription.BillingThresholdsInput{ItemThresholds: []subscription.ItemThresholdInput{{SubscriptionItemID: f.itemID, UsageGTE: "5000"}}}
	}
	for _, tc := range []struct {
		name        string
		periodBegan time.Duration // before now; 0 keeps the fixture's (days ago)
		thresholds  func(*thresholdFixture) subscription.BillingThresholdsInput
	}{
		{"amount cap, the period began days ago", 0, amount},
		{"item cap, the period began days ago", 0, item},
		{"amount cap, the period began inside the grace", 10 * time.Minute, amount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newThresholdFixture(t, "Threshold Grace Wait "+tc.name)
			ctx := postgres.WithLivemode(context.Background(), false)
			f.engine.SetInvoiceGrace(time.Hour)

			if tc.periodBegan > 0 {
				start := time.Now().UTC().Add(-tc.periodBegan).Truncate(time.Microsecond)
				tx, err := f.db.BeginTx(ctx, postgres.TxBypass, "")
				if err != nil {
					t.Fatalf("begin bypass: %v", err)
				}
				if _, err := tx.Exec(`UPDATE subscriptions SET current_billing_period_start = $1 WHERE id = $2`, start, f.subID); err != nil {
					t.Fatalf("move period start: %v", err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatalf("commit period start: %v", err)
				}
			}
			if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, tc.thresholds(f)); err != nil {
				t.Fatalf("set thresholds: %v", err)
			}
			// The cap was crossed five minutes ago.
			seedUsageAt(t, f, ctx, 6000, time.Now().Add(-5*time.Minute))

			if fired, errs := f.engine.ScanThresholds(ctx, 50); len(errs) > 0 || fired != 0 {
				t.Fatalf("scan inside the grace: fired=%d errs=%v, want no fire yet", fired, errs)
			}
			if invs := f.listInvoices(t, ctx); len(invs) != 0 {
				t.Fatalf("invoices = %d, want none: the usage that crossed is still inside the grace", len(invs))
			}

			f.engine.SetInvoiceGrace(0)
			if fired, errs := f.engine.ScanThresholds(ctx, 50); len(errs) > 0 || fired != 1 {
				t.Fatalf("scan with no grace: fired=%d errs=%v, want 1 fire", fired, errs)
			}
			if invs := f.listInvoices(t, ctx); len(invs) != 1 || invs[0].BillingReason != domain.BillingReasonThreshold {
				t.Fatalf("want the one threshold invoice, got %d", len(invs))
			}
		})
	}
}

// otherSubscriptionLeftTheMeter gives the customer a second subscription on the
// fixture's plan that stopped covering the meter at endedAt: canceled then,
// its billing interval on the plan closed then. It billed the customer's usage
// of the meter up to endedAt. The fixture's subscription is the one that has
// the meter now; its period began before endedAt (ADR-012 backdates a period
// to the day's start).
func otherSubscriptionLeftTheMeter(t *testing.T, f *thresholdFixture, ctx context.Context, endedAt time.Time) {
	t.Helper()
	endedAt = endedAt.UTC().Truncate(time.Microsecond)
	tx, err := f.db.BeginTx(ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	subA, itemA := postgres.NewID("hc_sub"), postgres.NewID("hc_si")
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, billing_time,
		                           canceled_at, created_at, updated_at)
		VALUES ($1, $2, 'code-left-the-meter', 'Left The Meter', $3, 'canceled', 'anniversary', $4, $5, $4)`,
		subA, f.tenantID, f.customerID, endedAt, f.cycleStart.Add(-24*time.Hour)); err != nil {
		t.Fatalf("insert the other subscription: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5)`, itemA, f.tenantID, subA, f.planID, f.cycleStart.Add(-24*time.Hour)); err != nil {
		t.Fatalf("insert its item: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, ends_at, source)
		VALUES ($1, $2, $3, $4, 1, $5, $6, 'create')`, f.tenantID, subA, itemA, f.planID, f.cycleStart.Add(-24*time.Hour), endedAt); err != nil {
		t.Fatalf("insert its billing interval: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// A customer moves a meter from one subscription to another: subscription A
// stops covering it ten minutes ago, and A billed the usage up to then. The
// fixture's subscription took the meter after that, and its period began
// days ago. Its windows open where A's coverage ended (the usage floor), so a
// fire bills none of A's usage, by the amount cap or by an item cap.
//
// The fire reads usage up to one grace before the scan. Built only up to
// there, the windows missed A's coverage end (it is after that instant), and
// the fire billed, and counted toward the cap, the usage A had billed.
func TestThresholdFire_AMeterAnotherSubscriptionBilledIsNotBilledAgain(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(f *thresholdFixture) subscription.BillingThresholdsInput
	}{
		{"amount cap", func(*thresholdFixture) subscription.BillingThresholdsInput {
			return subscription.BillingThresholdsInput{AmountGTE: 5000}
		}},
		{"item cap", func(f *thresholdFixture) subscription.BillingThresholdsInput {
			return subscription.BillingThresholdsInput{ItemThresholds: []subscription.ItemThresholdInput{{SubscriptionItemID: f.itemID, UsageGTE: "5000"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newThresholdFixture(t, "Threshold Meter Moved "+tc.name)
			ctx := postgres.WithLivemode(context.Background(), false)
			f.engine.SetInvoiceGrace(time.Hour)
			if _, err := f.subSvc.SetBillingThresholds(ctx, f.tenantID, f.subID, tc.set(f)); err != nil {
				t.Fatalf("set thresholds: %v", err)
			}
			f.ingestUsage(t, ctx, 6, 1000) // 6,000 calls, days ago: A's to bill
			otherSubscriptionLeftTheMeter(t, f, ctx, time.Now().Add(-10*time.Minute))

			if fired, errs := f.engine.ScanThresholds(ctx, 50); len(errs) > 0 || fired != 0 {
				t.Fatalf("threshold scan: fired=%d errs=%v, want no fire: the 6,000 calls were the other subscription's", fired, errs)
			}
			if invs := f.listInvoices(t, ctx); len(invs) != 0 {
				t.Fatalf("invoices = %d, want none", len(invs))
			}
		})
	}
}
