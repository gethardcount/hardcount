package payment

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// Parked invoices on a real database (REC-5, ADR-127 amendment 2026-10-20).
// A charge is parked when Stripe's answer is unclear and names no
// PaymentIntent: a 500, a 409, a network error, or the caller's deadline
// ending a call Stripe had not answered. The stale-hold sweep's List read
// finishes it. A payment Stripe shows is recorded or followed at the first
// check. When Stripe shows nothing, the invoice is charged again only 24 hours
// after the attempt, because Stripe may still create the payment: "During
// incident recovery, Stripe attempts to reconcile failed operations and send
// webhook events for any objects created during reconciliation", and "Limit
// retries of an unresolved operation to the first 24 hours after the original
// request" (docs.stripe.com/error-low-level). Only Stripe is faked
// (fakeStripe); every test counts the PaymentIntents there.

// pastParkedWait is past Stripe's 24 hours, counted from the attempt's hold.
const pastParkedWait = 25 * time.Hour

// parkCause is one way a charge ends with an unclear answer that names no
// PaymentIntent.
type parkCause struct {
	name     string
	mode     fakeModeKind
	deadline bool // the caller's deadline ends the call
}

var (
	// Stripe answered 500 and saved it under the key; it may still create
	// the payment during incident recovery.
	park500 = parkCause{name: "Stripe answered 500", mode: fakeSave500}
	// The caller's deadline (dunning 15 s, auto-charge 30 s) ended a call
	// Stripe had not answered: Stripe was slow, which is what an incident
	// looks like.
	parkDeadline = parkCause{name: "the caller's deadline ended the call", mode: fakeHang, deadline: true}
)

// park charges inv as kind and gets cause's unclear answer. It requires the
// park on disk: unknown, no PaymentIntent, the counter moved once, the hold
// kept, nothing at Stripe.
func (r *shRig) park(inv domain.Invoice, kind chargeKind, cause parkCause) domain.Invoice {
	r.t.Helper()
	r.fake.queue(fakeMode{kind: cause.mode})
	ctx := r.ctx
	if cause.deadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(r.ctx, 300*time.Millisecond)
		defer cancel()
	}
	before := r.fake.countPIs()
	_, err := r.chargeAs(ctx, kind, inv.ID, "pm_card_1")
	var pe *PaymentError
	if !errors.As(err, &pe) || !pe.Unknown {
		r.t.Fatalf("%s: err = %v, want an unclear outcome", cause.name, err)
	}
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != "" || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 {
		r.t.Fatalf("%s, %s: %s with PI %q seq %d, want parked: unknown, no PaymentIntent, seq %d",
			kind, cause.name, got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq, inv.ChargeAttemptSeq+1)
	}
	if r.hold(inv.ID) == nil {
		r.t.Fatalf("%s: the park released its charge hold", cause.name)
	}
	if n := r.attempts(inv.ID, domain.ChargeAttemptUnknown); n != 1 {
		r.t.Fatalf("%s: %d attempt rows with an unknown outcome, want 1 (the parked attempt)", cause.name, n)
	}
	if after := r.fake.countPIs(); after != before {
		r.t.Fatalf("setup: Stripe should have created nothing, has %d PaymentIntents (was %d)", after, before)
	}
	return got
}

// attempts counts the invoice's attempt rows with the given outcome.
func (r *shRig) attempts(invoiceID string, outcome domain.ChargeAttemptOutcome) int {
	r.t.Helper()
	return r.count(`SELECT count(*) FROM invoice_charge_attempts WHERE invoice_id = $1 AND outcome = $2`, invoiceID, string(outcome))
}

