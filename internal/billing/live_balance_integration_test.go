package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tax"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// liveFixture is the threshold fixture with the credit ledger wired into the
// engine (so a fire or a close really draws credit) and the public cost
// dashboard assembled over the same stores. Every test reads the live figure
// through estimated(), which asserts the public dashboard and the engine's
// own read return the same number.
type liveFixture struct {
	*thresholdFixture
	ctx       context.Context
	creditSvc *credit.Service
	dashboard *usage.CostDashboardAssembler
	token     string
}

// liveTestCredits adapts the credit service to the dashboard's credit reader,
// as internal/api does in production.
type liveTestCredits struct{ svc *credit.Service }

func (a liveTestCredits) CreditSummary(ctx context.Context, tenantID, customerID string) (usage.CostDashboardCredits, error) {
	bal, err := a.svc.GetBalance(ctx, tenantID, customerID)
	if err != nil {
		return usage.CostDashboardCredits{}, err
	}
	return usage.CostDashboardCredits{BalanceCents: bal.BalanceCents}, nil
}

func newLiveFixture(t *testing.T, name string, grantCents int64) *liveFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newThresholdFixture(t, name)
	ctx := postgres.WithLivemode(context.Background(), false)
	lf := wireLive(t, f, ctx)
	if grantCents > 0 {
		lf.grant(t, grantCents)
	}
	return lf
}

// wireLive replaces the fixture's engine with one that applies credit.
func wireLive(t *testing.T, f *thresholdFixture, ctx context.Context) *liveFixture {
	t.Helper()
	customerStore := customer.NewPostgresStore(f.db)
	creditSvc := credit.NewService(credit.NewPostgresStore(f.db))

	engine := billing.NewEngine(
		f.subAdapter,
		&usageStoreAdapter{usage.NewPostgresStore(f.db)},
		&pricingStoreAdapter{pricing.NewPostgresStore(f.db)},
		f.invAdapter,
		creditSvc, f.settings, testPaymentSetupsNoPM{}, testChargerSentinel{}, nil,
	)
	engine.SetIntervalReader(f.subStore)
	engine.SetTaxProviderResolver(tax.NewResolver(nil))
	engine.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	engine.SetDunningStarter(testDunningStarter{})
	engine.SetDunningResolver(&testDunningResolver{})
	engine.SetLiveBalanceReaders(creditSvc, f.subStore)
	f.engine = engine

	token, err := customer.NewCostDashboardToken()
	if err != nil {
		t.Fatalf("mint dashboard token: %v", err)
	}
	if err := customerStore.SetCostDashboardToken(ctx, f.tenantID, f.customerID, token); err != nil {
		t.Fatalf("set dashboard token: %v", err)
	}
	usageView := usage.NewCustomerUsageService(f.usageSvc, customerStore, f.subStore, f.pricingSvc)
	dashboard := usage.NewCostDashboardAssembler(customerStore, usageView, f.subStore, liveTestCredits{creditSvc})
	dashboard.SetLiveBalanceReader(engine)

	return &liveFixture{thresholdFixture: f, ctx: ctx, creditSvc: creditSvc, dashboard: dashboard, token: token}
}

func (lf *liveFixture) grant(t *testing.T, cents int64) {
	t.Helper()
	if _, err := lf.creditSvc.Grant(lf.ctx, lf.tenantID, credit.GrantInput{
		CustomerID: lf.customerID, AmountCents: cents, Description: "prepaid",
	}); err != nil {
		t.Fatalf("grant credit: %v", err)
	}
}

// balance is the settled ledger balance.
func (lf *liveFixture) balance(t *testing.T) int64 {
	t.Helper()
	bal, err := lf.creditSvc.GetBalance(lf.ctx, lf.tenantID, lf.customerID)
	if err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return bal.BalanceCents
}

// live is the engine's read, the one GET /v1/credits/balance/{id}/live serves.
func (lf *liveFixture) live(t *testing.T) domain.CreditLiveBalance {
	t.Helper()
	got, err := lf.engine.LiveCreditBalance(lf.ctx, lf.tenantID, lf.customerID)
	if err != nil {
		t.Fatalf("live credit balance: %v", err)
	}
	return got
}

