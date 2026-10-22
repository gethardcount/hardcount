package subscription

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// The two store writers of A3 (ADR-036 amendment 2026-10-22) on real
// Postgres: SetDunningPause, dunning's pause, and ReleaseDunningPauseTx, the
// payment's release.

type dunningPauseRig struct {
	t        *testing.T
	ctx      context.Context
	db       *postgres.DB
	tenantID string
	subs     *PostgresStore
	invs     *invoice.PostgresStore
	subID    string
	custID   string
}

func newDunningPauseRig(t *testing.T, name string) *dunningPauseRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	r := &dunningPauseRig{t: t, ctx: ctx, db: db, tenantID: testutil.CreateTestTenant(t, db, name),
		subs: NewPostgresStore(db), invs: invoice.NewPostgresStore(db)}
	r.invs.SetDunningPauseReleaser(r.subs)
	cust, err := customer.NewPostgresStore(db).Create(ctx, r.tenantID, domain.Customer{ExternalID: "cus_a3", DisplayName: "A3"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	r.custID = cust.ID
	plan, err := pricing.NewPostgresStore(db).CreatePlan(ctx, r.tenantID, domain.Plan{
		Code: "a3", Name: "A3", Currency: "USD", BillingInterval: domain.BillingMonthly, Status: domain.PlanActive, BaseAmountCents: 1000,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	start := time.Now().UTC().Add(-24 * time.Hour)
	end := start.AddDate(0, 1, 0)
	sub, err := r.subs.Create(ctx, r.tenantID, domain.Subscription{
		Code: "a3", DisplayName: "A3", CustomerID: cust.ID, Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeAnniversary,
		StartedAt: &start, CurrentBillingPeriodStart: &start, CurrentBillingPeriodEnd: &end, NextBillingAt: &end,
		Items: []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("create subscription: %v", err)
	}
	r.subID = sub.ID
	return r
}

// invoice adds a finalized invoice with due cents owed.
func (r *dunningPauseRig) invoice(number string, due int64) string {
	r.t.Helper()
	now := time.Now().UTC()
	inv, err := r.invs.Create(r.ctx, r.tenantID, domain.Invoice{
		CustomerID: r.custID, SubscriptionID: r.subID, InvoiceNumber: number,
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, Currency: "USD",
		SubtotalCents: 1000, TotalAmountCents: 1000, AmountDueCents: due,
		BillingPeriodStart: now.Add(-24 * time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		r.t.Fatalf("create invoice %s: %v", number, err)
	}
	if _, err := testutil.AdminPool(r.t).Exec(`UPDATE invoices SET tax_status = 'ok' WHERE id = $1`, inv.ID); err != nil {
		r.t.Fatalf("tax ok: %v", err)
	}
	return inv.ID
}

func (r *dunningPauseRig) pause() *domain.PauseCollection {
	r.t.Helper()
	sub, err := r.subs.Get(r.ctx, r.tenantID, r.subID)
	if err != nil {
		r.t.Fatalf("get subscription: %v", err)
	}
	return sub.PauseCollection
}

// SetDunningPause never overrides a pause in force, replaces one whose
// resumes_at has passed, and refuses a canceled subscription.
func TestSetDunningPause_Rules(t *testing.T) {
	t.Run("operator timed pause kept", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Rules Timed")
		x := r.invoice("INV-A3-R1", 1000)
		resumes := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Microsecond)
		if _, err := r.subs.SetPauseCollection(r.ctx, r.tenantID, r.subID, domain.PauseCollection{
			Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: &resumes,
		}); err != nil {
			t.Fatalf("operator pause: %v", err)
		}
		_, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x)
		if err != nil || outcome != DunningPauseAlreadyHeld {
			t.Fatalf("SetDunningPause = %v, %v; want DunningPauseAlreadyHeld", outcome, err)
		}
		pc := r.pause()
		if pc == nil || pc.SetBy != domain.PauseSetByOperator || pc.ResumesAt == nil || !pc.ResumesAt.Equal(resumes) {
			t.Errorf("pause = %+v, want the operator's, resuming %v", pc, resumes)
		}
	})

	t.Run("dunning pause kept", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Rules Twice")
		x := r.invoice("INV-A3-R2", 1000)
		if _, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x); err != nil || outcome != DunningPauseWritten {
			t.Fatalf("first SetDunningPause = %v, %v; want written", outcome, err)
		}
		if _, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x); err != nil || outcome != DunningPauseAlreadyHeld {
			t.Fatalf("second SetDunningPause = %v, %v; want DunningPauseAlreadyHeld", outcome, err)
		}
	})

	t.Run("lapsed operator pause replaced", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Rules Lapsed")
		x := r.invoice("INV-A3-R3", 1000)
		resumes := time.Now().UTC().Add(time.Hour)
		if _, err := r.subs.SetPauseCollection(r.ctx, r.tenantID, r.subID, domain.PauseCollection{
			Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: &resumes,
		}); err != nil {
			t.Fatalf("operator pause: %v", err)
		}
		// The resumes_at passed; the resumer has not run yet.
		if _, err := testutil.AdminPool(t).Exec(`UPDATE subscriptions SET pause_collection_resumes_at = now() - interval '1 minute' WHERE id = $1`, r.subID); err != nil {
			t.Fatalf("lapse the pause: %v", err)
		}
		if _, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x); err != nil || outcome != DunningPauseWritten {
			t.Fatalf("SetDunningPause = %v, %v; want written", outcome, err)
		}
		if pc := r.pause(); pc == nil || pc.SetBy != domain.PauseSetByDunning || pc.ResumesAt != nil {
			t.Errorf("pause = %+v, want dunning's with no resumes_at", pc)
		}
	})

	t.Run("canceled subscription", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Rules Canceled")
		x := r.invoice("INV-A3-R4", 1000)
		if _, err := testutil.AdminPool(t).Exec(`UPDATE subscriptions SET status = 'canceled', canceled_at = now() WHERE id = $1`, r.subID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if _, _, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x); !errors.Is(err, errs.ErrInvalidState) {
			t.Fatalf("SetDunningPause on a canceled subscription: %v, want invalid state", err)
		}
	})
}