// assertStillParked requires the park unchanged after a check that found
// nothing inside Stripe's 24 hours: still parked, the hold kept, and the
// check stamped so the row rotates instead of heading the queue every tick.
func (r *shRig) assertStillParked(parked domain.Invoice, when string) {
	r.t.Helper()
	got := r.get(parked.ID)
	if got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != "" || got.ChargeAttemptSeq != parked.ChargeAttemptSeq {
		r.t.Fatalf("%s: %s with PI %q seq %d, want still parked at seq %d: Stripe may still create the payment",
			when, got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq, parked.ChargeAttemptSeq)
	}
	if r.hold(parked.ID) == nil {
		r.t.Fatalf("%s: the hold was cleared: nothing would look at Stripe for this invoice again", when)
	}
	if got.ProviderSyncedAt == nil {
		r.t.Errorf("%s: the check was not stamped, so the parked row heads the queue on every tick", when)
	}
}

// 1. Stripe shows nothing for the parked charge. For 24 hours the invoice
// stays parked and nothing charges it (Stripe may still create the payment).
// After that it goes back to pending with a fresh key, the parked attempt is
// recorded as failed, and the next auto-charge takes one payment.
func TestParked_StripeShowsNothing_WaitsADayThenChargedOnce(t *testing.T) {
	for _, cause := range []parkCause{park500, parkDeadline} {
		t.Run(cause.name, func(t *testing.T) {
			r := newSHRig(t, "Parked Nothing")
			inv := r.seed("INV-PK-NONE")
			parked := r.park(inv, autoCharge, cause)

			for _, age := range []time.Duration{staleSweepAge, 2 * time.Hour, 23 * time.Hour} {
				r.ageHold(inv.ID, age)
				r.runClean()
				r.assertStillParked(parked, "a check "+age.String()+" after the hold")
				r.assertNoSecondCharge(inv.ID)
			}

			r.ageHold(inv.ID, pastParkedWait)
			r.runClean()
			got := r.get(inv.ID)
			if got.Status != domain.InvoiceFinalized || got.PaymentStatus != domain.PaymentPending || got.ChargeAttemptSeq != parked.ChargeAttemptSeq+1 {
				t.Fatalf("after 24 hours: %s/%s seq %d, want finalized/pending seq %d (a fresh key for the next charge)",
					got.Status, got.PaymentStatus, got.ChargeAttemptSeq, parked.ChargeAttemptSeq+1)
			}
			if got.LastPaymentError != "" {
				t.Errorf("last_payment_error = %q, want empty: the banner would print Stripe's 500 as the bank's words", got.LastPaymentError)
			}
			if r.hold(inv.ID) != nil {
				t.Error("hold kept after the park was finished")
			}
			if n := r.attempts(inv.ID, domain.ChargeAttemptUnknown); n != 0 {
				t.Errorf("%d attempt rows still unknown, want 0: Velox now knows the attempt took nothing", n)
			}
			if n := r.attempts(inv.ID, domain.ChargeAttemptFailed); n != 1 {
				t.Errorf("%d failed attempt rows, want 1 (the parked attempt)", n)
			}

			if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
				t.Fatalf("the charge after the park: %v", err)
			}
			pi := r.fake.onlyPI()
			if want := ChargeIdempotencyKey(inv.ID, parked.ChargeAttemptSeq+1, ""); pi.key != want {
				t.Errorf("the new charge's key = %q, want %q (never the parked attempt's)", pi.key, want)
			}
			r.assertPaidOnce(inv.ID, pi.id)
		})
	}
}

