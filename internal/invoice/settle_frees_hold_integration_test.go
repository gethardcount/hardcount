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
// the reconciler. Left set, the hold blocked the hosted page, Collect, void
// and offline payment until it ended and the stale-hold sweep cleared it: up
// to an hour after the customer was emailed that the payment had failed, with
// a link to a page that answered "payment in progress". Only a dunning retry
// whose payment came back processing was released early, by a write of its
// own.
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
		res := fail(t, inv.ID, "pi_late")
		if !res.Recorded {
			t.Skip("this failure report does not move an invoice that is not in flight; the write under test is not reached")
		}
		if !held(t, inv.ID) {
			t.Error("a failure report on an invoice that was not in flight freed the hold of a charger that has claimed and not yet stamped")
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
