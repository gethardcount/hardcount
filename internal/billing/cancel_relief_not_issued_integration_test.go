package billing_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// failingFirstIssue fails the first Issue of a relief credit note, as a
// database error or a deadline would, and passes every later call through.
type failingFirstIssue struct {
	billing.CreditNoteAdjuster
	mu     sync.Mutex
	failed bool
}

func (f *failingFirstIssue) Issue(ctx context.Context, tenantID, id string) (domain.CreditNote, error) {
	f.mu.Lock()
	first := !f.failed
	f.failed = true
	f.mu.Unlock()
	if first {
		return domain.CreditNote{}, errors.New("issue: database is shutting down")
	}
	return f.CreditNoteAdjuster.Issue(ctx, tenantID, id)
}

// A cancel's relief credit note is not issued in the call (the issuer
// fails). Collected in the same call, the final invoice charged the card in
// full, 3000c, and the 2450c relief landed on the balance of a customer with
// no subscription. Now the final invoice is not collected in the call. One
// billing run (the scheduler's tick body, as the leader runs it) issues the
// note (clawback_issue, a reconciler) before it collects queued invoices (the
// auto-charge step), and the card is charged 550c, net of the relief. The
// tick is run whole, so the test also pins that order.
//
// The rig is the #1272 one: a 4900c in-advance base, paid, canceled half way
// through the month (2450c of relief), and 3000c of usage on the final
// invoice. Card on file.
func TestCancel_ReliefNotIssued_FinalInvoiceWaitsForTheSweeps(t *testing.T) {
	db := testutil.SetupTestDB(t)
	for _, c := range []struct {
		name string
		run  func(*reliefCancelRig, *testing.T)
	}{
		{"immediate cancel", (*reliefCancelRig).cancelImmediately},
		{"scheduled cancel", (*reliefCancelRig).fireScheduledCancel},
	} {
		t.Run(c.name, func(t *testing.T) {
			const usageCents, wantDue = 3000, 3000 - reliefCents
			r := newReliefCancelRig(t, db, "Relief Not Issued "+c.name, reliefRigOpts{card: true, usageUnits: usageCents})
			r.engine.SetCreditNoteAdjuster(&failingFirstIssue{CreditNoteAdjuster: r.cn})
			c.run(r, t)

			final := r.finalInvoice(t)
			if charges := r.charger.charges(final.ID); len(charges) != 0 {
				t.Fatalf("final invoice charged %v in the cancel's own call, want nothing: the relief was not issued, so the card would pay all %dc", charges, usageCents)
			}
			if final.AmountDueCents != usageCents || final.Status != domain.InvoiceFinalized {
				t.Fatalf("after the cancel: final invoice %s with %dc due, want finalized with %dc due (not collected yet)", final.Status, final.AmountDueCents, usageCents)
			}

			// One billing run, as the leader runs it.
			s := billing.NewScheduler(r.engine, time.Hour, 50, nil, nil, clock.NewFake(r.cancelAt.Add(time.Hour)))
			s.SetClawbackRetrier(r.cn)
			billing.RunBillingHalf(s, r.ctx)

			final = r.invoice(t, final.ID)
			if final.CreditsAppliedCents != reliefCents {
				t.Errorf("final invoice credits_applied = %dc, want %dc: the relief must reduce it", final.CreditsAppliedCents, reliefCents)
			}
			if charges := r.charger.charges(final.ID); len(charges) != 1 || charges[0] != wantDue {
				t.Errorf("final invoice charges = %v, want one charge of %dc, net of the relief", charges, wantDue)
			}
			if bal := r.balance(t); bal != 0 {
				t.Errorf("credit balance = %dc, want 0 (the relief was spent on the final invoice)", bal)
			}
		})
	}
}

// The same on a test clock. The wall-clock billing run skips simulated rows,
// so a clock-pinned relief draft is issued only by the catch-up
// (RetryPendingClawbackIssueForClock) and the final invoice is charged only by
// it (RetryPendingChargesForClock). The catch-up charged before it issued
// clawback drafts, so the next Advance charged the card 3000c and only then
// issued the relief. It now issues pending drafts before the charge phase too.
func TestCancel_ReliefNotIssued_OnATestClock_TheAdvanceChargesNetOfTheRelief(t *testing.T) {
	db := testutil.SetupTestDB(t)
	const usageCents, wantDue = 3000, 3000 - reliefCents
	r := newReliefCancelRig(t, db, "Relief Not Issued Test Clock", reliefRigOpts{card: true, usageUnits: usageCents})

	clockID := postgres.NewID("hc_tclk")
	tx, err := db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO test_clocks (id, tenant_id, livemode, name, frozen_time, status) VALUES ($1, $2, false, 'relief clock', $3, 'ready')`, []any{clockID, r.tenantID, r.cancelAt}},
		{`UPDATE customers SET test_clock_id = $1 WHERE id = $2`, []any{clockID, r.customerID}},
		{`UPDATE subscriptions SET test_clock_id = $1 WHERE id = $2`, []any{clockID, r.subID}},
		{`UPDATE invoices SET is_simulated = true WHERE customer_id = $1`, []any{r.customerID}},
	} {
		if _, err := tx.ExecContext(r.ctx, q.sql, q.args...); err != nil {
			postgres.Rollback(tx)
			t.Fatalf("pin to the clock: %s: %v", q.sql, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	clocks := testclock.NewPostgresStore(db)
	r.engine.SetTestClockReader(clocks)
	r.subSvc.SetResolver(r.engine)
	r.engine.SetCreditNoteAdjuster(&failingFirstIssue{CreditNoteAdjuster: r.cn})

	r.cancelImmediately(t)
	final := r.finalInvoice(t)
	if !final.IsSimulated {
		t.Fatal("setup: the final invoice must be on the test clock")
	}
	if charges := r.charger.charges(final.ID); len(charges) != 0 {
		t.Fatalf("final invoice charged %v in the cancel's own call, want nothing", charges)
	}

	// The next Advance's catch-up.
	execBypassOnce(t, db, `UPDATE test_clocks SET status = 'advancing' WHERE id = $1`, clockID)
	orchestrator := testclock.NewService(clocks)
	orchestrator.SetBillingRunner(r.engine)
	orchestrator.SetClawbackRetrier(r.cn)
	if err := orchestrator.RunCatchup(leadertest.Token(t, testutil.AdminPool(t), r.ctx, leader.RoleTestClockCatchup), testclock.CatchupJob{TenantID: r.tenantID, ClockID: clockID}); err != nil {
		t.Fatalf("catch-up: %v", err)
	}

	final = r.invoice(t, final.ID)
	if final.CreditsAppliedCents != reliefCents {
		t.Errorf("final invoice credits_applied = %dc, want %dc: the relief must reduce it", final.CreditsAppliedCents, reliefCents)
	}
	if charges := r.charger.charges(final.ID); len(charges) != 1 || charges[0] != wantDue {
		t.Errorf("final invoice charges = %v, want one charge of %dc, net of the relief", charges, wantDue)
	}
	if bal := r.balance(t); bal != 0 {
		t.Errorf("credit balance = %dc, want 0", bal)
	}
}

func execBypassOnce(t *testing.T, db *postgres.DB, q string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
