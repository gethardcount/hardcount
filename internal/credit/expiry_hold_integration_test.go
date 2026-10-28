package credit_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// ADR-071 §2 (amended 2026-10-19): the expiry sweep does not retire a grant
// while a draw that can still use it is owed. A grant applies to a period's
// invoice when the period ends before the grant expires (Stripe: "The
// invoice's period_end occurs before the credit grant's expires_at time").
// The real trigger is a close that fails on billing day and waits on the
// ADR-119 retry ladder while the grant passes expires_at; retiring the grant
// then charges the card for usage the grant should have paid.

// bypassExec runs one statement as the table owner in the test-mode
// partition: the fixture writes production has no single writer for.
func bypassExec(t *testing.T, db *postgres.DB, ctx context.Context, q string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.livemode', 'off', true)`); err != nil {
		t.Fatalf("set livemode: %v", err)
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		t.Fatalf("exec: %v\n%s", err, q)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func holdCustomer(t *testing.T, ctx context.Context, db *postgres.DB, tenantID, name string) string {
	t.Helper()
	c, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_" + name, DisplayName: name})
	if err != nil {
		t.Fatalf("create customer %s: %v", name, err)
	}
	return c.ID
}

// seedSub inserts a subscription of customerID in status whose next period
// end is next, as the billing run leaves one. clockID pins it to a test clock
// ("" for wall clock). The hold reads only the status and the due instant.
func seedSub(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, customerID, status string, next time.Time, clockID string) string {
	t.Helper()
	id := postgres.NewID("hc_sub")
	bypassExec(t, db, ctx, `INSERT INTO subscriptions (
			id, tenant_id, code, display_name, customer_id, status, billing_time,
			current_billing_period_start, current_billing_period_end, next_billing_at,
			livemode, test_clock_id)
		VALUES ($1, $2, $1, $1, $3, $4, 'calendar', $5, $6, $6, false, $7)`,
		id, tenantID, customerID, status, next.AddDate(0, -1, 0), next, postgres.NullableString(clockID))
	return id
}

// setNextBilling moves a subscription's due instant, as a successful close
// does when it advances the period.
func setNextBilling(t *testing.T, db *postgres.DB, ctx context.Context, subID string, next time.Time) {
	t.Helper()
	bypassExec(t, db, ctx, `UPDATE subscriptions SET current_billing_period_end = $1, next_billing_at = $1 WHERE id = $2`, next, subID)
}

// recordFailure saves a counted billing failure on the close due at due, with
// the next attempt 15 minutes out: the first rung of the ADR-119 ladder.
func recordFailure(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, subID string, due, now time.Time) {
	t.Helper()
	next := now.Add(15 * time.Minute)
	ok, err := subscription.NewPostgresStore(db).RecordBillingFailure(ctx, tenantID, subID, subscription.BillingFailureWrite{
		DueSeen: due, ObservedAttempts: 0, Attempts: 1, FailedAt: now,
		NextAttemptAt: &next, LastError: "plan lookup refused (test)",
	})
	if err != nil || !ok {
		t.Fatalf("record billing failure on %s: recorded=%v err=%v", subID, ok, err)
	}
}

func expireAll(t *testing.T, ctx context.Context, svc *credit.Service) {
	t.Helper()
	if _, errs := svc.ExpireCredits(ctx); len(errs) > 0 {
		t.Fatalf("expire credits: %v", errs)
	}
}

// seedPeriodInvoice writes an invoice of customerID issued at issued, for
// $50.00, through the real store (which queues a finalized, unpaid invoice
// for collection in the same insert).
func seedPeriodInvoice(t *testing.T, ctx context.Context, store *invoice.PostgresStore, tenantID, customerID, subID string, reason domain.InvoiceBillingReason, status domain.InvoiceStatus, taxStatus domain.InvoiceTaxStatus, issued time.Time) domain.Invoice {
	t.Helper()
	due := issued.Add(7 * 24 * time.Hour)
	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		InvoiceNumber:      fmt.Sprintf("HOLD-%d", invoiceSeq.Add(1)),
		CustomerID:         customerID,
		SubscriptionID:     subID,
		Status:             status,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		SubtotalCents:      5000,
		TotalAmountCents:   5000,
		AmountDueCents:     5000,
		BillingPeriodStart: issued.AddDate(0, -1, 0),
		BillingPeriodEnd:   issued,
		IssuedAt:           &issued,
		DueAt:              &due,
		CreatedAt:          issued,
		BillingReason:      reason,
		TaxFacts:           domain.TaxFacts{TaxStatus: taxStatus},
	})
	if err != nil {
		t.Fatalf("create %s invoice: %v", reason, err)
	}
	return inv
}

