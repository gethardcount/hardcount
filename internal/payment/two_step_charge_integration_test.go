package payment

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/email"
)

// Two-step charge (ADR-137): each attempt creates its PaymentIntent
// unconfirmed, puts it on the invoice as an unknown outcome, then confirms.
// These run the same rig as the stale-hold tests (real invoice
// store, webhook and email outboxes, the reconciler as router.go wires it).
// two_step_safety_integration_test.go holds the review's break attempts.

func newTwoStepRig(t *testing.T, name string) *shRig {
	t.Helper()
	return newSHRig(t, name)
}

// ageUpdated moves the invoice's last update into the past, past the
// reconciler's cooling window for unknown and processing payments.
// ageUpdated is "age has passed since the invoice's last write": its
// updated_at, and its charge hold if it has one, move back by age. The unknown
// sweep reads an invoice once its last write is a minute old and no charge
// holds it any more.
func (r *shRig) ageUpdated(invoiceID string, age time.Duration) {
	r.exec(`UPDATE invoices SET updated_at = now() - make_interval(secs => $2),
			charge_hold_token = CASE WHEN charge_hold_token = auto_charge_claimed_until
				THEN charge_hold_token - make_interval(secs => $2) ELSE charge_hold_token END,
			auto_charge_claimed_until = auto_charge_claimed_until - make_interval(secs => $2)
		WHERE id = $1`, invoiceID, age.Seconds())
}

// assertPaidNowHoldClearedLater: the reconciler's settle pays the invoice at
// once, and, as in every design (MarkPaid writes no hold), the charge hold an
// unclear outcome kept is cleared by the stale-hold sweep once it ends. A
// hold on a paid invoice blocks nothing a paid invoice allows.
func (r *shRig) assertPaidNowHoldClearedLater(invoiceID, piID string) {
	r.t.Helper()
	got := r.get(invoiceID)
	if got.Status != domain.InvoicePaid || got.PaymentStatus != domain.PaymentSucceeded || got.StripePaymentIntentID != piID {
		r.t.Fatalf("after the reconciler: %s/%s on %q, want paid/succeeded on %q", got.Status, got.PaymentStatus, got.StripePaymentIntentID, piID)
	}
	if n := r.fake.countPIs(); n != 1 {
		r.t.Errorf("PaymentIntents at Stripe = %d, want 1", n)
	}
	r.ageHold(invoiceID, staleSweepAge)
	r.runClean()
	r.assertPaidOnce(invoiceID, piID)
}

func (r *shRig) attemptsOf(invoiceID string) []domain.InvoiceChargeAttempt {
	r.t.Helper()
	a, err := r.invoices.ListChargeAttemptsByInvoice(r.ctx, r.tenantID, invoiceID)
	if err != nil {
		r.t.Fatalf("list attempts: %v", err)
	}
	return a
}

// The id is on disk before any money can move, and one charge is one
// PaymentIntent, one attempt row, one receipt.
func TestTwoStep_SavesTheAttemptBeforeConfirming(t *testing.T) {
	r := newTwoStepRig(t, "A2 Saves First")
	inv := r.seed("INV-A2-OK")
	pendingAtConfirm := -1
	r.fake.beforeConfirm = func() {
		pendingAtConfirm = r.count(`SELECT count(*) FROM invoice_charge_attempts
			WHERE invoice_id = $1 AND outcome = 'pending' AND stripe_payment_intent_id <> ''`, inv.ID)
	}
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if pendingAtConfirm != 1 {
		t.Errorf("pending attempt rows when the confirm was sent = %d, want 1", pendingAtConfirm)
	}
	pi := r.fake.onlyPI()
	r.assertPaidOnce(inv.ID, pi.id)
	if r.fake.creates != 1 || r.fake.confirms != 1 {
		t.Errorf("calls: creates %d, confirms %d; want 1 and 1", r.fake.creates, r.fake.confirms)
	}
	if a := r.attemptsOf(inv.ID); len(a) != 1 || a[0].Outcome != domain.ChargeAttemptSucceeded || a[0].StripePaymentIntentID != pi.id {
		t.Errorf("attempt rows = %+v, want one succeeded row for %s", a, pi.id)
	}
}

