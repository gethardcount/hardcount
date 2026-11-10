package invoice_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The write that ends an attempt frees that attempt's charge hold.
//
// A charge whose confirm got no answer keeps its hold and leaves the invoice
// in flight on its PaymentIntent. The answer then arrives by webhook or from
// the reconciler. The hold must go with it: while it is set, the hosted page
// answers "payment in progress", and Collect, void and offline payment are
// refused, on an invoice whose customer was just emailed that the payment
// failed. A hold that is not the ended attempt's must stay.
func TestSettle_FreesTheChargeHoldOfTheAttemptItEnds(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Settle Frees Hold")
	store := invoice.NewPostgresStore(db)

	// setHold moves the hold's end, keeping its token equal to it as a claim
	// writes it. tokened=false leaves the token behind: a hold an earlier
	// version took.
	setHold := func(t *testing.T, id, interval string, tokened bool) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, `
			UPDATE invoices SET auto_charge_claimed_until = now() + $2::interval,
			       charge_hold_token = CASE WHEN $4 THEN now() + $2::interval ELSE NULL END
			 WHERE id = $1 AND tenant_id = $3`, id, interval, tenantID, tokened); err != nil {
			t.Fatalf("set hold: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit hold: %v", err)
		}
	}
	held := func(t *testing.T, id string) bool {
		t.Helper()
		state, err := store.ChargeHoldStateOf(ctx, tenantID, id)
		if err != nil {
			t.Fatalf("read hold: %v", err)
		}
		return state != domain.ChargeHoldNone
	}
	// inFlight seeds an invoice a charger claimed and left in flight on
	// pi_slow with the given payment status, its hold ending at holdEnds.
	inFlight := func(t *testing.T, num string, status domain.InvoicePaymentStatus, holdEnds string) domain.Invoice {
		t.Helper()
		inv := seedClaimableInvoice(t, db, ctx, tenantID, num)
		if hold, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID); err != nil || !hold.Held() {
			t.Fatalf("claim: held=%v err=%v", hold.Held(), err)
		}
		if _, applied, err := store.StampChargeOutcome(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq, domain.PaymentProcessing, "pi_slow", ""); err != nil || !applied {
			t.Fatalf("stamp processing: applied=%v err=%v", applied, err)
		}
		if status == domain.PaymentUnknown {
			tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer postgres.Rollback(tx)
			if _, err := tx.ExecContext(ctx, `UPDATE invoices SET payment_status = 'unknown' WHERE id = $1`, inv.ID); err != nil {
				t.Fatalf("set unknown: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}
		setHold(t, inv.ID, holdEnds, true)
		return inv
	}
	fail := func(t *testing.T, id, pi string) domain.PaymentFailureResult {
		t.Helper()
		_, res, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, id,
			domain.PaymentFailureReport{PaymentIntentID: pi, Message: "payment failed", DeclineCode: "insufficient_funds"}, nil)
		if err != nil {
			t.Fatalf("failure settle: %v", err)
		}
		return res
	}

	for _, tc := range []struct {
		name     string
		status   domain.InvoicePaymentStatus
		holdEnds string
	}{
		{"a decline for an unknown charge, hold still live", domain.PaymentUnknown, "5 minutes"},
		{"a failure for a processing payment, hold still live", domain.PaymentProcessing, "5 minutes"},
		{"a failure that arrives after the hold ended", domain.PaymentProcessing, "-11 minutes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := inFlight(t, "INV-SFH-"+string(tc.status)+tc.holdEnds, tc.status, tc.holdEnds)
			if res := fail(t, inv.ID, "pi_slow"); !res.Recorded {
				t.Fatal("setup: the failure must be recorded on the invoice")
			}
			if held(t, inv.ID) {
				t.Error("the charge hold is still set after the attempt it belonged to failed")
			}
		})
	}

	t.Run("a payment that succeeds frees the hold", func(t *testing.T) {
		inv := inFlight(t, "INV-SFH-paid", domain.PaymentUnknown, "5 minutes")
		if _, err := store.MarkPaid(ctx, tenantID, inv.ID, "pi_slow", time.Now().UTC()); err != nil {
			t.Fatalf("mark paid: %v", err)
		}
		if held(t, inv.ID) {
			t.Error("a paid invoice still carries a charge hold")
		}
	})

	// The safety half. The invoice already failed on pi_slow, and a new
	// charger has claimed it and not yet put its PaymentIntent on it. A late
	// or repeated failure report for pi_slow must not free that charger's
	// hold: with the hold gone, a second charger could claim beside it.
	t.Run("a repeated failure report leaves a new charger's hold alone", func(t *testing.T) {
		inv := inFlight(t, "INV-SFH-repeat", domain.PaymentProcessing, "5 minutes")
		fail(t, inv.ID, "pi_slow")
		if held(t, inv.ID) {
			t.Fatal("setup: the first failure frees the first attempt's hold")
		}
		if hold, err := store.ClaimChargeForDunningRetry(ctx, tenantID, inv.ID); err != nil || !hold.Held() {
			t.Fatalf("the next charger's claim: held=%v err=%v", hold.Held(), err)
		}
		fail(t, inv.ID, "pi_slow") // Stripe redelivers the webhook
		if !held(t, inv.ID) {
			t.Error("a repeated failure report freed the hold of a charger that has claimed and not yet stamped")
		}
	})

	// The case that reaches the changed write with the hold kept. The invoice
	// is failed with no PaymentIntent of its own in flight, and a charger has
	// claimed it and not yet stamped. A failure report that moves the invoice
	// (a first failure for a PaymentIntent it does not hold) must record the
	// failure and leave that charger's hold.
	t.Run("a failure that moves an invoice not in flight leaves a claimed hold", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-SFH-claimed")
		if hold, err := store.ClaimChargeForDunningRetry(ctx, tenantID, inv.ID); err != nil || !hold.Held() {
			t.Fatalf("claim: held=%v err=%v", hold.Held(), err)
		}
		if res := fail(t, inv.ID, "pi_late"); !res.Recorded {
			t.Fatal("setup: the failure must be recorded on the invoice, or the write under test is not reached")
		}
		if !held(t, inv.ID) {
			t.Error("a failure report on an invoice that was not in flight freed the hold of a charger that has claimed and not yet stamped")
		}
	})

	// The invoice can name the reported PaymentIntent without being in flight
	// on it: the unknown sweep ended that attempt with no payment
	// (EndAttemptWithoutPayment leaves the PaymentIntent on the pending
	// invoice), and a new charger has claimed. A failure report for the old
	// PaymentIntent that arrives now must leave the new charger's hold. Only
	// the payment-status half of the rule tells the two apart.
	t.Run("a late failure for an attempt already ended leaves a new charger's hold", func(t *testing.T) {
		inv := inFlight(t, "INV-SFH-ended", domain.PaymentUnknown, "5 minutes")
		if status, ended, err := store.EndAttemptWithoutPayment(ctx, tenantID, inv.ID, "pi_slow", domain.AttemptReasonNeverConfirmed); err != nil || !ended || status != domain.PaymentPending {
			t.Fatalf("setup: end the attempt: status %q ended %v err %v, want pending, ended", status, ended, err)
		}
		if got, err := store.Get(ctx, tenantID, inv.ID); err != nil || got.StripePaymentIntentID != "pi_slow" || held(t, inv.ID) {
			t.Fatalf("setup: after the end the invoice names %q (err %v) and held=%v, want pi_slow and no hold", got.StripePaymentIntentID, err, held(t, inv.ID))
		}
		if hold, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID); err != nil || !hold.Held() {
			t.Fatalf("the next charger's claim: held=%v err=%v", hold.Held(), err)
		}
		if res := fail(t, inv.ID, "pi_slow"); !res.Recorded {
			t.Fatal("setup: the late failure must be recorded on the invoice, or the write under test is not reached")
		}
		if !held(t, inv.ID) {
			t.Error("a late failure for a PaymentIntent the invoice was no longer in flight on freed the hold of a charger that has claimed and not yet stamped")
		}
	})

	// A hold without its token is an earlier version's. The stale-hold sweep
	// keeps it for a person on a finalized invoice, and the settle must not
	// undo that.
	t.Run("an earlier version's hold is left for a person", func(t *testing.T) {
		inv := inFlight(t, "INV-SFH-untokened", domain.PaymentProcessing, "5 minutes")
		setHold(t, inv.ID, "5 minutes", false)
		if res := fail(t, inv.ID, "pi_slow"); !res.Recorded {
			t.Fatal("setup: the failure must be recorded on the invoice")
		}
		if !held(t, inv.ID) {
			t.Error("the settle freed a hold that carries no token")
		}
	})

	// A failure report for a PaymentIntent the invoice is not in flight on
	// (an older attempt's) leaves the current attempt and its hold alone.
	t.Run("a failure for another PaymentIntent leaves the hold", func(t *testing.T) {
		inv := inFlight(t, "INV-SFH-other", domain.PaymentProcessing, "5 minutes")
		fail(t, inv.ID, "pi_other")
		if !held(t, inv.ID) {
			t.Error("a failure for another PaymentIntent freed the in-flight attempt's hold")
		}
	})
}

