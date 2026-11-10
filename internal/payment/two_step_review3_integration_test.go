package payment

// From the third independent review of the two-step charge (ADR-137,
// 2026-11-10): a reconciler cancel Stripe refuses must not stall the unknown
// sweep; a charger past its hold must not get its PaymentIntent confirmed
// behind the stale-hold sweep's back; a decline that lands before the
// reconciler's cancel is recorded as the decline it was.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// r3CancelRefuser is the rig's fake Stripe with cancels refused for some
// PaymentIntents: a restricted key that may read PaymentIntents but not write
// them, or a Stripe incident answering cancels with an error.
type r3CancelRefuser struct {
	*fakeStripe
	refuse map[string]bool
}

func (c *r3CancelRefuser) CancelPaymentIntent(ctx context.Context, id string) error {
	if c.refuse[id] {
		return errors.New("stripe cancel: The provided key does not have the required permissions for this endpoint")
	}
	return c.fakeStripe.CancelPaymentIntent(ctx, id)
}

// The unknown sweep reads one page a tick, least recently stamped first
// (NULLS FIRST). The new requires_confirmation branch returns on a refused
// cancel without stamping provider_synced_at, so such a row heads the queue on
// every tick, as the failed read did before this change stamped it. With a
// page of them, no newer unclear charge is ever read.
func TestReconcile_CancelRefused_RowRotates_QueueNotStarved(t *testing.T) {
	r := newSHRig(t, "R3 Cancel Refused Starves")
	stuck := r.seed("INV-R3-STUCK")
	stuckPI := r.fake.addPI(stripeCustomerOf(stuck), stuck.ID, "", "requires_confirmation", shDue)
	r.exec(`UPDATE invoices SET payment_status = 'unknown', stripe_payment_intent_id = $2,
		updated_at = now() - interval '20 minutes', provider_synced_at = NULL WHERE id = $1`, stuck.ID, stuckPI.id)

	newer := r.seed("INV-R3-NEWER")
	newerPI := r.fake.addPI(stripeCustomerOf(newer), newer.ID, "", "succeeded", shDue)
	r.exec(`UPDATE invoices SET payment_status = 'unknown', stripe_payment_intent_id = $2,
		updated_at = now() - interval '10 minutes', provider_synced_at = NULL WHERE id = $1`, newer.ID, newerPI.id)

	rec := NewReconciler(&r3CancelRefuser{fakeStripe: r.fake, refuse: map[string]bool{stuckPI.id: true}}, r.invoices, time.Minute)
	rec.SetSettler(r.stripe)
	for i := 0; i < 3; i++ { // three ticks, page size 1
		_, _ = rec.Run(r.ctx, 1)
	}

	if got := r.get(stuck.ID); got.ProviderSyncedAt == nil {
		t.Errorf("a refused cancel left the row unstamped (provider_synced_at NULL): it heads the unknown sweep's queue on every tick")
	}
	if got := r.get(newer.ID); got.Status != domain.InvoicePaid {
		t.Errorf("newer unknown invoice whose payment succeeded at Stripe: %s/%s after 3 ticks, want paid (starved behind the refused cancel)",
			got.Status, got.PaymentStatus)
	}
}

// A charger that stalls past its hold cannot put its PaymentIntent on the
// invoice, so it confirms nothing: the stamp is a compare-and-set that needs
// the hold still live on the database clock. Review 3 found the earlier form
// of this save could land after the sweep's list (it wrote another table).
func TestStamp_AfterTheHoldEnded_Refused(t *testing.T) {
	r := newSHRig(t, "Stamp After Hold")
	inv := r.seed("INV-STAMP-LATE")
	c, err := r.claim(autoCharge, inv.ID)
	if err != nil || !c.ok() {
		t.Fatalf("claim: %v ok=%v", err, c.ok())
	}
	attempt := domain.InvoiceChargeAttempt{
		InvoiceID: inv.ID, StripePaymentIntentID: "pi_late", Trigger: domain.ChargeTriggerAutoCharge,
		Outcome: domain.ChargeAttemptPending, StripePaymentMethodID: "pm_card_1", AmountCents: shDue,
		OccurredAt: time.Now().UTC(),
	}
	held := r.hold(inv.ID)
	r.exec(`UPDATE invoices SET auto_charge_claimed_until = now() - interval '1 second', charge_hold_token = now() - interval '1 second' WHERE id = $1`, inv.ID)
	ended := r.hold(inv.ID)
	for name, hold := range map[string]*time.Time{"the hold as claimed": held, "the hold as it now reads": ended} {
		if _, ok, err := r.invoices.StampPendingConfirm(r.ctx, r.tenantID, attempt, domain.ChargeHold{Until: *hold}, inv.ChargeAttemptSeq); err != nil || ok {
			t.Fatalf("%s: stamp applied=%v err=%v after the hold ended, want refused", name, ok, err)
		}
	}
	if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentPending || got.StripePaymentIntentID != "" || got.ChargeAttemptSeq != inv.ChargeAttemptSeq {
		t.Fatalf("a refused stamp changed the invoice: %s on %q seq %d", got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq)
	}
	if n := len(r.attemptsOf(inv.ID)); n != 0 {
		t.Errorf("a refused stamp left %d attempt rows", n)
	}
}