// TestExpireCredits_HoldsAGrantWhileAFailedCloseIsOwed: billing day was 30
// hours ago, the close failed and waits on the retry ladder, and the
// customer's grant expired two hours after the period ended. The sweep must
// not retire it: the close, when it succeeds, draws on credit that was live
// at the period end. A trialing subscription owes its close the same way; a
// canceled one owes nothing. Once the close has run (the due instant moves
// past the expiry) and no invoice is owed, the grant is retired.
//
// Fails before the fix: the sweep retired the grant at expires_at plus the
// invoice grace. Mutation: drop clause (i) of domain.CreditDrawOwedSQL.
func TestExpireCredits_HoldsAGrantWhileAFailedCloseIsOwed(t *testing.T) {
	db, ctx, tenantID, cust, _, svc, _ := expirySetup(t, "expiry_hold_failed_close")
	now := time.Now().UTC().Truncate(time.Microsecond)
	due := now.Add(-30 * time.Hour)
	expires := now.Add(-28 * time.Hour)

	active := seedSub(t, db, ctx, tenantID, cust.ID, "active", due, "")
	recordFailure(t, db, ctx, tenantID, active, due, now)
	activeGrant := seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 5000, expires)

	trialCust := holdCustomer(t, ctx, db, tenantID, "hold_trial")
	trial := seedSub(t, db, ctx, tenantID, trialCust, "trialing", due, "")
	recordFailure(t, db, ctx, tenantID, trial, due, now)
	trialGrant := seedExpiredGrant(t, db, ctx, svc, tenantID, trialCust, 3000, expires)

	canceledCust := holdCustomer(t, ctx, db, tenantID, "hold_canceled")
	seedSub(t, db, ctx, tenantID, canceledCust, "canceled", due, "")
	canceledGrant := seedExpiredGrant(t, db, ctx, svc, tenantID, canceledCust, 2000, expires)

	expireAll(t, ctx, svc)
	if got := grantConsumed(t, db, ctx, activeGrant); got != 0 {
		t.Fatalf("active sub's grant consumed=%d after the sweep, want 0: its failed close is still owed", got)
	}
	if got := grantConsumed(t, db, ctx, trialGrant); got != 0 {
		t.Fatalf("trialing sub's grant consumed=%d after the sweep, want 0: its close is still owed", got)
	}
	if got := grantConsumed(t, db, ctx, canceledGrant); got != 2000 {
		t.Fatalf("canceled sub's grant consumed=%d, want 2000 retired: a canceled subscription owes no close", got)
	}

	// The retries succeed: each close moves the due instant past the expiry,
	// and no invoice is left owed.
	setNextBilling(t, db, ctx, active, now.Add(30*24*time.Hour))
	setNextBilling(t, db, ctx, trial, now.Add(30*24*time.Hour))
	expireAll(t, ctx, svc)
	if sum, entries, total := customerLedgerFacts(t, db, ctx, cust.ID); sum != 0 || entries != 1 || total != 5000 {
		t.Fatalf("active customer after the close: sum=%d expiry entries=%d total=%d, want 0/1/5000", sum, entries, total)
	}
	if sum, entries, total := customerLedgerFacts(t, db, ctx, trialCust); sum != 0 || entries != 1 || total != 3000 {
		t.Fatalf("trialing customer after the close: sum=%d expiry entries=%d total=%d, want 0/1/3000", sum, entries, total)
	}
}