// Stripe charged and its answer was a 500. The one-call charge's answer named
// no PaymentIntent, so the invoice was parked and waited up to 24 hours when
// Stripe's list showed nothing. The confirm names its PaymentIntent: the
// invoice is unknown on that id, not parked, and the reconciler records the
// payment with one read by id.
func TestTwoStep_ConfirmAnswerLost_NamesThePayment_NotParked(t *testing.T) {
	r := newTwoStepRig(t, "A2 Confirm 500")
	inv := r.seed("INV-A2-500")
	r.fake.queueConfirm(fakeMode{kind: fakeSave500})
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the 500's error")
	}
	pi := r.fake.onlyPI()
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != pi.id {
		t.Fatalf("after the 500: %s on %q, want unknown on %q", got.PaymentStatus, got.StripePaymentIntentID, pi.id)
	}
	if got.Parked() {
		t.Fatal("the invoice is parked: a two-step confirm always names its PaymentIntent")
	}
	r.assertNoSecondCharge(inv.ID)

	reads := r.fake.readCount()
	r.ageUpdated(inv.ID, 5*time.Minute)
	r.runClean()
	if n := r.fake.readCount() - reads; n != 1 {
		t.Errorf("Stripe reads to resolve it = %d, want 1 (one read by id)", n)
	}
	r.assertPaidNowHoldClearedLater(inv.ID, pi.id)
}

// The caller's deadline ends while Stripe is still confirming (dunning 15 s,
// auto-charge 30 s). The one-call charge parked it; now it is unknown on its
// PaymentIntent.
func TestTwoStep_DeadlineEndsDuringConfirm_NotParked(t *testing.T) {
	r := newTwoStepRig(t, "A2 Deadline")
	inv := r.seed("INV-A2-DEADLINE")
	r.fake.queueConfirm(fakeMode{kind: fakeOutliveCtx})
	ctx, cancel := context.WithTimeout(r.ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := r.chargeAs(ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the deadline's error")
	}
	pi := r.fake.onlyPI()
	got := r.get(inv.ID)
	if got.Parked() || got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != pi.id {
		t.Fatalf("after the deadline: %s on %q parked=%v, want unknown on %q, not parked", got.PaymentStatus, got.StripePaymentIntentID, got.Parked(), pi.id)
	}
	r.assertNoSecondCharge(inv.ID)
	r.ageUpdated(inv.ID, 5*time.Minute)
	r.runClean()
	r.assertPaidNowHoldClearedLater(inv.ID, pi.id)
}

// Stripe charged and the process died before saving the result. The pending
// attempt names the PaymentIntent; the charge hold still blocks every other
// way to pay (a changed card, another charge kind, the hosted page), and the
// stale-hold sweep records the payment.
func TestTwoStep_CrashAfterConfirm_NoSecondPayment(t *testing.T) {
	r := newTwoStepRig(t, "A2 Crash")
	inv := r.seed("INV-A2-CRASH")
	r.fake.queueConfirm(fakeMode{kind: fakeCrashAfterCreate})
	if !crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") }) {
		t.Fatal("the charge did not reach the crash point")
	}
	pi := r.fake.onlyPI()
	if a := r.attemptsOf(inv.ID); len(a) != 1 || a[0].Outcome != domain.ChargeAttemptPending || a[0].StripePaymentIntentID != pi.id {
		t.Fatalf("attempt rows after the crash = %+v, want one pending row naming %s", a, pi.id)
	}
	r.assertNoSecondCharge(inv.ID)
	r.ageHold(inv.ID, staleSweepAge)
	r.assertNoSecondCharge(inv.ID)
	r.runClean()
	r.assertPaidOnce(inv.ID, pi.id)
	if a := r.attemptsOf(inv.ID); len(a) != 1 || a[0].Outcome != domain.ChargeAttemptSucceeded || a[0].StripePaymentIntentID != pi.id {
		t.Errorf("attempt rows after the sweep = %+v, want one succeeded row for %s (one attempt, one row)", a, pi.id)
	}
}

// The create got an unclear answer (a 500 Stripe saved under its key). No
// confirm was sent, so nothing can have been charged and the invoice is not
// marked unknown. The hold stays for the stale-hold sweep, which moves the
// counter; the next charge uses the next key and charges once.
func TestTwoStep_CreateUnclear_NothingConfirmed(t *testing.T) {
	r := newTwoStepRig(t, "A2 Create 500")
	inv := r.seed("INV-A2-CREATE500")
	r.fake.queue(fakeMode{kind: fakeSave500})
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want an error")
	}
	if r.fake.confirms != 0 || r.fake.countPIs() != 0 {
		t.Fatalf("confirms %d, PaymentIntents %d; want 0 and 0", r.fake.confirms, r.fake.countPIs())
	}
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentPending {
		t.Fatalf("invoice is %s, want pending: nothing was confirmed", got.PaymentStatus)
	}
	if r.hold(inv.ID) == nil {
		t.Fatal("no hold: the create's saved 500 must wait for the sweep to move the counter")
	}
	r.ageHold(inv.ID, staleSweepAge)
	r.runClean()
	if got := r.get(inv.ID); got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 || r.hold(inv.ID) != nil {
		t.Fatalf("after the sweep: seq %d hold %v, want seq %d and no hold", got.ChargeAttemptSeq, r.hold(inv.ID), inv.ChargeAttemptSeq+1)
	}
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
		t.Fatalf("charge under the next key: %v", err)
	}
	r.assertPaidOnce(inv.ID, r.fake.onlyPI().id)
}