// 2. Where a parked invoice goes after 24 hours with nothing at Stripe: the
// park overwrote the payment status, and its attempt row says who made it. A
// dunning retry's park goes back to failed, so the run retries it at once
// (the parked attempt was never counted); every other park goes back to
// pending.
func TestParked_GoesBackToWhereAChargePicksItUp(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failedFirst bool // the invoice was failed (a decline) when the parked charge was claimed
		kind        chargeKind
		noAttempt   bool // the attempt row's best-effort write was lost
		want        domain.InvoicePaymentStatus
	}{
		{name: "auto-charge", kind: autoCharge, want: domain.PaymentPending},
		{name: "dunning retry after a decline", failedFirst: true, kind: dunningRetry, want: domain.PaymentFailed},
		{name: "dunning retry on a run whose card was just added", kind: dunningRetry, want: domain.PaymentFailed},
		{name: "Collect on a pending invoice", kind: collect, want: domain.PaymentPending},
		{name: "Collect on a failed invoice", failedFirst: true, kind: collect, want: domain.PaymentPending},
		{name: "no attempt row", failedFirst: true, kind: dunningRetry, noAttempt: true, want: domain.PaymentPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSHRig(t, "Parked Target")
			inv := r.seed("INV-PK-TARGET")
			if tc.failedFirst {
				r.exec(`UPDATE invoices SET payment_status = 'failed', last_payment_error = 'Your card was declined.' WHERE id = $1`, inv.ID)
			}
			parked := r.park(r.get(inv.ID), tc.kind, park500)
			if tc.noAttempt {
				r.exec(`DELETE FROM invoice_charge_attempts WHERE invoice_id = $1`, inv.ID)
			}

			r.ageHold(inv.ID, pastParkedWait)
			r.runClean()
			got := r.get(inv.ID)
			if got.PaymentStatus != tc.want || got.ChargeAttemptSeq != parked.ChargeAttemptSeq+1 || r.hold(inv.ID) != nil {
				t.Fatalf("after 24 hours: %s seq %d hold %v, want %s seq %d and no hold",
					got.PaymentStatus, got.ChargeAttemptSeq, r.hold(inv.ID), tc.want, parked.ChargeAttemptSeq+1)
			}
			if got.LastPaymentError != "" {
				t.Errorf("last_payment_error = %q, want empty", got.LastPaymentError)
			}

			// The charger that owns it next takes one payment.
			next := autoCharge
			if tc.want == domain.PaymentFailed {
				next = dunningRetry
			}
			if _, err := r.chargeAs(r.ctx, next, inv.ID, "pm_card_1"); err != nil {
				t.Fatalf("%s after the park: %v", next, err)
			}
			r.assertPaidOnce(inv.ID, r.fake.onlyPI().id)
		})
	}
}

// 3. Stripe creates the payment the 500 answered for, hours later, during its
// incident recovery, and the webhook is lost. The invoice was still parked,
// so nothing had charged again; the next check records that payment: paid
// once, one PaymentIntent, one receipt.
func TestParked_StripeCreatesThePaymentLater_RecordedNotChargedAgain(t *testing.T) {
	r := newSHRig(t, "Parked Recovered")
	inv := r.seed("INV-PK-RECOVERED")
	parked := r.park(inv, autoCharge, park500)
	r.ageHold(inv.ID, staleSweepAge)
	r.runClean()
	r.assertStillParked(parked, "the first check")

	recovered := r.fake.addPI(stripeCustomerOf(inv), inv.ID, "", "succeeded", shDue)
	r.assertNoSecondCharge(inv.ID)
	r.ageHold(inv.ID, 6*time.Hour)
	r.runClean()
	r.assertPaidOnce(inv.ID, recovered.id)
}

// 4. A processing payment for the parked invoice is found: the invoice
// follows it (processing, its id), the hold is cleared, and the processing
// sweep records it once it succeeds with its webhook lost.
func TestParked_ProcessingPaymentFound_InvoiceFollowsIt(t *testing.T) {
	r := newSHRig(t, "Parked Processing")
	inv := r.seed("INV-PK-PROC")
	parked := r.park(inv, autoCharge, park500)
	pi := r.fake.addPI(stripeCustomerOf(inv), inv.ID, "", "processing", shDue)
	r.ageHold(inv.ID, staleSweepAge)
	r.runClean()
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentProcessing || got.StripePaymentIntentID != pi.id || got.ChargeAttemptSeq != parked.ChargeAttemptSeq+1 {
		t.Fatalf("after the check: %s with PI %q seq %d, want processing with %q seq %d",
			got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq, pi.id, parked.ChargeAttemptSeq+1)
	}
	if r.hold(inv.ID) != nil {
		t.Error("hold kept although the invoice now follows the processing payment")
	}

	r.fake.setStatus(pi.id, "succeeded")
	r.exec(`UPDATE invoices SET updated_at = now() - interval '2 hours' WHERE id = $1`, inv.ID)
	r.runClean()
	r.assertPaidOnce(inv.ID, pi.id)
}