// TestExpireCredits_HoldsAGrantWhileAPeriodInvoiceIsUncollected: the close
// ran (the due instant is in the next period) but Stripe Tax was down, so the
// period's invoice is a draft (#653). The grant expired after the period
// ended. While the draft waits for tax, and then while the finalized invoice
// waits in the collection queue, the grant is held: that invoice still draws
// on it. Once the invoice is settled the grant is retired. A manual draft and
// a subscription_cancel invoice issued before the expiry hold nothing: they
// test expiry at the instant they are collected.
//
// Fails before the fix. Mutations: drop `status = 'draft'` (the first sweep
// retires it) or drop `auto_charge_pending` (the second does).
func TestExpireCredits_HoldsAGrantWhileAPeriodInvoiceIsUncollected(t *testing.T) {
	db, ctx, tenantID, cust, _, svc, invoiceStore := expirySetup(t, "expiry_hold_period_invoice")
	now := time.Now().UTC().Truncate(time.Microsecond)
	issued := now.Add(-3 * time.Hour)
	expires := now.Add(-2 * time.Hour)

	sub := seedSub(t, db, ctx, tenantID, cust.ID, "active", issued.AddDate(0, 1, 0), "")
	grant := seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 4000, expires)
	draft := seedPeriodInvoice(t, ctx, invoiceStore, tenantID, cust.ID, sub,
		domain.BillingReasonSubscriptionCycle, domain.InvoiceDraft, domain.InvoiceTaxPending, issued)

	manualCust := holdCustomer(t, ctx, db, tenantID, "hold_manual")
	manualGrant := seedExpiredGrant(t, db, ctx, svc, tenantID, manualCust, 1000, expires)
	seedPeriodInvoice(t, ctx, invoiceStore, tenantID, manualCust, "",
		domain.BillingReasonManual, domain.InvoiceDraft, domain.InvoiceTaxOK, issued)

	cancelCust := holdCustomer(t, ctx, db, tenantID, "hold_cancel_final")
	cancelGrant := seedExpiredGrant(t, db, ctx, svc, tenantID, cancelCust, 1500, expires)
	queued := seedPeriodInvoice(t, ctx, invoiceStore, tenantID, cancelCust, "",
		domain.BillingReasonSubscriptionCancel, domain.InvoiceFinalized, domain.InvoiceTaxOK, issued)
	if !queued.AutoChargePending {
		t.Fatal("setup: a finalized unpaid invoice must be queued for collection")
	}

	expireAll(t, ctx, svc)
	if got := grantConsumed(t, db, ctx, grant); got != 0 {
		t.Fatalf("grant consumed=%d while the period's draft waits for tax, want 0 (held)", got)
	}
	if got := grantConsumed(t, db, ctx, manualGrant); got != 1000 {
		t.Fatalf("manual draft's customer: grant consumed=%d, want 1000 retired (a manual invoice holds nothing)", got)
	}
	if got := grantConsumed(t, db, ctx, cancelGrant); got != 1500 {
		t.Fatalf("cancel invoice's customer: grant consumed=%d, want 1500 retired (a subscription_cancel invoice holds nothing)", got)
	}

	// Tax resolves and the draft is finalized: it is now queued for
	// collection, and still holds the grant.
	bypassExec(t, db, ctx, `UPDATE invoices SET tax_status = 'ok', tax_error_code = NULL, tax_pending_reason = '' WHERE id = $1`, draft.ID)
	fin, err := invoiceStore.UpdateStatus(ctx, tenantID, draft.ID, domain.InvoiceFinalized)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if !fin.AutoChargePending {
		t.Fatal("setup: finalize must queue the invoice for collection")
	}
	expireAll(t, ctx, svc)
	if got := grantConsumed(t, db, ctx, grant); got != 0 {
		t.Fatalf("grant consumed=%d while the finalized invoice is queued for collection, want 0 (held)", got)
	}

	// Settled: nothing owed draws on the grant any more.
	if _, err := invoiceStore.MarkPaid(ctx, tenantID, draft.ID, "", now); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	expireAll(t, ctx, svc)
	if sum, entries, total := customerLedgerFacts(t, db, ctx, cust.ID); sum != 0 || entries != 1 || total != 4000 {
		t.Fatalf("after the invoice settled: sum=%d expiry entries=%d total=%d, want 0/1/4000", sum, entries, total)
	}
}

