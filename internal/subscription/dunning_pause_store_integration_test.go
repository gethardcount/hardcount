package subscription

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
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
		}, nil); err != nil {
			t.Fatalf("operator pause: %v", err)
		}
		_, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil)
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
		if _, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil); err != nil || outcome != DunningPauseWritten {
			t.Fatalf("first SetDunningPause = %v, %v; want written", outcome, err)
		}
		if _, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil); err != nil || outcome != DunningPauseAlreadyHeld {
			t.Fatalf("second SetDunningPause = %v, %v; want DunningPauseAlreadyHeld", outcome, err)
		}
	})

	t.Run("lapsed operator pause replaced", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Rules Lapsed")
		x := r.invoice("INV-A3-R3", 1000)
		resumes := time.Now().UTC().Add(time.Hour)
		if _, err := r.subs.SetPauseCollection(r.ctx, r.tenantID, r.subID, domain.PauseCollection{
			Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: &resumes,
		}, nil); err != nil {
			t.Fatalf("operator pause: %v", err)
		}
		// The resumes_at passed; the resumer has not run yet.
		if _, err := testutil.AdminPool(t).Exec(`UPDATE subscriptions SET pause_collection_resumes_at = now() - interval '1 minute' WHERE id = $1`, r.subID); err != nil {
			t.Fatalf("lapse the pause: %v", err)
		}
		if _, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil); err != nil || outcome != DunningPauseWritten {
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
		if _, _, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil); !errors.Is(err, errs.ErrInvalidState) {
			t.Fatalf("SetDunningPause on a canceled subscription: %v, want invalid state", err)
		}
	})
}

// T6 (C2). A payment commits while dunning's pause is being written. The
// pause locks the invoice (after the subscription), so it waits for the
// payment's transaction, then finds the invoice paid and pauses nothing.
// Mutation: drop the invoice FOR NO KEY UPDATE in SetDunningPause and the
// pause lands on a paid debt, with nothing left to lift it.
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
		_, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil)
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

// T4 at the store. A dunning pause belongs to the invoice whose dunning set
// it (Sagar's A3 decision, 2026-10-21): paying another invoice leaves it,
// and paying that invoice lifts it even while another invoice is still owed.
func TestReleaseDunningPause_OnlyThePausingInvoice(t *testing.T) {
	r := newDunningPauseRig(t, "A3 Release Pausing Invoice")
	pausing := r.invoice("INV-A3-P1", 1000)
	other := r.invoice("INV-A3-P2", 1000)
	stillOwed := r.invoice("INV-A3-P3", 1000)
	if _, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, pausing, nil, nil); err != nil || outcome != DunningPauseWritten {
		t.Fatalf("dunning pause: %v, %v", outcome, err)
	}

	if _, err := r.invs.MarkPaid(r.ctx, r.tenantID, other, "", time.Now().UTC()); err != nil {
		t.Fatalf("pay the other invoice: %v", err)
	}
	if pc := r.pause(); pc == nil || pc.ResumesAt != nil {
		t.Fatalf("pause = %+v after paying an invoice dunning did not pause for, want it held", pc)
	}

	if _, err := r.invs.MarkPaid(r.ctx, r.tenantID, pausing, "", time.Now().UTC()); err != nil {
		t.Fatalf("pay the pausing invoice: %v", err)
	}
	if pc := r.pause(); pc == nil || pc.ResumesAt == nil {
		t.Fatalf("pause = %+v after paying the invoice it was set for (%s still owed), want resumes_at set", pc, stillOwed)
	}
}