// paymentFailedEmails counts the payment-failed emails queued for the tenant.
func (r *shRig) paymentFailedEmails() int {
	r.t.Helper()
	return r.count(`SELECT count(*) FROM email_outbox WHERE tenant_id = $1 AND email_type = $2`, r.tenantID, email.TypePaymentFailed)
}

// The confirm failed before Stripe received it, so the invoice is unknown on a
// PaymentIntent still in requires_confirmation. One call never left a
// Hardcount charge there, so the reconciler read that status as "in flight"
// and skipped it on every tick: the invoice was stuck. Now it cancels the
// PaymentIntent first (a confirm arriving later is refused) and ends the
// attempt as one that charged nothing: back to pending, no payment-failed
// email, no dunning, the attempt row closed. The next charge takes one payment.
func TestTwoStep_ConfirmNeverArrived_CanceledThenChargedOnce(t *testing.T) {
	r := newTwoStepRig(t, "A2 Never Arrived")
	inv := r.seed("INV-A2-NEVER")
	r.fake.queueConfirm(fakeMode{kind: fakeConfirmNeverArrived})
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the unclear error")
	}
	first := r.fake.onlyPI()
	if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != first.id {
		t.Fatalf("setup: %s on %q, want unknown on %q", got.PaymentStatus, got.StripePaymentIntentID, first.id)
	}
	r.ageUpdated(inv.ID, 5*time.Minute)
	r.runClean()
	if st := r.fake.pi(first.id).status; st != "canceled" {
		t.Fatalf("the never-confirmed PaymentIntent is %s at Stripe, want canceled before the invoice is freed", st)
	}
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentPending || r.hold(inv.ID) != nil {
		t.Fatalf("after the reconciler: %s, hold %v; want pending and no hold (nothing was charged)", got.PaymentStatus, r.hold(inv.ID))
	}
	if n := r.paymentFailedEmails(); n != 0 {
		t.Errorf("payment-failed emails = %d, want 0: no bank saw a charge", n)
	}
	if n := len(r.dunning.started); n != 0 {
		t.Errorf("dunning runs started = %d, want 0", n)
	}
	if a := r.attemptsOf(inv.ID); len(a) != 1 || a[0].Outcome != domain.ChargeAttemptFailed || a[0].ProviderReason != domain.AttemptReasonNeverConfirmed {
		t.Errorf("attempt rows = %+v, want one failed row saying nothing was charged", a)
	}
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
		t.Fatalf("next charge: %v", err)
	}
	got = r.get(inv.ID)
	if got.Status != domain.InvoicePaid || got.StripePaymentIntentID == first.id || r.fake.countPIs() != 2 {
		t.Fatalf("after the next charge: %s on %q with %d PaymentIntents; want paid on a new PaymentIntent, 2 in all (one canceled)",
			got.Status, got.StripePaymentIntentID, r.fake.countPIs())
	}
}

// The reconciler canceled the never-confirmed PaymentIntent but did not get to
// save the end (a crash, a database fault). The next tick finds it canceled
// with no charge: still not a failed payment, so still no email.
func TestTwoStep_CanceledWithNoCharge_EndsWithNoEmail(t *testing.T) {
	r := newTwoStepRig(t, "A2 Canceled No Charge")
	inv := r.seed("INV-A2-CANCELED")
	r.fake.queueConfirm(fakeMode{kind: fakeConfirmNeverArrived})
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the unclear error")
	}
	first := r.fake.onlyPI()
	if err := r.fake.CancelPaymentIntent(r.ctx, first.id); err != nil {
		t.Fatal(err)
	}
	r.ageUpdated(inv.ID, 5*time.Minute)
	r.runClean()
	if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentPending {
		t.Fatalf("after the reconciler: %s, want pending", got.PaymentStatus)
	}
	if n := r.paymentFailedEmails(); n != 0 {
		t.Errorf("payment-failed emails = %d, want 0", n)
	}
}