// estimated is the estimated remaining credit. It reads both surfaces and
// fails unless the public dashboard's credits block carries the engine read's
// numbers and they add up: estimated = max(balance - unbilled, 0).
func (lf *liveFixture) estimated(t *testing.T) int64 {
	t.Helper()
	got, err := lf.dashboard.GetByToken(lf.ctx, lf.token)
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	c := got.(usage.CostDashboardProjection).Credits
	live := lf.live(t)
	if c.EstimatedRemainingCents != live.EstimatedRemainingCents || c.BalanceCents != live.BalanceCents || c.UnbilledUsageCents != live.UnbilledUsageCents {
		t.Errorf("the two surfaces disagree: dashboard balance/unbilled/estimated = %d/%d/%d, live read = %d/%d/%d",
			c.BalanceCents, c.UnbilledUsageCents, c.EstimatedRemainingCents,
			live.BalanceCents, live.UnbilledUsageCents, live.EstimatedRemainingCents)
	}
	if want := max(live.BalanceCents-live.UnbilledUsageCents, 0); live.EstimatedRemainingCents != want {
		t.Errorf("estimated remaining = %d, want max(%d - %d, 0) = %d", live.EstimatedRemainingCents, live.BalanceCents, live.UnbilledUsageCents, want)
	}
	if live.ComputedAt.IsZero() {
		t.Error("computed_at is not set")
	}
	return c.EstimatedRemainingCents
}

// insertUsage writes one event at an exact instant (ingest stamps and
// validates timestamps; these tests need events at chosen points of a cycle).
func (lf *liveFixture) insertUsage(t *testing.T, meterID string, qty int64, ts time.Time) {
	t.Helper()
	lf.exec(t, `
		INSERT INTO usage_events (id, tenant_id, customer_id, meter_id, quantity, timestamp, livemode)
		VALUES ($1, $2, $3, $4, $5, $6, false)
	`, postgres.NewID("vlx_ue"), lf.tenantID, lf.customerID, meterID, qty, ts.UTC().Truncate(time.Microsecond))
}