// ReleaseDunningPauseTx leaves an operator's pause and a canceled
// subscription alone.
func TestReleaseDunningPause_Rules(t *testing.T) {
	t.Run("operator pause", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Release Operator")
		x := r.invoice("INV-A3-L1", 1000)
		if _, err := r.subs.SetPauseCollection(r.ctx, r.tenantID, r.subID, domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}, nil); err != nil {
			t.Fatalf("operator pause: %v", err)
		}
		if _, err := r.invs.MarkPaid(r.ctx, r.tenantID, x, "", time.Now().UTC()); err != nil {
			t.Fatalf("mark paid: %v", err)
		}
		if pc := r.pause(); pc == nil || pc.ResumesAt != nil || pc.SetBy != domain.PauseSetByOperator {
			t.Errorf("pause = %+v, want the operator's, untouched", pc)
		}
	})

	t.Run("operator took over a dunning pause", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Release Takeover")
		x := r.invoice("INV-A3-L2", 1000)
		if _, _, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil); err != nil {
			t.Fatalf("dunning pause: %v", err)
		}
		if _, err := r.subs.SetPauseCollection(r.ctx, r.tenantID, r.subID, domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}, nil); err != nil {
			t.Fatalf("operator pause: %v", err)
		}
		if _, err := r.invs.MarkPaid(r.ctx, r.tenantID, x, "", time.Now().UTC()); err != nil {
			t.Fatalf("mark paid: %v", err)
		}
		if pc := r.pause(); pc == nil || pc.ResumesAt != nil || pc.SetBy != domain.PauseSetByOperator {
			t.Errorf("pause = %+v, want the operator's, untouched", pc)
		}
	})

	t.Run("canceled subscription", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Release Canceled")
		x := r.invoice("INV-A3-L4", 1000)
		if _, _, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil); err != nil {
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

// waitForLockWait returns once some transaction on this database waits for
// a row lock: the goroutine under test has reached its blocking lock.
func waitForLockWait(t *testing.T) {
	t.Helper()
	admin := testutil.AdminPool(t)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := admin.QueryRow(`SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND a.datname = current_database()`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no transaction waited for a lock within 5s")
}

// Lock order (finding 2 of the #1340 review). A cancel or a plan swap holds
// the subscription row and then writes the overdue invoice (its relief).
// The two A3 writers that touch both rows, a payment of the pausing invoice
// and dunning's pause, take the subscription first too (the order
// lockSubscriptionRow documents), so each waits behind the cancel instead
// of holding the invoice the cancel needs. Mutation: take the invoice first
// in either writer, and Postgres aborts one side with "deadlock detected".
func TestDunningPause_SubscriptionLockedBeforeInvoice(t *testing.T) {
	// holdSubscriptionThenInvoice stands in for the cancel: it locks the
	// subscription, waits until op blocks, then locks the invoice.
	holdSubscriptionThenInvoice := func(t *testing.T, r *dunningPauseRig, invID string, op func() error) {
		t.Helper()
		tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var one int
		if err := tx.QueryRowContext(r.ctx, `SELECT 1 FROM subscriptions WHERE id = $1 FOR NO KEY UPDATE`, r.subID).Scan(&one); err != nil {
			t.Fatalf("lock subscription: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- op() }()
		waitForLockWait(t)
		if err := tx.QueryRowContext(r.ctx, `SELECT 1 FROM invoices WHERE id = $1 FOR UPDATE`, invID).Scan(&one); err != nil {
			t.Fatalf("the cancel's invoice lock: %v (want it granted: the other writer must wait on the subscription first)", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit the cancel: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("the writer behind the cancel: %v", err)
		}
	}

	t.Run("payment of the pausing invoice", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Lock Order Payment")
		x := r.invoice("INV-A3-LO1", 1000)
		if _, _, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil); err != nil {
			t.Fatalf("dunning pause: %v", err)
		}
		holdSubscriptionThenInvoice(t, r, x, func() error {
			_, err := r.invs.MarkPaid(r.ctx, r.tenantID, x, "", time.Now().UTC())
			return err
		})
		if pc := r.pause(); pc == nil || pc.ResumesAt == nil {
			t.Errorf("pause = %+v after the payment, want resumes_at set", pc)
		}
	})

	t.Run("dunning pause", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Lock Order Pause")
		x := r.invoice("INV-A3-LO2", 1000)
		holdSubscriptionThenInvoice(t, r, x, func() error {
			_, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil)
			if err == nil && outcome != DunningPauseWritten {
				return fmt.Errorf("outcome %v, want written", outcome)
			}
			return err
		})
	})
}

// T10. The CHECK makes a writer that clears the pause but leaves the
// invoice it was set for fail.
func TestPauseInvoice_Check(t *testing.T) {
	r := newDunningPauseRig(t, "A3 Pause Invoice Check")
	_, err := testutil.AdminPool(t).Exec(`UPDATE subscriptions SET pause_collection_invoice_id = 'inv_x' WHERE id = $1`, r.subID)
	if err == nil || !strings.Contains(err.Error(), "subscriptions_pause_collection_invoice_id_check") {
		t.Fatalf("an invoice id with no pause: %v, want the CHECK to refuse it", err)
	}
}

// collectionPausedEvents reads the subscription.collection_paused rows the
// outbox holds for the rig's subscription, oldest first.
func (r *dunningPauseRig) collectionPausedEvents() []map[string]any {
	r.t.Helper()
	rows, err := testutil.AdminPool(r.t).Query(`
		SELECT payload FROM webhook_outbox
		WHERE event_type = $1 AND payload->'subscription'->>'id' = $2
		ORDER BY created_at, id`, domain.EventSubscriptionCollectionPaused, r.subID)
	if err != nil {
		r.t.Fatalf("read outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			r.t.Fatalf("scan outbox row: %v", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			r.t.Fatalf("decode outbox row: %v", err)
		}
		out = append(out, data)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read outbox: %v", err)
	}
	return out
}

// failingOutbox refuses every enqueue.
type failingOutbox struct{}

func (failingOutbox) Enqueue(context.Context, *sql.Tx, string, string, map[string]any) (string, error) {
	return "", errors.New("outbox down")
}

// Dunning's pause queues subscription.collection_paused (triggered_by
// dunning) in its own transaction: once when it pauses, never when a pause
// was already in force or the invoice was paid first, and not at all when
// the pause rolls back.
func TestSetDunningPause_QueuesCollectionPausedInTheTx(t *testing.T) {
	t.Run("written once, with the row as stored", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Paused Event")
		r.subs.SetOutboxEnqueuer(webhook.NewOutboxStore(r.db))
		x := r.invoice("INV-A3-EV1", 1000)
		if _, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil); err != nil || outcome != DunningPauseWritten {
			t.Fatalf("SetDunningPause = %v, %v; want written", outcome, err)
		}
		// A re-attempted exhaustion finds the pause in force: no second event.
		if _, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil); err != nil || outcome != DunningPauseAlreadyHeld {
			t.Fatalf("second SetDunningPause = %v, %v; want DunningPauseAlreadyHeld", outcome, err)
		}
		evs := r.collectionPausedEvents()
		if len(evs) != 1 {
			t.Fatalf("%d subscription.collection_paused events queued, want 1", len(evs))
		}
		if err := domain.CheckWebhookData(domain.EventSubscriptionCollectionPaused, evs[0]); err != nil {
			t.Error(err)
		}
		if evs[0]["triggered_by"] != "dunning" {
			t.Errorf("triggered_by = %v, want dunning", evs[0]["triggered_by"])
		}
		stored, err := r.subs.Get(r.ctx, r.tenantID, r.subID)
		if err != nil {
			t.Fatalf("get subscription: %v", err)
		}
		want := domain.SubscriptionWebhookData(stored, nil)[domain.WebhookResourceSubscription]
		if got := evs[0][domain.WebhookResourceSubscription]; !reflect.DeepEqual(normalizeJSON(t, got), normalizeJSON(t, want)) {
			t.Errorf("event subscription = %v\nwant the stored row %v", got, want)
		}
	})

	t.Run("operator pause in force: no event", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Paused Event Held")
		r.subs.SetOutboxEnqueuer(webhook.NewOutboxStore(r.db))
		x := r.invoice("INV-A3-EV2", 1000)
		if _, err := r.subs.SetPauseCollection(r.ctx, r.tenantID, r.subID, domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft}, nil); err != nil {
			t.Fatalf("operator pause: %v", err)
		}
		if _, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil); err != nil || outcome != DunningPauseAlreadyHeld {
			t.Fatalf("SetDunningPause = %v, %v; want DunningPauseAlreadyHeld", outcome, err)
		}
		if evs := r.collectionPausedEvents(); len(evs) != 0 {
			t.Errorf("%d subscription.collection_paused events queued, want none", len(evs))
		}
	})

	t.Run("invoice paid first: no event", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Paused Event Settled")
		r.subs.SetOutboxEnqueuer(webhook.NewOutboxStore(r.db))
		x := r.invoice("INV-A3-EV3", 1000)
		if _, err := r.invs.MarkPaid(r.ctx, r.tenantID, x, "", time.Now().UTC()); err != nil {
			t.Fatalf("pay: %v", err)
		}
		if _, outcome, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil); err != nil || outcome != DunningPauseInvoiceSettled {
			t.Fatalf("SetDunningPause = %v, %v; want DunningPauseInvoiceSettled", outcome, err)
		}
		if evs := r.collectionPausedEvents(); len(evs) != 0 {
			t.Errorf("%d subscription.collection_paused events queued, want none", len(evs))
		}
	})

	t.Run("enqueue fails: the pause rolls back", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Paused Event Rollback")
		r.subs.SetOutboxEnqueuer(failingOutbox{})
		x := r.invoice("INV-A3-EV4", 1000)
		if _, _, err := r.subs.SetDunningPause(r.ctx, r.tenantID, r.subID, x, nil, nil); err == nil {
			t.Fatal("SetDunningPause with the outbox down succeeded, want the error")
		}
		if pc := r.pause(); pc != nil {
			t.Errorf("pause = %+v after a failed enqueue, want none (one transaction)", pc)
		}
	})
}