// 5. A parked invoice written off, and Stripe shows nothing for 24 hours: it
// leaves the park (pending, no hold, out of the parked count) and nothing
// charges it (ADR-113). A write-off moves no money.
func TestParked_WrittenOff_StripeShowsNothing_LeavesThePark(t *testing.T) {
	r := newSHRig(t, "Parked Written Off")
	inv := r.seed("INV-PK-WO")
	parked := r.park(inv, autoCharge, park500)
	if _, err := r.invSvc.MarkUncollectible(r.ctx, r.tenantID, inv.ID); err != nil {
		t.Fatalf("write-off on a parked invoice: %v, want allowed", err)
	}
	r.ageHold(inv.ID, staleSweepAge)
	r.runClean()
	if got := r.get(inv.ID); got.Status != domain.InvoiceUncollectible || got.PaymentStatus != domain.PaymentUnknown || r.hold(inv.ID) == nil {
		t.Fatalf("the first check: %s/%s hold %v, want written off, still parked, hold kept (Stripe may still create the payment)",
			got.Status, got.PaymentStatus, r.hold(inv.ID))
	}

	r.ageHold(inv.ID, pastParkedWait)
	r.runClean()
	got := r.get(inv.ID)
	if got.Status != domain.InvoiceUncollectible || got.PaymentStatus != domain.PaymentPending || got.ChargeAttemptSeq != parked.ChargeAttemptSeq+1 || r.hold(inv.ID) != nil {
		t.Fatalf("after 24 hours: %s/%s seq %d hold %v, want uncollectible/pending seq %d and no hold",
			got.Status, got.PaymentStatus, got.ChargeAttemptSeq, r.hold(inv.ID), parked.ChargeAttemptSeq+1)
	}
	if n := r.count(`SELECT count(*) FROM invoices WHERE id = $1 AND payment_status = 'unknown' AND COALESCE(stripe_payment_intent_id, '') = ''`, inv.ID); n != 0 {
		t.Error("still counted as parked")
	}
	for _, kind := range []chargeKind{autoCharge, dunningRetry, collect} {
		if _, err := r.chargeAs(r.ctx, kind, inv.ID, "pm_card_1"); !errors.Is(err, errNotClaimed) {
			t.Errorf("%s on the written-off invoice: err = %v, want its claim refused", kind, err)
		}
	}
	if n := r.fake.countPIs(); n != 0 {
		t.Errorf("PaymentIntents at Stripe = %d, want 0: nothing charges a written-off invoice", n)
	}
}