// The window this change opens: charger A is still inside its confirm when
// A's failure is settled by its webhook, which frees A's hold. Charger B then
// claims and stamps its own PaymentIntent. When A's confirm returns, A runs
// the tail of its charge: it reports its decline, or stamps "processing", and
// releases its hold. Each of those writes is guarded by something A read (the
// counter its stamp returned, the hold value its claim wrote), so none of
// them may touch B's attempt: the invoice stays unknown on B's PaymentIntent,
// under B's hold, with the counter where B's stamp left it.
func TestSettle_FirstChargersTailLeavesTheNextAttemptAlone(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Settle Then Second Charger")
	store := invoice.NewPostgresStore(db)
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-SFH-interleave")

	stamp := func(hold domain.ChargeHold, seq int64, pi string) domain.Invoice {
		t.Helper()
		stamped, ok, err := store.StampPendingConfirm(ctx, tenantID, domain.InvoiceChargeAttempt{
			InvoiceID: inv.ID, StripePaymentIntentID: pi, Trigger: domain.ChargeTriggerAutoCharge,
			Outcome: domain.ChargeAttemptPending, AmountCents: inv.AmountDueCents, OccurredAt: time.Now().UTC(),
		}, hold, seq)
		if err != nil || !ok {
			t.Fatalf("stamp %s: ok=%v err=%v", pi, ok, err)
		}
		return stamped
	}
	holdOf := func() *time.Time {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var h *time.Time
		if err := tx.QueryRowContext(ctx, `SELECT auto_charge_claimed_until FROM invoices WHERE id = $1`, inv.ID).Scan(&h); err != nil {
			t.Fatalf("read hold: %v", err)
		}
		return h
	}

	// Charger A claims, stamps pi_A and is now inside its confirm.
	h1, err := store.ClaimAutoCharge(ctx, tenantID, inv.ID)
	if err != nil || !h1.Held() {
		t.Fatalf("A's claim: held=%v err=%v", h1.Held(), err)
	}
	a := stamp(h1, inv.ChargeAttemptSeq, "pi_A")

	// pi_A's failure arrives by webhook and is settled: A's hold is freed.
	if _, res, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, inv.ID,
		domain.PaymentFailureReport{PaymentIntentID: "pi_A", Message: "card declined", DeclineCode: "insufficient_funds"}, nil); err != nil || !res.Recorded {
		t.Fatalf("webhook settle of pi_A: recorded=%v err=%v", res.Recorded, err)
	}
	if holdOf() != nil {
		t.Fatal("setup: the settle of pi_A must free A's hold")
	}

	// Charger B claims the failed invoice and stamps pi_B.
	h2, err := store.ClaimChargeForDunningRetry(ctx, tenantID, inv.ID)
	if err != nil || !h2.Held() {
		t.Fatalf("B's claim: held=%v err=%v", h2.Held(), err)
	}
	cur, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("read before B's stamp: %v", err)
	}
	b := stamp(h2, cur.ChargeAttemptSeq, "pi_B")

	// A's confirm returns. Its tail, in each form it can take:
	seqA := a.ChargeAttemptSeq
	if _, res, err := store.MarkPaymentFailedReportingTransition(ctx, tenantID, inv.ID,
		domain.PaymentFailureReport{PaymentIntentID: "pi_A", Message: "card declined", DeclineCode: "insufficient_funds", ChargedAtSeq: &seqA}, nil); err != nil || res.Recorded {
		t.Fatalf("A's own decline report: recorded=%v err=%v, want it not recorded as the invoice's current attempt", res.Recorded, err)
	}
	if _, applied, err := store.StampChargeOutcome(ctx, tenantID, inv.ID, seqA, domain.PaymentProcessing, "pi_A", ""); err != nil || applied {
		t.Fatalf("A's processing stamp: applied=%v err=%v, want it refused", applied, err)
	}
	if err := store.ReleaseChargeHold(ctx, tenantID, inv.ID, h1); err != nil {
		t.Fatalf("A's release: %v", err)
	}

	got, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("read after A's tail: %v", err)
	}
	if got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != "pi_B" || got.ChargeAttemptSeq != b.ChargeAttemptSeq {
		t.Fatalf("after A's tail: %s on %q seq %d, want unknown on pi_B seq %d: A's late writes touched B's attempt",
			got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq, b.ChargeAttemptSeq)
	}
	if h := holdOf(); h == nil || !h.Equal(h2.Until) {
		t.Fatalf("after A's tail: hold %v, want B's %v", h, h2.Until)
	}
}
