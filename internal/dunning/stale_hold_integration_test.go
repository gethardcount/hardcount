package dunning_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
	"github.com/sagarsuperuser/velox/internal/platform/leader/leadertest"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Dunning and the charge hold (#976, ADR-127). A set hold — a charge running,
// or one whose result was lost and waits for the stale-hold sweep — blocks the
// dunning retry's claim and dunning's final action. These tests pin that the
// due-run scans skip such a run instead of letting it head the queue, that
// the final action never fires while a hold is set, and that the final action
// takes the hold itself so no charge can start while it cancels.

func setHold(t *testing.T, db *postgres.DB, ctx context.Context, invoiceID string, offset time.Duration) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET auto_charge_claimed_until = now() + make_interval(secs => $2) WHERE id = $1`,
		invoiceID, offset.Seconds()); err != nil {
		t.Fatalf("set hold: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func clearHold(t *testing.T, db *postgres.DB, ctx context.Context, invoiceID string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET auto_charge_claimed_until = NULL WHERE id = $1`, invoiceID); err != nil {
		t.Fatalf("clear hold: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// 10b. 25 runs whose invoices hold a stale charge hold, due earlier than one
// ordinary due run, at limit 20: the ordinary run is picked and none of the
// held ones. A written-off invoice with a hold is still picked (the terminal
// branch resolves its run).
func TestListDueRuns_ChargeHoldIsSkipped(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleDunning)
	tenantID := testutil.CreateTestTenant(t, db, "DueRuns Charge Hold")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_hold_due", DisplayName: "Hold Due"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	dstore := dunning.NewPostgresStore(db)
	policy, err := dstore.UpsertPolicy(ctx, tenantID, domain.DunningPolicy{
		Name: "default", Enabled: true, RetryDays: []int{3, 3, 3},
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionMarkUncollectible,
	})
	if err != nil {
		t.Fatalf("upsert policy: %v", err)
	}
	istore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	mkRun := func(num string, dueAgo time.Duration) (invoiceID, runID string) {
		inv, err := istore.Create(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num, Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
			Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		})
		if err != nil {
			t.Fatalf("create invoice %s: %v", num, err)
		}
		run, err := dstore.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
			InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID, State: domain.DunningActive, Reason: "payment_failed",
		})
		if err != nil {
			t.Fatalf("create run %s: %v", num, err)
		}
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invoice_dunning_runs SET next_action_at = now() - make_interval(secs => $2) WHERE id = $1`,
			run.ID, dueAgo.Seconds()); err != nil {
			t.Fatalf("set next_action_at: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return inv.ID, run.ID
	}

	held := map[string]bool{}
	for i := 0; i < 25; i++ {
		invID, runID := mkRun(fmt.Sprintf("INV-HOLD-%02d", i), 3*time.Hour)
		setHold(t, db, ctx, invID, -11*time.Minute) // stale: a lost charge
		held[runID] = true
	}
	live, liveRun := mkRun("INV-HOLD-LIVE", 3*time.Hour)
	setHold(t, db, ctx, live, 4*time.Minute) // a charge running right now
	held[liveRun] = true
	wroff, wroffRun := mkRun("INV-HOLD-WROFF", 3*time.Hour)
	setHold(t, db, ctx, wroff, -11*time.Minute)
	if _, err := istore.UpdateStatus(ctx, tenantID, wroff, domain.InvoiceUncollectible); err != nil {
		t.Fatalf("write off: %v", err)
	}
	_, ordinary := mkRun("INV-HOLD-ORDINARY", time.Hour)

	due, err := dstore.ListDueRuns(ctx, tenantID, now.Add(time.Minute), 20)
	if err != nil {
		t.Fatalf("ListDueRuns: %v", err)
	}
	got := map[string]bool{}
	for _, r := range due {
		got[r.ID] = true
		if held[r.ID] {
			t.Errorf("run %s picked while its invoice's charge hold is set: its claim cannot succeed, and an oldest-first LIMITed scan would keep it at the head", r.ID)
		}
	}
	if !got[ordinary] {
		t.Error("the ordinary due run was not picked: held runs filled the page")
	}
	if !got[wroffRun] {
		t.Error("a written-off invoice's run must still be picked even with a hold: the terminal branch resolves it")
	}
}

// The sim-time twin of the scan above: same rule, written a second way.
func TestListDueRunsForClock_ChargeHoldIsSkipped(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := "tnt_hold_clock"
	const clockID = "vlx_tclk_hold"
	frozen := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)

	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustExec(`SELECT set_config('app.livemode', 'off', true)`)
	mustExec(`INSERT INTO tenants (id, name, status) VALUES ($1, 'Hold Clock', 'active')`, tenantID)
	mustExec(`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		VALUES ($1, $2, 'hold', $3, 'ready', false)`, clockID, tenantID, frozen)
	mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, test_clock_id, created_at, updated_at)
		VALUES ('cus_hold_clk', $1, 'hold_clk', 'Hold', '', $2, $3, $3)`, tenantID, clockID, frozen)
	mustExec(`INSERT INTO dunning_policies (id, tenant_id, name, enabled, is_default, retry_days, final_subscription_action, final_invoice_action, livemode)
		VALUES ('dpol_hold', $1, 'P', true, true, '[3, 3, 3]', 'pause', 'none', false)`, tenantID)
	seedInvRun := func(invID, runID, invStatus string, hold bool) {
		mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number,
			status, payment_status, is_simulated, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, tax_status,
			billing_period_start, billing_period_end, created_at, updated_at, auto_charge_claimed_until)
		VALUES ($1, $2, 'cus_hold_clk', $1, $3, 'failed', true, 'USD', 2500, 2500, 2500, 'ok', $4, $4, $4, $4,
			CASE WHEN $5 THEN now() - interval '11 minutes' END)`,
			invID, tenantID, invStatus, frozen.Add(-24*time.Hour), hold)
		mustExec(`INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason, attempt_count, next_action_at, paused, created_at, updated_at, livemode)
		VALUES ($1, $2, $3, 'cus_hold_clk', 'dpol_hold', 'active', 'payment_failed', 0, $4, false, $5, $5, false)`,
			runID, tenantID, invID, frozen.Add(-time.Hour), frozen.Add(-24*time.Hour))
	}
	seedInvRun("inv_clk_held", "run_clk_held", "finalized", true)
	seedInvRun("inv_clk_held_wroff", "run_clk_held_wroff", "uncollectible", true)
	seedInvRun("inv_clk_free", "run_clk_free", "finalized", false)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	due, err := dunning.NewPostgresStore(db).ListDueRunsForClock(ctx, tenantID, clockID, frozen, 50)
	if err != nil {
		t.Fatalf("ListDueRunsForClock: %v", err)
	}
	got := map[string]bool{}
	for _, r := range due {
		got[r.ID] = true
	}
	if got["run_clk_held"] {
		t.Error("a simulated invoice with a charge hold must be excluded from the catchup scan")
	}
	if !got["run_clk_held_wroff"] {
		t.Error("a written-off simulated invoice with a hold must still be selected — the terminal branch resolves the run")
	}
	if !got["run_clk_free"] {
		t.Error("an ordinary failed simulated invoice must remain due — the control")
	}
}

// 10a. The final retry's result was lost (its process crashed after Stripe
// took the money), and the exhaustion lease then ended, so the run is due for
// exhaust-on-entry. While the charge's stale hold is set the run is not even
// picked; nothing fires. Once the stale-hold sweep has recorded the payment
// (hold cleared, invoice paid), the next tick resolves the run as recovered,
// the subscription untouched.
func TestExhaust_LostFinalRetry_RunWaitsForTheSweep(t *testing.T) {
	db := testutil.SetupTestDB(t)
	acts := finalActions{name: "cancel and write off", sub: domain.SubActionCancel, inv: domain.InvActionMarkUncollectible}
	f := newExhaustFixture(t, db, "Lost Final Retry", acts, 2, nil, nil)
	wireChargeHolds(f)
	setHold(t, db, f.ctx, f.invoiceID, -11*time.Minute)

	f.tick(t)
	f.assertNothingFired(t)

	// What the sweep does when it finds the payment: settle, clear the hold.
	if _, err := f.invoices.MarkPaid(f.ctx, f.tenantID, f.invoiceID, "pi_found_by_sweep", time.Now().UTC()); err != nil {
		t.Fatalf("settle: %v", err)
	}
	clearHold(t, db, f.ctx, f.invoiceID)
	f.tick(t)
	run, err := f.runs.GetRun(f.ctx, f.tenantID, f.runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.State != domain.DunningResolved || run.Resolution != domain.ResolutionPaymentRecovered {
		t.Errorf("run state=%s resolution=%q, want resolved / payment_recovered", run.State, run.Resolution)
	}
	sub, err := f.subs.Get(f.ctx, f.tenantID, f.subID)
	if err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if sub.Status != domain.SubscriptionActive {
		t.Errorf("subscription is %s, want active: the customer paid", sub.Status)
	}
}

// The final action reaches a hold the due-run scan could not have seen: the
// final retry is declined, and while it ran another charge took the hold (a
// Collect, an auto-charge whose card was just attached). Neither action fires;
// the run keeps its lease and is looked at again.
func TestExhaust_HoldSetDuringTheFinalRetry_HoldsTheFinalAction(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, acts := range policyActions {
		for _, hold := range []struct {
			name   string
			offset time.Duration
		}{
			{"a charge running", 4 * time.Minute},
			{"a lost charge's stale hold", -11 * time.Minute},
		} {
			t.Run(acts.name+"/"+hold.name, func(t *testing.T) {
				var f *exhaustFixture
				retry := &finalRetry{t: t, f: &f, raceIn: func(t *testing.T, ctx context.Context, f *exhaustFixture) {
					setHold(t, f.db, ctx, f.invoiceID, hold.offset)
				}}
				f = newExhaustFixture(t, db, "Hold During Final "+acts.name+" "+hold.name, acts, 1, nil, retry)
				wireChargeHolds(f)

				f.tick(t)
				if retry.calls != 1 {
					t.Fatalf("final retry charged %d times, want 1", retry.calls)
				}
				f.assertHeld(t)
			})
		}
	}
}

// 10c. The auto-charge sweep charging a no-card invoice whose card was just
// attached, and dunning's final action canceling its subscription, race. They
// must never both happen: the final action takes the charge hold, and the
// charge claim refuses any hold. Deterministic in both orders, then raced
// with goroutines.
func TestExhaust_RacesTheAutoCharge_NeverChargedAndCanceled(t *testing.T) {
	db := testutil.SetupTestDB(t)
	acts := finalActions{name: "cancel", sub: domain.SubActionCancel, inv: domain.InvActionNone}

	// The charge as the auto-charge sweep makes it once the card exists:
	// claim, then (Stripe succeeding) the paid write. Returns whether it
	// charged.
	charge := func(t *testing.T, f *exhaustFixture) bool {
		t.Helper()
		claimed, err := claimAutoCharge(f.ctx, f.invoices, f.tenantID, f.invoiceID)
		if err != nil {
			t.Errorf("auto-charge claim: %v", err)
			return false
		}
		if !claimed {
			return false
		}
		if _, err := f.invoices.MarkPaid(f.ctx, f.tenantID, f.invoiceID, "pi_autocharge", time.Now().UTC()); err != nil {
			t.Errorf("auto-charge settle: %v", err)
			return false
		}
		return true
	}
	// noCardInvoice turns the fixture's invoice into the no-card dunning
	// shape the auto-charge sweep still visits every tick.
	noCardInvoice := func(t *testing.T, f *exhaustFixture) {
		t.Helper()
		tx, err := f.db.BeginTx(f.ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(f.ctx, `UPDATE invoices SET payment_status = 'pending', auto_charge_pending = TRUE WHERE id = $1`, f.invoiceID); err != nil {
			t.Fatalf("no-card shape: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	outcome := func(t *testing.T, f *exhaustFixture, charged bool) {
		t.Helper()
		sub, err := f.subs.Get(f.ctx, f.tenantID, f.subID)
		if err != nil {
			t.Fatalf("read subscription: %v", err)
		}
		if charged && sub.Status == domain.SubscriptionCanceled {
			t.Errorf("the invoice was charged AND its subscription canceled by dunning: a paying customer canceled")
		}
	}

	t.Run("the charge starts while the cancel runs", func(t *testing.T) {
		f := newExhaustFixture(t, db, "Race Cancel First", acts, 2, nil, nil)
		wireChargeHolds(f)
		noCardInvoice(t, f)
		var charged bool
		f.svc.SetSubscriptionCanceler(&duringCancel{inner: dunningMovers{svc: f.subs}, during: func() { charged = charge(t, f) }})
		f.tick(t)
		outcome(t, f, charged)
	})

	t.Run("the cancel starts while the charge holds the invoice", func(t *testing.T) {
		f := newExhaustFixture(t, db, "Race Charge First", acts, 2, nil, nil)
		wireChargeHolds(f)
		noCardInvoice(t, f)
		claimed, err := claimAutoCharge(f.ctx, f.invoices, f.tenantID, f.invoiceID)
		if err != nil || !claimed {
			t.Fatalf("auto-charge claim: %v %v", claimed, err)
		}
		f.tick(t) // the final action meets the charge's hold
		if _, err := f.invoices.MarkPaid(f.ctx, f.tenantID, f.invoiceID, "pi_autocharge", time.Now().UTC()); err != nil {
			t.Fatalf("settle: %v", err)
		}
		outcome(t, f, true)
	})

	t.Run("raced", func(t *testing.T) {
		for i := 0; i < 8; i++ {
			f := newExhaustFixture(t, db, fmt.Sprintf("Race Goroutines %d", i), acts, 2, nil, nil)
			wireChargeHolds(f)
			noCardInvoice(t, f)
			var wg sync.WaitGroup
			start := make(chan struct{})
			var charged bool
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				charged = charge(t, f)
			}()
			go func() {
				defer wg.Done()
				<-start
				_, _ = f.svc.ProcessDueRuns(f.ctx, f.tenantID, 50)
			}()
			close(start)
			wg.Wait()
			outcome(t, f, charged)
		}
	})
}

// duringCancel runs during() inside the dunning cancel, before the
// subscription is canceled: the window in which a charge must not start.
type duringCancel struct {
	inner  dunning.SubscriptionCanceler
	during func()
}

func (d *duringCancel) Cancel(ctx context.Context, tenantID, id string) error {
	d.during()
	return d.inner.Cancel(ctx, tenantID, id)
}