// 6. Collisions on real Postgres.
func TestParked_Collisions(t *testing.T) {
	// Stripe's recovery creates the payment, and its webhook settles the
	// invoice, between the sweep's List (which saw nothing) and its write. The
	// write is refused; the invoice stays paid, with one payment.
	t.Run("a webhook settles it between the list and the write", func(t *testing.T) {
		r := newSHRig(t, "Parked Race Webhook")
		inv := r.seed("INV-PK-RACE")
		r.park(inv, autoCharge, park500)
		r.ageHold(inv.ID, pastParkedWait)

		var once sync.Once
		var recovered *fakePI
		var webhookErr error
		r.fake.mu.Lock()
		r.fake.afterList = func() {
			once.Do(func() {
				recovered = r.fake.addPI(stripeCustomerOf(inv), inv.ID, "", "succeeded", shDue)
				amt := shDue
				webhookErr = r.stripe.HandleWebhook(r.ctx, r.tenantID, domain.StripeWebhookEvent{
					StripeEventID: "evt_" + recovered.id, EventType: "payment_intent.succeeded", ObjectType: "payment_intent",
					InvoiceID: inv.ID, PaymentIntentID: recovered.id, PaymentStatus: "succeeded",
					AmountCents: &amt, Currency: "usd", Payload: map[string]any{},
					ReceivedAt: time.Now().UTC(), OccurredAt: time.Now().UTC(),
				})
			})
		}
		r.fake.mu.Unlock()
		r.runClean()
		if webhookErr != nil {
			t.Fatalf("webhook: %v", webhookErr)
		}
		if recovered == nil {
			t.Fatal("setup: the sweep never listed the customer's payments")
		}
		got := r.get(inv.ID)
		if got.Status != domain.InvoicePaid || got.PaymentStatus != domain.PaymentSucceeded || got.StripePaymentIntentID != recovered.id {
			t.Fatalf("after the race: %s/%s with PI %q, want paid with %q: the sweep's write went over the webhook's",
				got.Status, got.PaymentStatus, got.StripePaymentIntentID, recovered.id)
		}
		r.fake.mu.Lock()
		r.fake.afterList = nil
		r.fake.mu.Unlock()
		if r.hold(inv.ID) != nil {
			r.ageHold(inv.ID, staleSweepAge) // the next check finds the payment recorded and clears
			r.runClean()
		}
		r.assertPaidOnce(inv.ID, recovered.id)
	})

	// Two sweeps (two leaders during a lease handover) finish one park: the
	// counter moves once.
	t.Run("two sweeps on one parked invoice", func(t *testing.T) {
		r := newSHRig(t, "Parked Race Sweeps")
		inv := r.seed("INV-PK-RACE-SWEEPS")
		parked := r.park(inv, autoCharge, park500)
		r.ageHold(inv.ID, pastParkedWait)
		other := NewReconciler(r.fake, r.invoices, time.Minute)
		other.SetSettler(r.stripe)

		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, rec := range []*Reconciler{r.rec, other} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _ = rec.Run(r.ctx, 50)
			}()
		}
		close(start)
		wg.Wait()
		got := r.get(inv.ID)
		if got.PaymentStatus != domain.PaymentPending || got.ChargeAttemptSeq != parked.ChargeAttemptSeq+1 || r.hold(inv.ID) != nil {
			t.Fatalf("after two sweeps: %s seq %d hold %v, want pending, seq %d (moved once), no hold",
				got.PaymentStatus, got.ChargeAttemptSeq, r.hold(inv.ID), parked.ChargeAttemptSeq+1)
		}
	})
}

// 7. A parked invoice whose Stripe customer is gone from the account: List
// is refused for good, so the invoice stays parked, its check stamped (it
// rotates), and write-off is the exit (ADR-127 R9).
func TestParked_StripeCustomerGone_KeptAndStamped(t *testing.T) {
	r := newSHRig(t, "Parked Customer Gone")
	inv := r.seed("INV-PK-GONE")
	parked := r.park(inv, autoCharge, park500)
	r.fake.setCustomer(stripeCustomerOf(inv), "missing")
	r.ageHold(inv.ID, pastParkedWait)
	_ = r.run()
	r.assertStillParked(parked, "a check Stripe refused")
	if _, err := r.invSvc.MarkUncollectible(r.ctx, r.tenantID, inv.ID); err != nil {
		t.Fatalf("write-off: %v, want allowed (the exit when Stripe cannot be asked)", err)
	}
}