// T6 (C2). A payment commits while dunning's pause is being written. The
// pause locks the invoice first, so it waits for the payment's transaction,
// then finds the invoice paid and pauses nothing. Mutation: drop the
// invoice FOR NO KEY UPDATE in SetDunningPause and the pause lands on a paid
// debt, with nothing left to lift it.
func TestSetDunningPause_PaymentCommitsFirst(t *testing.T) {
	r := newDunningPauseRig(t, "A3 Pause Race")
	x := r.invoice("INV-A3-RACE", 0) // nothing due: SettleZeroDueTx is the paid writer the test can hold open

	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var one int
	if err := tx.QueryRowContext(r.ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR NO KEY UPDATE`, x).Scan(&one); err != nil {
		t.Fatalf("lock invoice: %v", err)
	}
	if _, paid, err := r.invs.SettleZeroDueTx(r.ctx, tx, r.tenantID, x, time.Now().UTC()); err != nil || !paid {
		t.Fatalf("settle: paid=%v err=%v", paid, err)
	}

	type result struct {
		outcome DunningPauseOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		_, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x)
		done <- result{outcome, err}
	}()
	select {
	case res := <-done:
		t.Fatalf("SetDunningPause returned %v, %v while the payment's transaction held the invoice; want it to wait", res.outcome, res.err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit payment: %v", err)
	}
	res := <-done
	if res.err != nil || res.outcome != DunningPauseInvoiceSettled {
		t.Fatalf("SetDunningPause after the payment = %v, %v; want DunningPauseInvoiceSettled", res.outcome, res.err)
	}
	if pc := r.pause(); pc != nil {
		t.Errorf("pause = %+v on a subscription whose debt was paid, want none", pc)
	}
}

// T5 (C3), the interleaving that loses the release: two payments (here two
// credit notes, whose transaction the test can hold open) on one
// dunning-paused subscription, each the last thing owed but the other's,
// with both transactions open at once. The first counts the second's
// invoice as still owed (its payment has not committed). The release locks
// the subscription before it counts, so the second payment waits for the
// first to commit, then counts nothing owed and releases. Mutation: drop
// the subscription FOR NO KEY UPDATE in ReleaseDunningPauseTx and the second
// counts the first's invoice still owed: the pause sticks on a subscription
// that owes nothing.
func TestReleaseDunningPause_TwoPaymentsAtOnce(t *testing.T) {
	r := newDunningPauseRig(t, "A3 Release Race")
	x1 := r.invoice("INV-A3-X1", 1000)
	x2 := r.invoice("INV-A3-X2", 1000)
	if _, _, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x1); err != nil {
		t.Fatalf("dunning pause: %v", err)
	}
	settle := func(invID string) (*sql.Tx, error) {
		tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
		if err != nil {
			return nil, err
		}
		var one int
		if err := tx.QueryRowContext(r.ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR NO KEY UPDATE`, invID).Scan(&one); err != nil {
			postgres.Rollback(tx)
			return nil, err
		}
		// A credit note's transaction: the note takes what is due to zero,
		// and the invoice is marked paid. Until it commits, the other
		// payment still sees this invoice owed.
		if _, err := tx.ExecContext(r.ctx, `UPDATE invoices SET amount_due_cents = 0 WHERE id = $1`, invID); err != nil {
			postgres.Rollback(tx)
			return nil, err
		}
		if _, paid, err := r.invs.SettleZeroDueTx(r.ctx, tx, r.tenantID, invID, time.Now().UTC()); err != nil || !paid {
			postgres.Rollback(tx)
			return nil, fmt.Errorf("settle %s: paid=%v err=%v", invID, paid, err)
		}
		return tx, nil
	}

	tx1, err := settle(x1) // X2 still owed: no release; with the lock, holds the subscription
	if err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() {
		tx2, err := settle(x2) // with the lock, waits here for tx1
		if err != nil {
			second <- err
			return
		}
		second <- tx2.Commit()
	}()
	time.Sleep(300 * time.Millisecond) // the second payment reaches its release
	if err := tx1.Commit(); err != nil {
		t.Fatalf("commit X1: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("X2: %v", err)
	}
	if pc := r.pause(); pc == nil || pc.ResumesAt == nil {
		t.Fatalf("pause = %+v after both invoices were paid, want resumes_at set", pc)
	}
}