// sweepUntilClear runs the stale-hold sweep once and requires the hold gone:
// every case it is used for (nothing saved, or an unconfirmed PaymentIntent
// it cancels) resolves in one run.
func (r *shRig) sweepUntilClear(invoiceID string) {
	r.t.Helper()
	r.ageHold(invoiceID, staleSweepAge)
	r.runClean()
	if r.hold(invoiceID) != nil {
		r.t.Fatal("hold still set after the sweep")
	}
}

// failedBeforeConfirm checks what a failure before the stamp must leave: one
// PaymentIntent at Stripe, never confirmed; nothing charged; the invoice
// still pending (not unknown, not parked) and held, with no attempt row. Then
// the stale-hold sweep clears the hold and moves the counter without asking
// Stripe, and the next charge takes exactly one payment on a new
// PaymentIntent. The unconfirmed PaymentIntent stays where it is and can take
// no money: only one stamped on the invoice is ever confirmed.
func (r *shRig) failedBeforeConfirm(inv domain.Invoice) {
	r.t.Helper()
	orphan := r.fake.onlyPI()
	if orphan.status != "requires_confirmation" {
		r.t.Fatalf("the PaymentIntent at Stripe is %s, want requires_confirmation (created, never confirmed)", orphan.status)
	}
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentPending || got.Parked() || r.hold(inv.ID) == nil {
		r.t.Fatalf("invoice %s parked=%v hold=%v, want pending, not parked, held", got.PaymentStatus, got.Parked(), r.hold(inv.ID))
	}
	r.assertNoSecondCharge(inv.ID)
	reads := r.fake.readCount()
	r.sweepUntilClear(inv.ID)
	if n := r.fake.readCount() - reads; n != 0 {
		r.t.Fatalf("Stripe reads by the stale-hold sweep = %d, want 0", n)
	}
	if st := r.fake.pi(orphan.id).status; st != "requires_confirmation" {
		r.t.Fatalf("the never-stamped PaymentIntent is %s, want it left unconfirmed", st)
	}
	if got := r.get(inv.ID); got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 {
		r.t.Fatalf("counter %d, want %d: the next charge must use a new key", got.ChargeAttemptSeq, inv.ChargeAttemptSeq+1)
	}
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
		r.t.Fatalf("next charge: %v", err)
	}
	got = r.get(inv.ID)
	if got.Status != domain.InvoicePaid || got.StripePaymentIntentID == orphan.id || r.fake.countPIs() != 2 {
		r.t.Fatalf("after the next charge: %s on %q with %d PaymentIntents; want paid on a new one, 2 in all (one canceled)",
			got.Status, got.StripePaymentIntentID, r.fake.countPIs())
	}
	if n := r.events(domain.EventPaymentSucceeded, inv.ID); n != 1 {
		r.t.Errorf("payment.succeeded rows = %d, want 1", n)
	}
}

// Step 1 (create) timed out after Stripe had created the PaymentIntent.
func TestTwoStep_CreateAnswerLost_StripeCreatedIt(t *testing.T) {
	r := newTwoStepRig(t, "A2 Create Lost")
	inv := r.seed("INV-A2-CREATELOST")
	r.fake.queue(fakeMode{kind: fakeOutliveCtx})
	ctx, cancel := context.WithTimeout(r.ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := r.chargeAs(ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the deadline's error")
	}
	if r.fake.confirms != 0 {
		t.Fatalf("confirms = %d, want 0", r.fake.confirms)
	}
	r.failedBeforeConfirm(inv)
}

// The process died right after Stripe created the PaymentIntent (step 1),
// before anything was saved.
func TestTwoStep_CrashAfterCreate(t *testing.T) {
	r := newTwoStepRig(t, "A2 Crash After Create")
	inv := r.seed("INV-A2-CRASHCREATE")
	r.fake.queue(fakeMode{kind: fakeCrashAfterCreate})
	if !crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") }) {
		t.Fatal("the charge did not reach the crash point")
	}
	if n := len(r.attemptsOf(inv.ID)); n != 0 {
		t.Fatalf("attempt rows = %d, want 0 (the crash came before step 2)", n)
	}
	r.failedBeforeConfirm(inv)
}