// 8. The upgrade. A release with #1299 and not this change cleared a parked
// invoice's hold at its first check when Stripe showed nothing, and with the
// Search sweep gone nothing would ever look at it again. The migration gives
// each such park a hold at its parked attempt's time (or 2026-01-01 when the
// attempt row was lost), so the sweep's 24 hours count from the attempt.
func TestParked_HoldClearedBeforeTheUpgrade_MigrationGivesItBack(t *testing.T) {
	r := newSHRig(t, "Parked Migration")

	// A: parked 2 hours ago. Its 24 hours are not over.
	a := r.seed("INV-PK-MIG-RECENT")
	parkedA := r.park(a, autoCharge, park500)
	r.exec(`UPDATE invoice_charge_attempts SET created_at = now() - interval '2 hours' WHERE invoice_id = $1`, a.ID)

	// B: parked a month ago; Stripe took the money then and the webhook was
	// lost.
	b := r.seed("INV-PK-MIG-OLD")
	r.park(b, autoCharge, park500)
	monthAgo := time.Now().Add(-30 * 24 * time.Hour)
	r.exec(`UPDATE invoice_charge_attempts SET created_at = $2 WHERE invoice_id = $1`, b.ID, monthAgo)
	paidB := r.fake.addPI(stripeCustomerOf(b), b.ID, "", "succeeded", shDue)
	r.fake.setCreated(paidB.id, monthAgo.Add(time.Minute))

	// C: parked, its attempt row lost.
	c := r.seed("INV-PK-MIG-NOROW")
	parkedC := r.park(c, autoCharge, park500)
	r.exec(`DELETE FROM invoice_charge_attempts WHERE invoice_id = $1`, c.ID)

	for _, id := range []string{a.ID, b.ID, c.ID} {
		r.exec(`UPDATE invoices SET auto_charge_claimed_until = NULL WHERE id = $1`, id)
	}
	// Not parks: an open invoice, and an unknown invoice that names its
	// PaymentIntent (the unknown sweep's).
	open := r.seed("INV-PK-MIG-OPEN")
	named := r.seed("INV-PK-MIG-NAMED")
	namedPI := r.fake.addPI(stripeCustomerOf(named), named.ID, "", "processing", shDue)
	r.exec(`UPDATE invoices SET payment_status = 'unknown', stripe_payment_intent_id = $2 WHERE id = $1`, named.ID, namedPI.id)

	r.applyMigrationUp("hold_parked_invoices")

	for _, id := range []string{open.ID, named.ID} {
		if r.hold(id) != nil {
			t.Errorf("invoice %s got a hold: only a park with none gets one", id)
		}
	}
	if h := r.hold(a.ID); h == nil || time.Since(*h) < 90*time.Minute || time.Since(*h) > 3*time.Hour {
		t.Errorf("A's hold = %v, want its parked attempt's time, about 2 hours ago", h)
	}

	r.runClean()
	r.assertStillParked(parkedA, "A, parked 2 hours ago")
	if got := r.get(b.ID); got.Status != domain.InvoicePaid || got.StripePaymentIntentID != paidB.id {
		t.Errorf("B: %s with PI %q, want paid with %q (the payment from a month ago)", got.Status, got.StripePaymentIntentID, paidB.id)
	}
	if got := r.get(c.ID); got.PaymentStatus != domain.PaymentPending || got.ChargeAttemptSeq != parkedC.ChargeAttemptSeq+1 || r.hold(c.ID) != nil {
		t.Errorf("C: %s seq %d hold %v, want pending seq %d and no hold (no attempt row: dated 2026-01-01, long past 24 hours)",
			got.PaymentStatus, got.ChargeAttemptSeq, r.hold(c.ID), parkedC.ChargeAttemptSeq+1)
	}
}

// applyMigrationUp runs the Up section of the migration file named
// *_<suffix>.sql against the test database, on rows the test seeded after
// the test database was migrated.
func (r *shRig) applyMigrationUp(suffix string) {
	r.t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "platform", "migrate", "sql", "*_"+suffix+".sql"))
	if err != nil {
		r.t.Fatal(err)
	}
	if len(files) != 1 {
		r.t.Fatalf("migration *_%s.sql: found %d files, want 1", suffix, len(files))
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		r.t.Fatal(err)
	}
	up, _, found := strings.Cut(string(body), "-- +goose Down")
	if !found {
		r.t.Fatalf("%s: no Down section", files[0])
	}
	up = strings.Replace(up, "-- +goose Up", "", 1)
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, up); err != nil {
		r.t.Fatalf("%s: %v", files[0], err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatal(err)
	}
}