// The stale-hold sweep's clear and the charger's stamp are compare-and-sets
// on the same invoice row, so exactly one wins. Here the stamp lands between
// the sweep's list and its clear (written directly: the store refuses it once
// the hold has ended): the clear must not release the invoice, and the payment
// its confirm took is recorded once by the unknown sweep.
func TestStaleSweep_StampLandedAfterTheList_NotReleased(t *testing.T) {
	r := newSHRig(t, "Sweep Stamp Race")
	inv := r.seed("INV-SWEEP-RACE")
	c, err := r.claim(autoCharge, inv.ID)
	if err != nil || !c.ok() {
		t.Fatalf("claim: %v ok=%v", err, c.ok())
	}
	r.ageHold(inv.ID, staleSweepAge)
	holds, err := r.invoices.ListStaleChargeHolds(r.ctx, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	var h domain.StaleChargeHold
	for _, x := range holds {
		if x.Invoice.ID == inv.ID {
			h = x
		}
	}
	if h.Invoice.ID == "" || !h.Tokened || h.Invoice.PaymentStatus != domain.PaymentPending {
		t.Fatalf("setup: want a tokened stale hold on a pending invoice, got %+v", h)
	}
	pi := r.fake.addPI(stripeCustomerOf(inv), inv.ID, "", "succeeded", shDue)
	r.exec(`UPDATE invoices SET payment_status = 'unknown', stripe_payment_intent_id = $2,
		charge_attempt_seq = charge_attempt_seq + 1 WHERE id = $1`, inv.ID, pi.id)

	result, err := r.rec.checkStaleHold(r.ctx, h)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result != staleHoldLostRace {
		t.Fatalf("result = %s, want lost_race: the invoice changed after the sweep read it", result)
	}
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); !errors.Is(err, errNotClaimed) {
		t.Fatalf("a second charge: err = %v, want its claim refused", err)
	}
	r.runClean()
	if ids := r.succeededFor(inv.ID); len(ids) != 1 {
		t.Fatalf("succeeded PaymentIntents = %v, want one", ids)
	}
	if got := r.get(inv.ID); got.Status != domain.InvoicePaid || got.StripePaymentIntentID != pi.id {
		t.Fatalf("after the unknown sweep: %s on %q, want paid on %s", got.Status, got.StripePaymentIntentID, pi.id)
	}
}

// r3DeclineThenCancel is the rig's fake where the charge's own confirm,
// still in flight at Stripe, lands and is declined (a hard decline) just
// before the reconciler's cancel, which then succeeds on the
// requires_payment_method PaymentIntent.
type r3DeclineThenCancel struct {
	*fakeStripe
	declineCode string
}

func (c *r3DeclineThenCancel) CancelPaymentIntent(ctx context.Context, id string) error {
	c.mu.Lock()
	for _, pi := range c.pis {
		if pi.id == id && pi.status == "requires_confirmation" {
			pi.status = "requires_payment_method"
			pi.declineCode, pi.failureMsg = c.declineCode, "Your card was declined."
		}
	}
	c.mu.Unlock()
	return c.fakeStripe.CancelPaymentIntent(ctx, id)
}

// The reconciler's requires_confirmation branch reads the PaymentIntent,
// cancels it, and on a successful cancel ends the attempt as "never reached
// Stripe; nothing was charged" without looking at what the cancel found. A
// cancel succeeds on a declined (requires_payment_method) PaymentIntent too,
// so a confirm that reached the bank between the read and the cancel is
// recorded as never sent: no decline code, and the same tick's auto-charge
// charges the same hard-declined card again.
func TestReconcile_DeclineBeforeCancel_RecordedAsTheDecline(t *testing.T) {
	r := newSHRig(t, "R3 Decline Then Cancel")
	inv := r.seed("INV-R3-DECLCANCEL")
	r.fake.queueConfirm(fakeMode{kind: fakeConfirmNeverArrived})
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the unclear error")
	}
	pi := r.fake.onlyPI()
	r.ageUpdated(inv.ID, 5*time.Minute)

	rec := NewReconciler(&r3DeclineThenCancel{fakeStripe: r.fake, declineCode: "stolen_card"}, r.invoices, time.Minute)
	rec.SetSettler(r.stripe)
	_, _ = rec.Run(r.ctx, 50)

	got := r.get(inv.ID)
	t.Logf("invoice after reconcile: %s/%s on %s", got.Status, got.PaymentStatus, got.StripePaymentIntentID)
	for _, a := range r.attemptsOf(inv.ID) {
		if a.StripePaymentIntentID == pi.id {
			t.Logf("attempt %s: outcome=%s decline_code=%q reason=%q", a.StripePaymentIntentID, a.Outcome, a.DeclineCode, a.ProviderReason)
			if a.ProviderReason == domain.AttemptReasonNeverConfirmed {
				t.Errorf("a PaymentIntent the bank declined is recorded as %q", a.ProviderReason)
			}
		}
	}
	if code, err := r.invoices.RetryBlockingDecline(r.ctx, r.tenantID, inv.ID, "pm_card_1"); err != nil || code == "" {
		t.Errorf("hard decline not on record for the card (code=%q err=%v): the next retry charges it again", code, err)
	}
}
