package dunning_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"

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
// invoice in flight indefinitely and spun every tick. "Do we hold
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
	_, err = dstore.PutPolicy(ctx, tenantID, domain.DunningPolicy{
		Enabled: true, RetryDays: []int{3, 3, 3},
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
			InvoiceID: inv.ID, CustomerID: cust.ID,
			State: domain.DunningActive, Reason: "payment_failed",
		})
		if err != nil {
			t.Fatalf("create run %s: %v", num, err)
		}
		// CreateWithLineItems does NOT persist stripe_payment_intent_id: that
		// column is only ever written by the payment path. Setting it on the
		// struct silently did nothing, so it is written here.
		if piID != "" {
			tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE invoices SET stripe_payment_intent_id = $2 WHERE id = $1`, inv.ID, piID); err != nil {
				t.Fatalf("attach payment intent to %s: %v", num, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
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

	unknownWithPI := mkDueRun("INV-UNKNOWN-PI", domain.PaymentUnknown, "pi_live_1", domain.InvoiceFinalized)
	processing := mkDueRun("INV-PROCESSING", domain.PaymentProcessing, "pi_live_2", domain.InvoiceFinalized)
	unknownWrittenOff := mkDueRun("INV-UNKNOWN-WROFF", domain.PaymentUnknown, "pi_live_3", domain.InvoiceUncollectible)
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

	if got[unknownWithPI] {
		t.Error("'unknown' must be excluded: the claim requires pending/failed either way, and the reconciler settles only TERMINAL provider outcomes, so a PI at requires_action keeps this in flight forever")
	}
	if got[processing] {
		t.Error("'processing' must be excluded — ClaimChargeForDunningRetry cannot admit it, so selecting the run is wasted work every tick")
	}
	if !got[unknownWrittenOff] {
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