// TestExpireCreditsForClock_HoldsAGrantAFailedCloseNeeds: on a test clock
// the catch-up runs every phase even when one fails, so the expiry phase
// runs after a failed period close (testclock catch-up, Phase 1 then
// Phase 4). The grant expired after the period ended and before the clock's
// frozen time; it is held while the close is owed, and retired once it ran.
//
// Fails before the fix. Mutation: drop the hold from ListExpiredGrantsForClock
// only.
func TestExpireCreditsForClock_HoldsAGrantAFailedCloseNeeds(t *testing.T) {
	db, ctx, tenantID, cust, _, svc, _ := expirySetup(t, "expiry_hold_clock")
	now := time.Now().UTC().Truncate(time.Microsecond)
	frozen := now.Add(-10 * 24 * time.Hour)
	clockID := postgres.NewID("hc_tclk")
	bypassExec(t, db, ctx, `INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		VALUES ($1, $2, 'g8', $3, 'ready', false)`, clockID, tenantID, frozen)
	bypassExec(t, db, ctx, `UPDATE customers SET test_clock_id = $1 WHERE id = $2`, clockID, cust.ID)

	due := frozen.Add(-30 * time.Hour)
	sub := seedSub(t, db, ctx, tenantID, cust.ID, "active", due, clockID)
	grant := seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 5000, frozen.Add(-28*time.Hour))

	if _, errs := svc.ExpireCreditsForClock(ctx, tenantID, clockID, frozen); len(errs) > 0 {
		t.Fatalf("expire for clock: %v", errs)
	}
	if got := grantConsumed(t, db, ctx, grant); got != 0 {
		t.Fatalf("grant consumed=%d after the clock's expiry phase, want 0: the period close is still owed", got)
	}

	setNextBilling(t, db, ctx, sub, frozen.Add(20*24*time.Hour))
	if _, errs := svc.ExpireCreditsForClock(ctx, tenantID, clockID, frozen); len(errs) > 0 {
		t.Fatalf("expire for clock after the close: %v", errs)
	}
	if got := grantConsumed(t, db, ctx, grant); got != 5000 {
		t.Fatalf("grant consumed=%d after the close ran, want 5000 retired", got)
	}
}

// TestExpireCredits_NoGrace_HoldIsTheOwedClose: the sweep no longer waits
// out INVOICE_GRACE_PERIOD after a grant's expiry. A period that ended 30
// minutes ago is still inside its grace, its close not yet run, so its
// customer's grant that expired 10 minutes ago is held. A customer with no
// subscription owes no close: the same grant is retired on the first sweep,
// not an hour later.
//
// Fails before the fix (the first customer's grant is retired). Mutation:
// put the grace back in ListExpiredGrants (`expires_at < NOW() - 1h`) and the
// second customer's grant stays on the ledger.
func TestExpireCredits_NoGrace_HoldIsTheOwedClose(t *testing.T) {
	db, ctx, tenantID, cust, _, svc, _ := expirySetup(t, "expiry_hold_no_grace")
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedSub(t, db, ctx, tenantID, cust.ID, "active", now.Add(-30*time.Minute), "")
	held := seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 500, now.Add(-10*time.Minute))

	other := holdCustomer(t, ctx, db, tenantID, "hold_no_sub")
	gone := seedExpiredGrant(t, db, ctx, svc, tenantID, other, 700, now.Add(-10*time.Minute))

	expireAll(t, ctx, svc)
	if got := grantConsumed(t, db, ctx, held); got != 0 {
		t.Fatalf("grant consumed=%d while its period's close waits for the grace, want 0 (held)", got)
	}
	if got := grantConsumed(t, db, ctx, gone); got != 700 {
		t.Fatalf("no-subscription customer's grant consumed=%d, want 700: nothing is owed, so it is retired now", got)
	}
}

// One liveness rule (ADR-071 §2, amended 2026-10-22): a credit block is live
// at t when t <= expires_at. A dashboard grant "valid through Nov 30" is sent
// as the local midnight after Nov 30, and the period invoice is issued at
// that same midnight, so the boundary instant itself must count as live.

