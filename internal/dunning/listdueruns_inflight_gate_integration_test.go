package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/walk"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestListDueRuns_InFlightGate pins both halves of the in-flight exclusion,
// because each half fixes a bug the other one causes.
//
// EXCLUDE while the payment is in flight. ClaimChargeForDunningRetry requires
// payment_status IN ('pending','failed'), so at 'processing' or 'unknown' the
// claim CANNOT succeed: the adapter returns ErrTransientSkip and the transient
// handler deliberately does not reschedule (a Stripe blip must not burn a
// tenant's retry budget). Selecting the run is provably wasted work — it writes
// an attempt and rewinds it, every tick — and because next_action_at is frozen
// while that happens and this scan is next_action_at ASC with a LIMIT, these
// runs migrate to the head of the queue and stay there. At LIMIT of them, real
// dunning stops for everyone.
//
// KEEP once the invoice is terminal. The run is resolved by the processing
// path's own terminal branch, and markUncollectible's ResolveByInvoice is
// best-effort, documented as relying on dunning scanning invoice status "on next
// tick anyway". An exclusion that also hid written-off invoices would delete
// that backstop and strand the run active forever — which is precisely the bug
// the clawback scan had. Fixing one starvation by creating another is the
// failure mode this test exists to prevent.
//
// WHY unknown-WITH-AN-ID IS HERE. This test first asserted the OPPOSITE for that
// row — that it "can still be named and resolved, so the run must stay due" — on
// the belief that holding a PaymentIntent id means the reconciler will clear it.
// It does not: reconciler.go settles only TERMINAL provider outcomes and skips
// processing/requires_action/requires_confirmation/requires_capture every sweep,
// so a PI sitting at requires_action (off-session SCA nobody completes) keeps the
// invoice in flight indefinitely and spun exactly like a parked one. "Do we hold
// an id" was the wrong question; "can the claim succeed" is the right one, and
// the claim's own predicate answers it for every in-flight shape at once.
func TestListDueRuns_InFlightGate(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleDunning)
	tenantID := testutil.CreateTestTenant(t, db, "DueRuns InFlight Gate")

	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_inflight_due", DisplayName: "InFlight Due",
	})
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

	// mkDueRun builds a finalized invoice in the given payment shape plus an
	// active dunning run forced due, then applies the invoice status (a write-off
	// is a status change only — payment_status deliberately stays put, since
	// nobody ever learned whether the card was charged).
	mkDueRun := func(num string, ps domain.InvoicePaymentStatus, piID string, status domain.InvoiceStatus) string {
		inv, err := istore.CreateWithLineItems(ctx, tenantID, domain.Invoice{
			CustomerID: cust.ID, InvoiceNumber: num, Status: domain.InvoiceFinalized,
			PaymentStatus: ps, Currency: "USD",
			SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
		}, nil)
		if err != nil {
			t.Fatalf("create invoice %s: %v", num, err)
		}
		run, err := dstore.CreateRun(ctx, tenantID, domain.InvoiceDunningRun{
			InvoiceID: inv.ID, CustomerID: cust.ID, PolicyID: policy.ID,
			State: domain.DunningActive, Reason: "payment_failed",
		})
		if err != nil {
			t.Fatalf("create run %s: %v", num, err)
		}
		// CreateWithLineItems does NOT persist stripe_payment_intent_id — that
		// column is only ever written by the payment path. Setting it on the
		// struct silently did nothing, which once made the "unknown WITH a PI"
		// row indistinguishable from a parked one and its assertion vacuous.
		// Write it the real way.
		if piID != "" {
			if _, applied, err := istore.StampChargeOutcome(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq, ps, piID, ""); err != nil || !applied {
				t.Fatalf("attach payment intent to %s: applied=%v err=%v", num, applied, err)
			}
			after, gerr := istore.Get(ctx, tenantID, inv.ID)
			if gerr != nil {
				t.Fatalf("get invoice %s: %v", num, gerr)
			}
			if after.StripePaymentIntentID != piID {
				t.Fatalf("%s: PaymentIntent id did not persist (got %q) — the assertion below would pass for the wrong reason", num, after.StripePaymentIntentID)
			}
		}
		if status != domain.InvoiceFinalized {
			if _, err := istore.UpdateStatusWithReversalPrior(ctx, tenantID, inv.ID, status, nil); err != nil {
				t.Fatalf("set invoice %s status %s: %v", num, status, err)
			}
			after, gerr := istore.Get(ctx, tenantID, inv.ID)
			if gerr != nil {
				t.Fatalf("get invoice %s: %v", num, gerr)
			}
			if after.PaymentStatus != ps {
				t.Fatalf("%s: status change moved payment_status %q → %q; a write-off closes the invoice, it does not answer whether the card was charged", num, ps, after.PaymentStatus)
			}
		}
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE invoice_dunning_runs SET next_action_at = now() - interval '1 hour' WHERE id = $1`, run.ID); err != nil {
			t.Fatalf("set next_action_at: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return run.ID
	}

	parkedOpen := mkDueRun("INV-PARKED-OPEN", domain.PaymentUnknown, "", domain.InvoiceFinalized)
	unknownWithPI := mkDueRun("INV-UNKNOWN-PI", domain.PaymentUnknown, "pi_live_1", domain.InvoiceFinalized)
	processing := mkDueRun("INV-PROCESSING", domain.PaymentProcessing, "pi_live_2", domain.InvoiceFinalized)
	parkedWrittenOff := mkDueRun("INV-PARKED-WROFF", domain.PaymentUnknown, "", domain.InvoiceUncollectible)
	ordinaryFailed := mkDueRun("INV-FAILED", domain.PaymentFailed, "", domain.InvoiceFinalized)
	hostedLive := mkDueRun("INV-HOSTED-LIVE", domain.PaymentFailed, "", domain.InvoiceFinalized)
	hostedLiveWrittenOff := mkDueRun("INV-HOSTED-WROFF", domain.PaymentFailed, "", domain.InvoiceUncollectible)
	seedLiveHostedPage(t, db, ctx, tenantID, hostedLive)
	seedLiveHostedPage(t, db, ctx, tenantID, hostedLiveWrittenOff)
	held := mkDueRun("INV-HELD", domain.PaymentFailed, "", domain.InvoiceFinalized)
	heldWrittenOff := mkDueRun("INV-HELD-WROFF", domain.PaymentFailed, "", domain.InvoiceUncollectible)
	seedChargeHold(t, db, ctx, tenantID, held)
	seedChargeHold(t, db, ctx, tenantID, heldWrittenOff)

	due, err := dstore.ListDueRuns(ctx, tenantID, now.Add(time.Minute), 200)
	if err != nil {
		t.Fatalf("ListDueRuns: %v", err)
	}
	got := map[string]bool{}
	for _, r := range due {
		got[r.ID] = true
	}

	if got[parkedOpen] {
		t.Error("PARKED and open must be excluded — no charge path admits it, so every tick burns an attempt and rewinds it while its frozen next_action_at heads this LIMITed queue")
	}
	if got[unknownWithPI] {
		t.Error("'unknown' WITH a PaymentIntent id must be excluded too — the claim requires pending/failed either way, and the reconciler settles only TERMINAL provider outcomes, so a PI at requires_action keeps this in flight forever")
	}
	if got[processing] {
		t.Error("'processing' must be excluded — ClaimChargeForDunningRetry cannot admit it, so selecting the run is wasted work every tick")
	}
	if !got[parkedWrittenOff] {
		t.Error("a WRITTEN-OFF source must still be selected — the processing path's terminal branch is what resolves the run, and mark-uncollectible's own resolve is best-effort by design; excluding it strands the run active forever")
	}
	if !got[ordinaryFailed] {
		t.Error("an ordinary failed invoice must remain due — this is the normal dunning path and the control for the assertions above")
	}
	if got[hostedLive] {
		t.Error("a run whose customer is paying on the hosted page must be excluded — the charge claim refuses it (one payer at a time), so selecting it spins at the head of the queue for the page's whole life")
	}
	if !got[hostedLiveWrittenOff] {
		t.Error("a written-off source with a hosted page must still be selected — the terminal branch resolves the run")
	}
	if got[held] {
		t.Error("a run whose invoice a charge attempt holds must be excluded — the charge claim refuses a set hold, stale included")
	}
	if !got[heldWrittenOff] {
		t.Error("a written-off source with a hold must still be selected — the terminal branch resolves the run")
	}
}

// seedChargeHold sets a stale charge hold on the run's invoice: a charge
// whose result was lost, waiting for the stale-hold sweep.
func seedChargeHold(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, runID string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE invoices SET auto_charge_claimed_until = now() - interval '1 hour'
		WHERE id = (SELECT r.invoice_id FROM invoice_dunning_runs r WHERE r.id = $1)
	`, runID); err != nil {
		t.Fatalf("seed charge hold: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// seedLiveHostedPage gives the run's invoice an open hosted Checkout session
// that is still payable at Stripe.
func seedLiveHostedPage(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, runID string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, stripe_session_id, amount_cents, currency, status, expires_at)
		SELECT $1, $2, r.invoice_id, false, 'cs_' || $1, 5000, 'USD', 'open', now() + interval '50 minutes'
		FROM invoice_dunning_runs r WHERE r.id = $3
	`, postgres.NewID("hc_cks"), tenantID, runID); err != nil {
		t.Fatalf("seed hosted page: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// The sim-time twin. ListDueRunsForClock lists every due run of the clock once, with what
// holds its retry: a charge of its open invoice in flight, the first of
// domain.ChargeNotInFlight the invoice fails, else nothing. The job steps
// only the runs nothing holds: the charge claim refuses the others, and a
// clock advance would spin on them. A voided or written-off invoice is never
// held: the processing path's terminal branch is what resolves its run. A
// run not yet due, paused, resolved, or on another clock is not listed.
func TestListDueRunsForClock_SaysWhatHoldsEachRun(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := "tnt_held_clock"
	const clockID, otherClockID = "hc_tclk_held", "hc_tclk_held_other"
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
	// 0021 livemode-autoset trigger reads the GUC; pin the tx to test mode.
	mustExec(`SELECT set_config('app.livemode', 'off', true)`)
	mustExec(`INSERT INTO tenants (id, name, status) VALUES ($1, 'Held Clock', 'active')`, tenantID)
	for _, c := range []string{clockID, otherClockID} {
		mustExec(`INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
			VALUES ($1, $2, $1, $3, 'ready', false)`, c, tenantID, frozen)
		mustExec(`INSERT INTO customers (id, tenant_id, external_id, display_name, email, test_clock_id, created_at, updated_at)
			VALUES ('cus_' || $2, $1, $2, 'Held', '', $2, $3, $3)`, tenantID, c, frozen)
	}
	mustExec(`INSERT INTO dunning_policies (id, tenant_id, name, enabled, is_default, retry_days, final_subscription_action, final_invoice_action, livemode)
		VALUES ('dpol_held', $1, 'P', true, true, '[3, 3, 3]', 'pause', 'none', false)`, tenantID)
	type seedRun struct {
		inv, run, payStatus, piID, invStatus, runState string
		next                                           time.Time
		paused                                         bool
		clock                                          string
	}
	due := frozen.Add(-time.Hour)
	seeds := []seedRun{
		{"inv_h_unknown", "run_h_unknown", "unknown", "", "finalized", "active", due, false, clockID},
		{"inv_h_pi", "run_h_pi", "unknown", "pi_sim_1", "finalized", "active", due, false, clockID},
		{"inv_h_proc", "run_h_proc", "processing", "pi_sim_2", "finalized", "active", due, false, clockID},
		{"inv_h_hold", "run_h_hold", "failed", "", "finalized", "active", due, false, clockID},
		{"inv_h_hosted", "run_h_hosted", "failed", "", "finalized", "active", due, false, clockID},
		{"inv_h_both", "run_h_both", "processing", "pi_sim_3", "finalized", "active", due, false, clockID},
		{"inv_h_wroff", "run_h_wroff", "unknown", "", "uncollectible", "active", due, false, clockID},
		{"inv_h_voided", "run_h_voided", "processing", "pi_sim_4", "voided", "active", due, false, clockID},
		{"inv_h_failed", "run_h_failed", "failed", "", "finalized", "active", due, false, clockID},
		{"inv_h_paid", "run_h_paid", "succeeded", "pi_sim_5", "paid", "resolved", due, false, clockID},
		{"inv_h_later", "run_h_later", "processing", "pi_sim_6", "finalized", "active", frozen.Add(time.Hour), false, clockID},
		{"inv_h_paused", "run_h_paused", "processing", "pi_sim_7", "finalized", "active", due, true, clockID},
		{"inv_h_other", "run_h_other", "failed", "", "finalized", "active", due, false, otherClockID},
	}
	for _, r := range seeds {
		mustExec(`INSERT INTO invoices (id, tenant_id, customer_id, invoice_number,
			status, payment_status, stripe_payment_intent_id, is_simulated, currency,
			subtotal_cents, total_amount_cents, amount_due_cents, tax_status,
			billing_period_start, billing_period_end, created_at, updated_at)
		VALUES ($1, $2, 'cus_' || $7, 'N-' || $1, $3, $4, NULLIF($5,''), true, 'USD', 2500, 2500, 2500, 'ok', $6, $6, $6, $6)`,
			r.inv, tenantID, r.invStatus, r.payStatus, r.piID, frozen.Add(-24*time.Hour), r.clock)
		mustExec(`INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, reason, attempt_count, next_action_at, paused, created_at, updated_at, livemode)
		VALUES ($1, $2, $3, 'cus_' || $8, 'dpol_held', $4, 'payment_failed', 0, $5, $6, $7, $7, false)`,
			r.run, tenantID, r.inv, r.runState, r.next, r.paused, frozen.Add(-24*time.Hour), r.clock)
	}
	mustExec(`UPDATE invoices SET auto_charge_claimed_until = now() - interval '1 hour' WHERE id = 'inv_h_hold'`)
	for _, inv := range []string{"inv_h_hosted", "inv_h_both"} {
		mustExec(`INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, stripe_session_id, amount_cents, currency, status, expires_at)
			VALUES ('cks_' || $2, $1, $2, false, 'cs_' || $2, 2500, 'USD', 'open', now() + interval '50 minutes')`, tenantID, inv)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	listed, err := dunning.NewPostgresStore(db).ListDueRunsForClock(ctx, tenantID, clockID, frozen, walk.Key{}, 50)
	if err != nil {
		t.Fatalf("ListDueRunsForClock: %v", err)
	}
	got := map[string]domain.ChargeClaimState{}
	for _, r := range listed {
		if _, twice := got[r.ID]; twice {
			t.Errorf("run %s is listed twice", r.ID)
		}
		got[r.ID] = r.HeldBy
		if r.InvoiceNumber != "N-"+r.InvoiceID {
			t.Errorf("run %s: invoice number %q, want %q", r.ID, r.InvoiceNumber, "N-"+r.InvoiceID)
		}
	}
	want := map[string]domain.ChargeClaimState{
		// unknown, with or without a PaymentIntent: the claim requires
		// pending or failed, and a PI at requires_action never resolves.
		"run_h_unknown": domain.ChargeHeldByAttempt,
		"run_h_pi":      domain.ChargeHeldByAttempt,
		"run_h_proc":    domain.ChargeHeldByAttempt,
		// A charge hold, live or stale: the claim refuses a set hold.
		"run_h_hold":   domain.ChargeHeldByAttempt,
		"run_h_hosted": domain.ChargePayingOnHostedPage,
		// Processing and a live hosted page: the first condition, the
		// payment's, says what holds it.
		"run_h_both": domain.ChargeHeldByAttempt,
		// A written-off or voided source is never held.
		"run_h_wroff":  domain.ChargeClaimable,
		"run_h_voided": domain.ChargeClaimable,
		// The control.
		"run_h_failed": domain.ChargeClaimable,
	}
	if len(got) != len(want) {
		t.Errorf("listed %v; want %v (not run_h_paid, run_h_later, run_h_paused or run_h_other)", got, want)
	}
	for run, state := range want {
		if s, ok := got[run]; !ok || s != state {
			t.Errorf("run %s: held by %v (listed %v), want %v", run, s, ok, state)
		}
	}
}