// ReleaseDunningPauseTx leaves an operator's pause and a canceled
// subscription alone, and counts a written-off invoice as owed.
func TestReleaseDunningPause_Rules(t *testing.T) {
	t.Run("operator pause", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Release Operator")
		x := r.invoice("INV-A3-L1", 1000)
		if _, err := r.subs.SetPauseCollection(r.ctx, r.tenantID, r.subID, domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}); err != nil {
			t.Fatalf("operator pause: %v", err)
		}
		if _, err := r.invs.MarkPaid(r.ctx, r.tenantID, x, "", time.Now().UTC()); err != nil {
			t.Fatalf("mark paid: %v", err)
		}
		if pc := r.pause(); pc == nil || pc.ResumesAt != nil || pc.SetBy != domain.PauseSetByOperator {
			t.Errorf("pause = %+v, want the operator's, untouched", pc)
		}
	})

	t.Run("written-off invoice still owed", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Release Written Off")
		x1 := r.invoice("INV-A3-L2", 1000)
		x2 := r.invoice("INV-A3-L3", 1000)
		if _, _, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x1); err != nil {
			t.Fatalf("dunning pause: %v", err)
		}
		if _, err := testutil.AdminPool(t).Exec(`UPDATE invoices SET status = 'uncollectible', uncollectible_at = now() WHERE id = $1`, x2); err != nil {
			t.Fatalf("write off: %v", err)
		}
		if _, err := r.invs.MarkPaid(r.ctx, r.tenantID, x1, "", time.Now().UTC()); err != nil {
			t.Fatalf("mark paid: %v", err)
		}
		if pc := r.pause(); pc == nil || pc.ResumesAt != nil {
			t.Errorf("pause = %+v with a written-off invoice still owed, want it held", pc)
		}
	})

	t.Run("canceled subscription", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Release Canceled")
		x := r.invoice("INV-A3-L4", 1000)
		if _, _, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x); err != nil {
			t.Fatalf("dunning pause: %v", err)
		}
		if _, err := testutil.AdminPool(t).Exec(`UPDATE subscriptions SET status = 'canceled', canceled_at = now() WHERE id = $1`, r.subID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := r.invs.MarkPaid(r.ctx, r.tenantID, x, "", time.Now().UTC()); err != nil {
			t.Fatalf("mark paid: %v", err)
		}
		if pc := r.pause(); pc == nil || pc.ResumesAt != nil {
			t.Errorf("pause = %+v on the canceled subscription, want it untouched", pc)
		}
	})
}

// T10. The paired CHECK makes a writer that sets the pause without saying
// who set it fail.
func TestPauseSetBy_PairedCheck(t *testing.T) {
	r := newDunningPauseRig(t, "A3 Paired Check")
	_, err := testutil.AdminPool(t).Exec(`UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft' WHERE id = $1`, r.subID)
	if err == nil || !strings.Contains(err.Error(), "subscriptions_pause_collection_set_by_pair_check") {
		t.Fatalf("setting the pause with no set_by: %v, want the pair CHECK to refuse it", err)
	}
	_, err = testutil.AdminPool(t).Exec(`UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft', pause_collection_set_by = 'customer' WHERE id = $1`, r.subID)
	if err == nil || !strings.Contains(err.Error(), "subscriptions_pause_collection_set_by_check") {
		t.Fatalf("set_by='customer': %v, want the value CHECK to refuse it", err)
	}
}