// TestApplyToInvoice_PeriodInvoiceIssuedAtTheExpiryInstantDrawsTheGrant: a
// period invoice and a threshold invoice issued exactly at the grant's
// expires_at draw the grant, however late they are collected. A manual
// invoice collected after that instant draws nothing (control).
//
// Fails before the fix: the drain took only blocks with expires_at after the
// draw instant. Mutation: put `expires_at > $3` back in drainPositiveBlocks.
func TestApplyToInvoice_PeriodInvoiceIssuedAtTheExpiryInstantDrawsTheGrant(t *testing.T) {
	db, ctx, tenantID, cust, store, svc, invoiceStore := expirySetup(t, "expiry_boundary_draw")
	now := time.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(-3 * time.Hour)

	cases := []struct {
		name   string
		reason domain.InvoiceBillingReason
		drawn  int64
	}{
		{"cycle", domain.BillingReasonSubscriptionCycle, 2000},
		{"threshold", domain.BillingReasonThreshold, 2000},
		{"manual", domain.BillingReasonManual, 0},
	}
	for i, tc := range cases {
		custID := cust.ID
		if i > 0 {
			custID = holdCustomer(t, ctx, db, tenantID, "boundary_"+tc.name)
		}
		seedExpiredGrant(t, db, ctx, svc, tenantID, custID, 2000, expires)
		inv := seedPeriodInvoice(t, ctx, invoiceStore, tenantID, custID, "",
			tc.reason, domain.InvoiceFinalized, domain.InvoiceTaxOK, expires)
		if _, err := store.ApplyToInvoiceAtomic(ctx, tenantID, custID, inv.ID, "apply", inv.AmountDueCents, now); err != nil {
			t.Fatalf("%s: apply: %v", tc.name, err)
		}
		got, err := invoiceStore.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("%s: get invoice: %v", tc.name, err)
		}
		if got.CreditsAppliedCents != tc.drawn || got.AmountDueCents != 5000-tc.drawn {
			t.Errorf("%s: credits applied/amount due = %d/%d, want %d/%d", tc.name, got.CreditsAppliedCents, got.AmountDueCents, tc.drawn, 5000-tc.drawn)
		}
	}
}

// TestExpireCredits_HoldsAGrantExpiringAtTheDueInstant: the subscription is
// due exactly when the grant expires, and its close failed. The close draws
// on the grant (it is live at the period end), so the sweep must hold it.
// Once the close has run, nothing is owed and the grant is retired.
//
// Fails before the fix: arm (i) of the hold was `due < expires_at`.
// Mutation: put `<` back in arm (i) of domain.CreditDrawOwedSQL.
func TestExpireCredits_HoldsAGrantExpiringAtTheDueInstant(t *testing.T) {
	db, ctx, tenantID, cust, _, svc, _ := expirySetup(t, "expiry_hold_due_instant")
	now := time.Now().UTC().Truncate(time.Microsecond)
	due := now.Add(-2 * time.Hour)

	sub := seedSub(t, db, ctx, tenantID, cust.ID, "active", due, "")
	recordFailure(t, db, ctx, tenantID, sub, due, now)
	grant := seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 5000, due)

	expireAll(t, ctx, svc)
	if got := grantConsumed(t, db, ctx, grant); got != 0 {
		t.Fatalf("grant consumed=%d after the sweep, want 0: the close due at its expires_at still draws on it", got)
	}

	setNextBilling(t, db, ctx, sub, now.Add(30*24*time.Hour))
	expireAll(t, ctx, svc)
	if sum, entries, total := customerLedgerFacts(t, db, ctx, cust.ID); sum != 0 || entries != 1 || total != 5000 {
		t.Fatalf("after the close: sum=%d expiry entries=%d total=%d, want 0/1/5000", sum, entries, total)
	}
}

// TestExpireCredits_HoldsAGrantExpiringAtAPeriodInvoiceIssue: the close ran
// at the grant's expires_at, but its invoice is a tax-pending draft. The
// draft, and then the queued finalized invoice, still draw on the grant, so
// the sweep holds it until the invoice is settled.
//
// Fails before the fix: arm (ii) of the hold was `issued_at < expires_at`.
// Mutation: put `<` back in arm (ii) of domain.CreditDrawOwedSQL.
func TestExpireCredits_HoldsAGrantExpiringAtAPeriodInvoiceIssue(t *testing.T) {
	db, ctx, tenantID, cust, _, svc, invoiceStore := expirySetup(t, "expiry_hold_issue_instant")
	now := time.Now().UTC().Truncate(time.Microsecond)
	issued := now.Add(-3 * time.Hour)

	sub := seedSub(t, db, ctx, tenantID, cust.ID, "active", issued.AddDate(0, 1, 0), "")
	grant := seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 4000, issued)
	draft := seedPeriodInvoice(t, ctx, invoiceStore, tenantID, cust.ID, sub,
		domain.BillingReasonSubscriptionCycle, domain.InvoiceDraft, domain.InvoiceTaxPending, issued)

	expireAll(t, ctx, svc)
	if got := grantConsumed(t, db, ctx, grant); got != 0 {
		t.Fatalf("grant consumed=%d while the draft issued at its expires_at waits for tax, want 0 (held)", got)
	}

	bypassExec(t, db, ctx, `UPDATE invoices SET tax_status = 'ok', tax_error_code = NULL, tax_pending_reason = '' WHERE id = $1`, draft.ID)
	fin, err := invoiceStore.UpdateStatus(ctx, tenantID, draft.ID, domain.InvoiceFinalized)
	if err != nil || !fin.AutoChargePending {
		t.Fatalf("finalize: queued=%v err=%v, want queued", fin.AutoChargePending, err)
	}
	expireAll(t, ctx, svc)
	if got := grantConsumed(t, db, ctx, grant); got != 0 {
		t.Fatalf("grant consumed=%d while the finalized invoice is queued, want 0 (held)", got)
	}

	if _, err := invoiceStore.MarkPaid(ctx, tenantID, draft.ID, "", now); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	expireAll(t, ctx, svc)
	if sum, entries, total := customerLedgerFacts(t, db, ctx, cust.ID); sum != 0 || entries != 1 || total != 4000 {
		t.Fatalf("after the invoice settled: sum=%d expiry entries=%d total=%d, want 0/1/4000", sum, entries, total)
	}
}