func (lf *liveFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	tx, err := lf.db.BeginTx(lf.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`SET LOCAL app.livemode = 'off'`); err != nil {
		t.Fatalf("set local livemode: %v", err)
	}
	if _, err := tx.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// addSub gives the fixture customer a second subscription by raw SQL. The
// subscription service refuses two live subscriptions on one meter
// (rejectMeterOverlap), so the shared-meter case can only be built this way.
func (lf *liveFixture) addSub(t *testing.T, planID string, start, end time.Time) string {
	t.Helper()
	subID := postgres.NewID("vlx_sub")
	itemID := postgres.NewID("vlx_si")
	now := time.Now().UTC()
	lf.exec(t, `
		INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			created_at, updated_at, livemode
		) VALUES ($1, $2, $3, 'Second Sub', $4, 'active', 'anniversary', $5, $6, $6, $7, $7, false)
	`, subID, lf.tenantID, "code-"+subID, lf.customerID, start, end, now)
	lf.exec(t, `
		INSERT INTO subscription_items (id, tenant_id, subscription_id, plan_id, quantity, metadata, created_at, updated_at, livemode)
		VALUES ($1, $2, $3, $4, 1, '{}'::jsonb, $5, $5, false)
	`, itemID, lf.tenantID, subID, planID, now)
	lf.exec(t, `
		INSERT INTO billing_intervals (tenant_id, subscription_id, subscription_item_id, plan_id, quantity, starts_at, source, livemode)
		VALUES ($1, $2, $3, $4, 1, $5, 'create', false)
	`, lf.tenantID, subID, itemID, planID, start)
	return subID
}

// closeEveryPeriod ends every subscription's period just ahead of now, runs
// the real cycle close, and returns how many invoices it produced. The settled
// balance afterwards is what the engine actually drew, the ground truth the
// live figure is an estimate of.
func (lf *liveFixture) closeEveryPeriod(t *testing.T) int {
	t.Helper()
	newEnd := time.Now().UTC().Add(time.Second).Truncate(time.Microsecond)
	lf.exec(t, `UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE tenant_id = $2`, newEnd, lf.tenantID)
	if wait := time.Until(newEnd.Add(500 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	generated, failures := lf.engine.RunCycleForTenant(lf.ctx, lf.tenantID, 50)
	if len(failures) > 0 {
		t.Fatalf("cycle close failed: %v", failures)
	}
	return generated
}

// fire sets a reset=false amount threshold and runs the threshold scan, which
// must fire exactly once. Returns the instant the fire billed through.
func (lf *liveFixture) fire(t *testing.T, amountGTE int64) time.Time {
	t.Helper()
	noReset := false
	if _, err := lf.subSvc.SetBillingThresholds(lf.ctx, lf.tenantID, lf.subID, subscription.BillingThresholdsInput{
		AmountGTE: amountGTE, ResetBillingCycle: &noReset,
	}); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}
	fired, errs := lf.engine.ScanThresholds(lf.ctx, 50)
	if len(errs) > 0 || fired != 1 {
		t.Fatalf("threshold scan: fired=%d errs=%v, want 1 fire", fired, errs)
	}
	invs := lf.listInvoices(t, lf.ctx)
	if len(invs) != 1 {
		t.Fatalf("fire invoices: got %d, want 1", len(invs))
	}
	return invs[0].BillingPeriodEnd.UTC()
}

// (a) After a reset=false threshold invoice the usage it billed has already
// been paid from credit. Subtracting it again made the figure read too low
// (here: zero) for the rest of the cycle.
func TestLiveBalance_ThresholdFireResetFalse_NotDoubleCounted(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance Threshold Fire", 10000)

	lf.ingestUsage(t, lf.ctx, 60, 100) // 6,000 calls at 1c = $60.00
	billedThrough := lf.fire(t, 5000)
	if got := lf.balance(t); got != 4000 {
		t.Fatalf("settled balance after the fire = %d, want 4000 ($100 less the $60 invoice)", got)
	}
	lf.insertUsage(t, lf.meterID, 1000, billedThrough.Add(time.Second)) // $10.00 after the fire

	// $40.00 settled, $10.00 not yet billed.
	if got := lf.estimated(t); got != 3000 {
		t.Errorf("estimated remaining = %d, want 3000; 0 means the $60 the threshold invoice already drew was subtracted twice", got)
	}

	// The close bills only the $10.00, so the estimate was what really remained.
	if n := lf.closeEveryPeriod(t); n != 1 {
		t.Fatalf("cycle close produced %d invoices, want 1", n)
	}
	if got := lf.balance(t); got != 3000 {
		t.Errorf("settled balance after the close = %d, want 3000 (what the estimate said)", got)
	}
}

// A max bucket is exempt from the clamp: the fire left it off its invoice and
// the close bills it over the whole period, so the estimate must count its
// full-window amount (the peak predates the fire).
func TestLiveBalance_NonAdditiveBucketDeferredByFire_CountedFullWindow(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance Deferred Max", 10000)

	rrvMax, err := lf.pricingSvc.CreateRatingRule(lf.ctx, lf.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "gpu_peak", Name: "GPU Peak",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(100),
	})
	if err != nil {
		t.Fatalf("create max rule: %v", err)
	}
	maxMeter, err := lf.pricingSvc.CreateMeter(lf.ctx, lf.tenantID, pricing.CreateMeterInput{
		Key: "gpu", Name: "GPU Concurrency", Unit: "gpus",
		Aggregation: "max", RatingRuleVersionID: rrvMax.ID,
	})
	if err != nil {
		t.Fatalf("create max meter: %v", err)
	}
	mixed, err := lf.pricingSvc.CreatePlan(lf.ctx, lf.tenantID, pricing.CreatePlanInput{
		Code: "pln_mixed", Name: "Mixed Plan", Currency: "USD", BillingInterval: domain.BillingMonthly,
		MeterIDs: []string{lf.meterID, maxMeter.ID},
	})
	if err != nil {
		t.Fatalf("create mixed plan: %v", err)
	}
	lf.exec(t, `UPDATE subscription_items SET plan_id = $1 WHERE subscription_id = $2`, mixed.ID, lf.subID)
	lf.exec(t, `UPDATE billing_intervals SET plan_id = $1 WHERE subscription_id = $2`, mixed.ID, lf.subID)

	lf.insertUsage(t, maxMeter.ID, 5, lf.cycleStart.Add(30*time.Minute)) // peak of 5 at $1.00 = $5.00
	lf.ingestUsage(t, lf.ctx, 60, 100)                                   // $60.00 of calls
	lf.fire(t, 5000)
	if got := lf.balance(t); got != 4000 {
		t.Fatalf("settled balance after the fire = %d, want 4000 (the fire bills the calls and defers the max bucket)", got)
	}

	// $40.00 settled, less the $5.00 peak the close will still bill.
	if got := lf.estimated(t); got != 3500 {
		t.Errorf("estimated remaining = %d, want 3500; 4000 means the max bucket was clamped to the empty post-fire window", got)
	}
	if n := lf.closeEveryPeriod(t); n != 1 {
		t.Fatalf("cycle close produced %d invoices, want 1", n)
	}
	if got := lf.balance(t); got != 3500 {
		t.Errorf("settled balance after the close = %d, want 3500 (what the estimate said)", got)
	}
}

// Usage after a fire is priced continuing the bucket's tiers, as the close
// prices it (ADR-066 amendment), not restarting at the first tier.
func TestLiveBalance_GraduatedTiersContinueAcrossFire(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	var lf *liveFixture
	// 700 units on a ladder of 1,000 at 10c then 2c: the fire bills $70.00.
	p := p21FireAt700(t, "Live Balance Tier Continuity", func(p *p21Fire) {
		lf = wireLive(t, p.f, p.ctx)
		lf.grant(t, 20000)
	})
	if got := lf.balance(t); got != 13000 {
		t.Fatalf("settled balance after the fire = %d, want 13000 ($200 less the $70 invoice)", got)
	}
	p.seedAfterFire(t, 600, nil) // 300 at 10c + 300 at 2c = $36.00

	if got := lf.estimated(t); got != 9400 {
		t.Errorf("estimated remaining = %d, want 9400; 7000 restarts the tiers (600 at 10c), 3000 also re-counts the fire", got)
	}
	if n := lf.closeEveryPeriod(t); n != 1 {
		t.Fatalf("cycle close produced %d invoices, want 1", n)
	}
	if got := lf.balance(t); got != 9400 {
		t.Errorf("settled balance after the close = %d, want 9400 (what the estimate said)", got)
	}
}

// Usage after a fire is priced with the rule version in force when the PERIOD
// opened, as the close prices it, not the version in force when the post-fire
// window opened. A price published mid-period applies from the next period.
func TestLiveBalance_PricesAtThePeriodOpenAfterAFire(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	var lf *liveFixture
	p := p21FireAt700(t, "Live Balance Period Pin", func(p *p21Fire) {
		lf = wireLive(t, p.f, p.ctx)
		lf.grant(t, 20000)
		// Published now: after the period opened, before the fire.
		if _, err := p.f.pricingSvc.CreateRatingRule(p.ctx, p.f.tenantID, pricing.CreateRatingRuleInput{
			RuleKey: "calls_flat", Name: "Calls Repriced", Mode: domain.PricingFlat, Currency: "USD",
			FlatAmountCents: decimal.NewFromInt(50),
		}); err != nil {
			t.Fatalf("publish mid-period version: %v", err)
		}
	})
	if got := lf.balance(t); got != 13000 {
		t.Fatalf("settled balance after the fire = %d, want 13000 (the fire prices at the period-open version)", got)
	}
	p.seedAfterFire(t, 600, nil)

	if got := lf.estimated(t); got != 9400 {
		t.Errorf("estimated remaining = %d, want 9400; 0 prices the post-fire usage at the version published mid-period (600 at 50c)", got)
	}
	if n := lf.closeEveryPeriod(t); n != 1 {
		t.Fatalf("cycle close produced %d invoices, want 1", n)
	}
	if got := lf.balance(t); got != 9400 {
		t.Errorf("settled balance after the close = %d, want 9400 (what the estimate said)", got)
	}
}

// (b) A trial is not billed, but the first paid period is, and the service
// starts that period at the beginning of the day the trial ends. Usage sent on
// that last day, before the flip to active, is inside the period the close
// bills; usage before it is not. The subscription is built by the real Create
// (a trialing row has no billing_intervals until it converts), then moved back
// in time so the trial ends two seconds from now.
func TestLiveBalance_Trial_CountsOnlyTheFirstPaidPeriod(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance Trial", 10000)
	lf.exec(t, `UPDATE subscriptions SET status = 'canceled', canceled_at = now() WHERE id = $1`, lf.subID)

	sub, err := lf.subSvc.Create(lf.ctx, lf.tenantID, subscription.CreateInput{
		Code: "trial-sub", DisplayName: "Trial Sub", CustomerID: lf.customerID,
		Items:     []subscription.CreateItemInput{{PlanID: lf.planID, Quantity: 1}},
		TrialDays: 1,
	})
	if err != nil {
		t.Fatalf("create trialing subscription: %v", err)
	}
	if sub.Status != domain.SubscriptionTrialing || sub.TrialEndAt == nil || sub.CurrentBillingPeriodStart == nil {
		t.Fatalf("created subscription = status %s, trial_end %v, period start %v; want a trialing row with both set", sub.Status, sub.TrialEndAt, sub.CurrentBillingPeriodStart)
	}
	if !sub.CurrentBillingPeriodStart.Before(*sub.TrialEndAt) {
		t.Skipf("the trial ends at the very start of a day (%v); no last-day window to test", sub.TrialEndAt)
	}
	trialEnd := time.Now().UTC().Add(2 * time.Second).Truncate(time.Microsecond)
	shift := sub.TrialEndAt.Sub(trialEnd)
	lf.exec(t, `
		UPDATE subscriptions SET
			trial_start_at = trial_start_at - $1 * interval '1 microsecond',
			trial_end_at = trial_end_at - $1 * interval '1 microsecond',
			current_billing_period_start = current_billing_period_start - $1 * interval '1 microsecond',
			current_billing_period_end = current_billing_period_end - $1 * interval '1 microsecond',
			next_billing_at = next_billing_at - $1 * interval '1 microsecond'
		WHERE id = $2
	`, shift.Microseconds(), sub.ID)
	periodStart := sub.CurrentBillingPeriodStart.Add(-shift)
	lastDay := periodStart.Add(time.Until(periodStart) / -2) // midway between the period start and now
	if !lastDay.After(periodStart) || !lastDay.Before(time.Now()) {
		t.Skipf("no room between the period start %v and now", periodStart)
	}

	lf.insertUsage(t, lf.meterID, 3000, periodStart.Add(-2*time.Hour)) // $30.00 in the trial, before the first paid period
	lf.insertUsage(t, lf.meterID, 2000, lastDay)                       // $20.00 on the trial's last day, inside the first paid period

	if got := lf.estimated(t); got != 8000 {
		t.Errorf("estimated remaining while trialing = %d, want 8000; 10000 leaves out last-day usage the close will bill, 5000 counts trial usage nobody bills", got)
	}

	time.Sleep(time.Until(trialEnd.Add(500 * time.Millisecond)))
	if n, errs := lf.subSvc.ProcessExpiredTrials(lf.ctx, 50); n != 1 || len(errs) > 0 {
		t.Fatalf("trial expiry: processed=%d errs=%v, want 1", n, errs)
	}
	if got := lf.estimated(t); got != 8000 {
		t.Errorf("estimated remaining after the trial converted = %d, want 8000 (no jump at the flip)", got)
	}
	if n := lf.closeEveryPeriod(t); n != 1 {
		t.Fatalf("cycle close produced %d invoices, want 1", n)
	}
	if got := lf.balance(t); got != 8000 {
		t.Errorf("settled balance after the close = %d, want 8000 (what the estimate said)", got)
	}
}

// A trial that runs past the end of the stored period: the close advances the
// period without an invoice (handleTrialState), so that period's usage never
// draws credit and is not counted.
func TestLiveBalance_TrialOutlastsThePeriod_UsageNotSubtracted(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance Long Trial", 10000)

	lf.exec(t, `UPDATE subscriptions SET status = 'trialing', trial_start_at = $1, trial_end_at = $2 WHERE id = $3`,
		lf.cycleStart, lf.cycleEnd.Add(14*24*time.Hour), lf.subID)
	lf.ingestUsage(t, lf.ctx, 25, 100) // $25.00 of usage during the trial

	if got := lf.estimated(t); got != 10000 {
		t.Errorf("estimated remaining = %d, want 10000; 7500 subtracts trial usage nobody will bill", got)
	}

	if n := lf.closeEveryPeriod(t); n != 0 {
		t.Fatalf("cycle close produced %d invoices for a trialing subscription, want 0", n)
	}
	if invs := lf.listInvoices(t, lf.ctx); len(invs) != 0 {
		t.Fatalf("trial period close wrote %d invoices, want 0", len(invs))
	}
	if got := lf.balance(t); got != 10000 {
		t.Errorf("settled balance after the trial period closed = %d, want 10000", got)
	}
}

// A trialing subscription that is set to cancel at the end of its trial is
// canceled without an invoice, so its usage is not counted.
func TestLiveBalance_TrialSetToCancel_UsageNotSubtracted(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance Trial Cancel", 10000)

	lf.exec(t, `UPDATE subscriptions SET status = 'trialing', trial_start_at = $1, trial_end_at = $2, cancel_at_period_end = true WHERE id = $3`,
		lf.cycleStart.Add(-24*time.Hour), time.Now().UTC().Add(time.Hour), lf.subID)
	lf.ingestUsage(t, lf.ctx, 25, 100)

	if got := lf.estimated(t); got != 10000 {
		t.Errorf("estimated remaining = %d, want 10000; 7500 counts usage of a trial that cancels free", got)
	}
}

// A plan change in the middle of a period. The close bills each meter for the
// time its plan was on the subscription (billing_intervals), so usage on the
// outgoing plan's meter before the swap is still owed after the swap.
func TestLiveBalance_PlanSwapMidPeriod_OutgoingMeterStillCounted(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance Plan Swap", 10000)

	rrv, err := lf.pricingSvc.CreateRatingRule(lf.ctx, lf.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "tok_flat", Name: "Tok Flat",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	tok, err := lf.pricingSvc.CreateMeter(lf.ctx, lf.tenantID, pricing.CreateMeterInput{
		Key: "tok", Name: "Tok", Unit: "tok", Aggregation: "sum", RatingRuleVersionID: rrv.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	planB, err := lf.pricingSvc.CreatePlan(lf.ctx, lf.tenantID, pricing.CreatePlanInput{
		Code: "pln_tok", Name: "Tok Plan", Currency: "USD", BillingInterval: domain.BillingMonthly,
		MeterIDs: []string{tok.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	lf.ingestUsage(t, lf.ctx, 20, 100) // $20.00 of calls on the first plan
	if got := lf.estimated(t); got != 8000 {
		t.Fatalf("estimated remaining before the swap = %d, want 8000", got)
	}
	res, err := lf.subSvc.UpdateItem(lf.ctx, lf.tenantID, lf.subID, lf.itemID, subscription.UpdateItemInput{NewPlanID: planB.ID, Immediate: true})
	if err != nil {
		t.Fatalf("swap plan: %v", err)
	}
	lf.insertUsage(t, tok.ID, 500, res.EffectiveAt.Add(time.Millisecond))     // $5.00 of tok after the swap
	lf.insertUsage(t, tok.ID, 900, lf.cycleStart.Add(time.Hour))              // $9.00 of tok BEFORE the plan carried the meter: not billed
	lf.insertUsage(t, lf.meterID, 700, res.EffectiveAt.Add(time.Millisecond)) // $7.00 of calls AFTER the plan dropped the meter: not billed

	if got := lf.estimated(t); got != 7500 {
		t.Errorf("estimated remaining after the swap = %d, want 7500 ($100 less $20 calls before and $5 tok after); 8600 prices only the new plan's meter over the whole period", got)
	}
	if n := lf.closeEveryPeriod(t); n != 1 {
		t.Fatalf("cycle close produced %d invoices, want 1", n)
	}
	if got := lf.balance(t); got != 7500 {
		t.Errorf("settled balance after the close = %d, want 7500 (what the estimate said)", got)
	}
}

// A grant past its expiry that the hourly sweep has not written off yet is
// still in the ledger sum, but the close will not draw on it. The live read
// leaves it out, so it reports what a draw at this instant could spend.
func TestLiveBalance_ExpiredGrantNotYetSwept_NotSpendable(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance Expired Grant", 5000) // $50.00 that never expires

	expires := time.Now().UTC().Add(2 * time.Second)
	if _, err := lf.creditSvc.Grant(lf.ctx, lf.tenantID, credit.GrantInput{
		CustomerID: lf.customerID, AmountCents: 10000, Description: "promo", ExpiresAt: &expires,
	}); err != nil {
		t.Fatalf("grant expiring credit: %v", err)
	}
	lf.ingestUsage(t, lf.ctx, 20, 100) // $20.00 of usage
	if got := lf.estimated(t); got != 13000 {
		t.Fatalf("estimated remaining before the expiry = %d, want 13000", got)
	}
	time.Sleep(time.Until(expires.Add(500 * time.Millisecond)))

	if got := lf.balance(t); got != 15000 {
		t.Fatalf("ledger sum = %d, want 15000 (the sweep has not run)", got)
	}
	if got := lf.estimated(t); got != 3000 {
		t.Errorf("estimated remaining = %d, want 3000; 13000 counts a grant the close will not draw on", got)
	}
	if got := lf.live(t).BalanceCents; got != 5000 {
		t.Errorf("live balance_cents = %d, want 5000 (the credit that can still be spent)", got)
	}

	// The close draws the $20.00 from the unexpired grant only.
	if n := lf.closeEveryPeriod(t); n != 1 {
		t.Fatalf("cycle close produced %d invoices, want 1", n)
	}
	if got := lf.live(t).BalanceCents; got != 3000 {
		t.Errorf("live balance_cents after the close = %d, want 3000 (what the estimate said)", got)
	}
	// The sweep then writes the expiry; the figure does not move.
	if _, errs := lf.creditSvc.ExpireCredits(lf.ctx); len(errs) > 0 {
		t.Fatalf("expire credits: %v", errs)
	}
	if got := lf.balance(t); got != 3000 {
		t.Errorf("ledger sum after the sweep = %d, want 3000", got)
	}
	if got := lf.estimated(t); got != 3000 {
		t.Errorf("estimated remaining after the sweep = %d, want 3000", got)
	}
}

// (c) Each subscription's usage is counted over that subscription's own
// current period. One window (the latest-started subscription's) for every
// meter missed the other subscription's earlier usage.
func TestLiveBalance_TwoSubscriptionsDifferentCycles_EachOwnWindow(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance Two Cycles", 10000)

	rrv, err := lf.pricingSvc.CreateRatingRule(lf.ctx, lf.tenantID, pricing.CreateRatingRuleInput{
		RuleKey: "tokens_flat", Name: "Tokens Flat",
		Mode: domain.PricingFlat, Currency: "USD", FlatAmountCents: decimal.NewFromInt(1),
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	tokens, err := lf.pricingSvc.CreateMeter(lf.ctx, lf.tenantID, pricing.CreateMeterInput{
		Key: "tokens", Name: "Tokens", Unit: "tokens", Aggregation: "sum", RatingRuleVersionID: rrv.ID,
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	planB, err := lf.pricingSvc.CreatePlan(lf.ctx, lf.tenantID, pricing.CreatePlanInput{
		Code: "pln_tokens", Name: "Tokens Plan", Currency: "USD", BillingInterval: domain.BillingMonthly,
		MeterIDs: []string{tokens.ID},
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	// The fixture subscription's period opened 3 days ago; this one's opened
	// 10 days ago.
	startB := lf.cycleStart.Add(-7 * 24 * time.Hour)
	lf.addSub(t, planB.ID, startB, startB.Add(30*24*time.Hour))

	lf.ingestUsage(t, lf.ctx, 10, 100)                                      // $10.00 of calls, in the fixture sub's period
	lf.insertUsage(t, tokens.ID, 2000, lf.cycleStart.Add(-2*24*time.Hour))  // $20.00 of tokens, 5 days ago
	lf.insertUsage(t, lf.meterID, 400, lf.cycleStart.Add(-2*24*time.Hour))  // $4.00 of calls BEFORE the fixture sub's period: its last close billed that
	lf.insertUsage(t, tokens.ID, 700, startB.Add(-24*time.Hour))            // $7.00 of tokens before either period
	lf.insertUsage(t, lf.meterID, 300, lf.cycleStart.Add(-10*24*time.Hour)) // $3.00 of calls before either period

	if got := lf.estimated(t); got != 7000 {
		t.Errorf("estimated remaining = %d, want 7000 ($100 less $10 calls and $20 tokens); 9000 applies the later subscription's window to the tokens meter", got)
	}
	if n := lf.closeEveryPeriod(t); n != 2 {
		t.Fatalf("cycle close produced %d invoices, want 2", n)
	}
	if got := lf.balance(t); got != 7000 {
		t.Errorf("settled balance after both closes = %d, want 7000 (what the estimate said)", got)
	}
}

// Two subscriptions sharing one meter. The subscription service refuses this
// (rejectMeterOverlap) because usage is aggregated per customer and meter with
// no subscription key, so each subscription's close bills the same events.
// If such rows exist anyway, the estimate matches what the closes will draw:
// the shared usage once per subscription.
func TestLiveBalance_TwoSubscriptionsSharedMeter_MatchesClose(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance Shared Meter", 10000)

	startB := lf.cycleStart.Add(-7 * 24 * time.Hour)
	lf.addSub(t, lf.planID, startB, startB.Add(30*24*time.Hour))
	lf.ingestUsage(t, lf.ctx, 15, 100) // $15.00 of calls, inside both periods

	if got := lf.estimated(t); got != 7000 {
		t.Errorf("estimated remaining = %d, want 7000: each subscription's close bills the $15; 8500 counts it once and disagrees with the engine", got)
	}
	if n := lf.closeEveryPeriod(t); n != 2 {
		t.Fatalf("cycle close produced %d invoices, want 2", n)
	}
	if got := lf.balance(t); got != 7000 {
		t.Errorf("settled balance after both closes = %d, want 7000 (what the estimate said)", got)
	}
}

// The estimate never goes below zero; the parts show how far past it the
// customer is.
func TestLiveBalance_FlooredAtZero_PartsShowTheOverage(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance Floor", 1000)
	lf.ingestUsage(t, lf.ctx, 25, 100) // $25.00 of usage against $10.00 of credit

	if got := lf.estimated(t); got != 0 {
		t.Errorf("estimated remaining = %d, want 0", got)
	}
	if live := lf.live(t); live.BalanceCents != 1000 || live.UnbilledUsageCents != 2500 {
		t.Errorf("parts = balance %d, unbilled %d; want 1000 and 2500", live.BalanceCents, live.UnbilledUsageCents)
	}
}

// A top-up shows on the very next read: nothing is cached or stored.
func TestLiveBalance_TopUpShowsOnNextRead(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance Top Up", 1000)
	lf.ingestUsage(t, lf.ctx, 25, 100)
	if got := lf.estimated(t); got != 0 {
		t.Fatalf("estimated remaining before the top-up = %d, want 0", got)
	}
	lf.grant(t, 5000)
	if got := lf.estimated(t); got != 3500 {
		t.Errorf("estimated remaining after a $50 top-up = %d, want 3500", got)
	}
}

// A customer with no active subscription has no unbilled usage: usage sent
// outside a subscription is recorded but nothing will bill it.
func TestLiveBalance_NoActiveSubscription_EqualsSettled(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance No Sub", 10000)
	lf.ingestUsage(t, lf.ctx, 25, 100)
	lf.exec(t, `UPDATE subscriptions SET status = 'canceled', canceled_at = now() WHERE id = $1`, lf.subID)

	if got := lf.estimated(t); got != 10000 {
		t.Errorf("estimated remaining = %d, want 10000 (the settled balance)", got)
	}
}

// The public dashboard shows this period's usage and the live figure, and
// both need the same aggregation of the same meter over the same period. One
// request must scan the usage once, not once per reader.
func TestLiveBalance_DashboardAggregatesOncePerMeter(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance One Scan", 10000)
	lf.ingestUsage(t, lf.ctx, 25, 100)

	ctx, memo := usage.WithAggregationMemo(lf.ctx)
	got, err := lf.dashboard.GetByToken(ctx, lf.token)
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	proj := got.(usage.CostDashboardProjection)
	if proj.ProjectedTotalCents != 2500 || proj.Credits.EstimatedRemainingCents != 7500 {
		t.Fatalf("projected = %d, estimated = %d; want 2500 and 7500", proj.ProjectedTotalCents, proj.Credits.EstimatedRemainingCents)
	}
	if memo.Queries() != 1 || memo.Hits() != 1 {
		t.Errorf("usage aggregations: %d reached the database and %d were reused; want 1 and 1 (one meter, two readers)", memo.Queries(), memo.Hits())
	}

	// After a threshold invoice the live figure reads only the window since
	// the invoice, a different (and much shorter) scan than the page's.
	lf.fire(t, 2000)
	ctx, memo = usage.WithAggregationMemo(lf.ctx)
	if _, err := lf.dashboard.GetByToken(ctx, lf.token); err != nil {
		t.Fatalf("dashboard after fire: %v", err)
	}
	if memo.Queries() != 2 || memo.Hits() != 0 {
		t.Errorf("after a threshold invoice: %d aggregations reached the database and %d were reused; want 2 and 0", memo.Queries(), memo.Hits())
	}
}

// The live read pages through a customer's subscriptions: more than one page
// of them must not hide an active one from the figure.
func TestLiveBalance_CountsEverySubscriptionPastOnePage(t *testing.T) {
	lf := newLiveFixture(t, "Live Balance Paging", 10000)
	lf.ingestUsage(t, lf.ctx, 25, 100) // $25.00 on the fixture subscription

	empty, err := lf.pricingSvc.CreatePlan(lf.ctx, lf.tenantID, pricing.CreatePlanInput{
		Code: "pln_no_meters", Name: "No Meters", Currency: "USD", BillingInterval: domain.BillingMonthly,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	// 100 newer active subscriptions with nothing to bill fill the first page
	// (the list is newest first), leaving the fixture's on the second.
	for i := 0; i < 100; i++ {
		lf.addSub(t, empty.ID, lf.cycleStart, lf.cycleEnd)
	}
	if got := lf.live(t).UnbilledUsageCents; got != 2500 {
		t.Errorf("unbilled usage = %d, want 2500; 0 means the read stopped at the first page of subscriptions", got)
	}
}