// The process died after stamping the invoice (step 2) and before the confirm
// left (step 3). The invoice says unknown on the PaymentIntent, so no charge
// can start; once the hold has ended the unknown sweep reads the
// PaymentIntent, finds it never confirmed, cancels it and frees the invoice
// with no payment-failed email, and the next charge pays once on a new one.
func TestTwoStep_CrashBetweenStampAndConfirm(t *testing.T) {
	r := newTwoStepRig(t, "A2 Crash Before Confirm")
	inv := r.seed("INV-A2-CRASHSTAMP")
	r.fake.queueConfirm(fakeMode{kind: fakeCrashBeforeSend})
	if !crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") }) {
		t.Fatal("the charge did not reach the crash point")
	}
	pi := r.fake.onlyPI()
	if a := r.attemptsOf(inv.ID); len(a) != 1 || a[0].Outcome != domain.ChargeAttemptPending || a[0].StripePaymentIntentID != pi.id {
		t.Fatalf("attempt rows = %+v, want one pending row naming %s", a, pi.id)
	}
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != pi.id || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 || r.hold(inv.ID) == nil {
		t.Fatalf("after the crash: %s on %q seq %d hold %v; want unknown on %s, seq %d, held",
			got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq, r.hold(inv.ID), pi.id, inv.ChargeAttemptSeq+1)
	}
	r.assertNoSecondCharge(inv.ID)

	// While the hold is live the unknown sweep leaves it alone: its charger
	// may still be waiting for Stripe.
	r.exec(`UPDATE invoices SET updated_at = now() - interval '2 minutes' WHERE id = $1`, inv.ID)
	r.runClean()
	if st := r.fake.pi(pi.id).status; st != "requires_confirmation" {
		t.Fatalf("the PaymentIntent is %s while the hold is live, want it untouched", st)
	}

	r.ageUpdated(inv.ID, 6*time.Minute)
	r.runClean()
	got = r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentPending || r.hold(inv.ID) != nil || r.fake.pi(pi.id).status != "canceled" {
		t.Fatalf("after the unknown sweep: %s hold %v, PaymentIntent %s; want pending, no hold, canceled",
			got.PaymentStatus, r.hold(inv.ID), r.fake.pi(pi.id).status)
	}
	for _, a := range r.attemptsOf(inv.ID) {
		if a.StripePaymentIntentID == pi.id && (a.Outcome != domain.ChargeAttemptFailed || a.ProviderReason != domain.AttemptReasonNeverConfirmed) {
			t.Errorf("the canceled PaymentIntent's attempt row is %q (%q), want failed: never reached Stripe", a.Outcome, a.ProviderReason)
		}
	}
	if n := r.paymentFailedEmails(); n != 0 {
		t.Errorf("payment-failed emails = %d, want 0: no bank was asked", n)
	}
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
		t.Fatalf("next charge: %v", err)
	}
	if got := r.get(inv.ID); got.Status != domain.InvoicePaid || got.StripePaymentIntentID == pi.id || r.fake.countPIs() != 2 {
		t.Fatalf("after the next charge: %s on %q with %d PaymentIntents; want paid on a new one, 2 in all (one canceled)",
			got.Status, got.StripePaymentIntentID, r.fake.countPIs())
	}
}

// A decline is still per attempt: the next attempt (a dunning retry) gets a
// new PaymentIntent under the next key, as today.
func TestTwoStep_Decline_NextAttemptIsANewPaymentIntent(t *testing.T) {
	r := newTwoStepRig(t, "A2 Decline")
	inv := r.seed("INV-A2-DECLINE")
	r.fake.queueConfirm(fakeMode{kind: fakeDecline})
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the decline")
	}
	got := r.get(inv.ID)
	// The counter moved twice: the stamp before the confirm, then the decline.
	if got.PaymentStatus != domain.PaymentFailed || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+2 || r.hold(inv.ID) != nil {
		t.Fatalf("after the decline: %s seq %d hold %v; want failed, seq %d, no hold",
			got.PaymentStatus, got.ChargeAttemptSeq, r.hold(inv.ID), inv.ChargeAttemptSeq+2)
	}
	if _, err := r.chargeAs(r.ctx, dunningRetry, inv.ID, "pm_card_2"); err != nil {
		t.Fatalf("dunning retry: %v", err)
	}
	got = r.get(inv.ID)
	if got.Status != domain.InvoicePaid || r.fake.countPIs() != 2 {
		t.Fatalf("after the retry: %s with %d PaymentIntents, want paid with 2 (one declined, one succeeded)", got.Status, r.fake.countPIs())
	}
}