// normalizeJSON round-trips v through JSON so typed and decoded values compare.
func normalizeJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// failingAudit refuses every audit write.
type failingAudit struct{}

func (failingAudit) Log(context.Context, string, string, string, string, string, map[string]any) error {
	return errors.New("audit down")
}

func (failingAudit) LogInTx(context.Context, *sql.Tx, audit.Entry) error {
	return errors.New("audit down")
}

// dunningPausedRows reads the "Collection paused" audit rows dunning wrote
// for the rig's subscription.
func (r *dunningPauseRig) dunningPausedRows(logger *audit.Logger) []domain.AuditEntry {
	r.t.Helper()
	rows, _, err := logger.Query(r.ctx, r.tenantID, audit.QueryFilter{ResourceType: "subscription", ResourceID: r.subID})
	if err != nil {
		r.t.Fatalf("query audit: %v", err)
	}
	var out []domain.AuditEntry
	for _, row := range rows {
		if row.Metadata["action"] == domain.SubAuditCollectionPaused && row.Metadata["triggered_by"] == "dunning" {
			out = append(out, row)
		}
	}
	return out
}

// Dunning's "Collection paused" audit row is written in the pause's
// transaction, beside its subscription.collection_paused event: the pause,
// the row and the event commit together or not at all. Written after the
// commit, a failed or lost row was never written again, since a re-attempt
// finds the pause in force and returns early.
func TestPauseCollectionForDunning_AuditRowInThePauseTx(t *testing.T) {
	t.Run("one row with the pause, none on a re-attempt", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Pause Audit")
		r.subs.SetOutboxEnqueuer(webhook.NewOutboxStore(r.db))
		logger := audit.NewLogger(r.db)
		svc := NewService(r.subs, nil)
		svc.SetAuditLogger(logger)
		x := r.invoice("INV-A3-AU1", 1000)
		for i := 0; i < 2; i++ {
			if applied, err := svc.PauseCollectionForDunning(r.ctx, r.tenantID, r.subID, x); err != nil || !applied {
				t.Fatalf("PauseCollectionForDunning #%d = %v, %v; want applied", i+1, applied, err)
			}
		}
		rows := r.dunningPausedRows(logger)
		if len(rows) != 1 {
			t.Fatalf("%d dunning \"Collection paused\" rows, want 1", len(rows))
		}
		if rows[0].Metadata["invoice_id"] != x {
			t.Errorf("invoice_id = %v, want %s", rows[0].Metadata["invoice_id"], x)
		}
		if evs := r.collectionPausedEvents(); len(evs) != 1 {
			t.Errorf("%d subscription.collection_paused events, want 1", len(evs))
		}
	})

	t.Run("audit write fails: nothing commits", func(t *testing.T) {
		r := newDunningPauseRig(t, "A3 Pause Audit Fail")
		r.subs.SetOutboxEnqueuer(webhook.NewOutboxStore(r.db))
		svc := NewService(r.subs, nil)
		svc.SetAuditLogger(failingAudit{})
		x := r.invoice("INV-A3-AU2", 1000)
		if applied, err := svc.PauseCollectionForDunning(r.ctx, r.tenantID, r.subID, x); err == nil {
			t.Fatalf("PauseCollectionForDunning with the audit log down = applied %v, nil; want the error", applied)
		}
		if pc := r.pause(); pc != nil {
			t.Errorf("pause = %+v after a failed audit write, want none (one transaction)", pc)
		}
		if evs := r.collectionPausedEvents(); len(evs) != 0 {
			t.Errorf("%d subscription.collection_paused events after a failed audit write, want none", len(evs))
		}
	})
}