// TestLivenessAtTheExpiryInstant_DrainSpendableExposureAgree: the drain, the
// spendable balance and the commit banner apply one rule. At expires_at the
// grant is live in all three; one microsecond later it is live in none, and
// the sweep lists it.
//
// Fails before the fix: all three treated expires_at itself as expired.
// Mutations: put `expires_at > $3` back in CommitGrantExposure, or
// `expires_at <= $3` back in GetSpendableBalance.
func TestLivenessAtTheExpiryInstant_DrainSpendableExposureAgree(t *testing.T) {
	db, ctx, tenantID, cust, store, svc, invoiceStore := expirySetup(t, "expiry_boundary_agree")
	now := time.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(-3 * time.Hour)
	after := expires.Add(time.Microsecond)

	grant := seedExpiredGrant(t, db, ctx, svc, tenantID, cust.ID, 2000, expires)
	fundingID := "hc_inv_boundary_fund"
	bypassExec(t, db, ctx, `UPDATE customer_credit_ledger SET grant_kind = 'commit', source_invoice_id = $1 WHERE id = $2`, fundingID, grant)

	spendable := func(at time.Time) int64 {
		t.Helper()
		v, err := store.GetSpendableBalance(ctx, tenantID, cust.ID, at)
		if err != nil {
			t.Fatalf("spendable at %v: %v", at, err)
		}
		return v["USD"]
	}
	drawable := func(at time.Time) int64 {
		t.Helper()
		v, _, err := store.CommitGrantExposure(ctx, tenantID, fundingID, at)
		if err != nil {
			t.Fatalf("commit exposure at %v: %v", at, err)
		}
		return v
	}
	drain := func(at time.Time) int64 {
		t.Helper()
		inv := createOpenInvoice(t, ctx, invoiceStore, tenantID, cust.ID, 5000)
		applied, err := store.ApplyToInvoiceAtomic(ctx, tenantID, cust.ID, inv.ID, "apply", 5000, at)
		if err != nil {
			t.Fatalf("apply at %v: %v", at, err)
		}
		return applied
	}

	if got := spendable(expires); got != 2000 {
		t.Errorf("spendable at expires_at = %d, want 2000", got)
	}
	if got := drawable(expires); got != 2000 {
		t.Errorf("commit drawable at expires_at = %d, want 2000", got)
	}
	if got := spendable(after); got != 0 {
		t.Errorf("spendable 1µs after expires_at = %d, want 0", got)
	}
	if got := drawable(after); got != 0 {
		t.Errorf("commit drawable 1µs after expires_at = %d, want 0", got)
	}
	listed, err := store.ListExpiredGrants(ctx)
	if err != nil {
		t.Fatalf("list expired grants: %v", err)
	}
	found := false
	for _, g := range listed {
		found = found || g.ID == grant
	}
	if !found {
		t.Errorf("ListExpiredGrants does not list the grant past its expires_at")
	}
	if got := drain(after); got != 0 {
		t.Errorf("drain 1µs after expires_at = %d, want 0", got)
	}
	if got := drain(expires); got != 2000 {
		t.Errorf("drain at expires_at = %d, want 2000", got)
	}
}
