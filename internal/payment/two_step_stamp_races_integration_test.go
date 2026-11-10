package payment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
)

// From review 4 (2026-11-10): the ways the design could confirm, or charge
// again, around the stamp, and a late note from a stalled charger.

// commitThenError applies the real stamp and then reports an error, as the
// driver does when the commit reached the server and its reply was lost (the
// caller's context ended, the connection dropped).
type commitThenError struct{ InvoiceUpdater }

func (c commitThenError) StampPendingConfirm(ctx context.Context, tenantID string, a domain.InvoiceChargeAttempt, hold domain.ChargeHold, seq int64) (domain.Invoice, bool, error) {
	if _, ok, err := c.InvoiceUpdater.StampPendingConfirm(ctx, tenantID, a, hold, seq); err != nil || !ok {
		return domain.Invoice{}, ok, err
	}
	return domain.Invoice{}, false, errors.New("commit: connection reset by peer")
}

// The stamp committed but the charger was told it failed. The charger frees
// its hold and returns "nothing confirmed". That must not free the invoice
// for a second charge while the first PaymentIntent can still be confirmed:
// no confirm is sent, no claim admits the invoice (it is unknown), the sweep
// cancels the PaymentIntent, and the next charge takes exactly one payment.
func TestTwoStep_StampCommittedButReportedFailed_OnePayment(t *testing.T) {
	r := newTwoStepRig(t, "R4 Ambiguous Stamp")
	inv := r.seed("INV-R4-AMBIG")
	r.stripe.invoices = commitThenError{InvoiceUpdater: r.invoices}
	_, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1")
	r.stripe.invoices = r.invoices
	if !errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("err = %v, want ErrPaymentTransient", err)
	}
	if r.fake.confirms != 0 {
		t.Fatalf("confirms = %d after a stamp the charger believes failed, want 0", r.fake.confirms)
	}
	first := r.fake.onlyPI()
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != first.id {
		t.Fatalf("after the ambiguous stamp: %s on %q, want unknown on %s (the commit landed)", got.PaymentStatus, got.StripePaymentIntentID, first.id)
	}
	r.assertNoSecondCharge(inv.ID) // every claim and the hosted page refuse it

	r.timePasses(inv.ID, 2*time.Minute) // past the unknown sweep's cool-off
	r.runClean()
	if st := r.fake.pi(first.id).status; st != "canceled" {
		t.Fatalf("the unconfirmed PaymentIntent is %s, want canceled before the invoice is freed", st)
	}
	if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentPending {
		t.Fatalf("after the sweep: %s, want pending", got.PaymentStatus)
	}
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
		t.Fatalf("next charge: %v", err)
	}
	if paid := r.succeededFor(inv.ID); len(paid) != 1 || paid[0] == first.id {
		t.Fatalf("PaymentIntents that took money: %v, want exactly one, and not %s", paid, first.id)
	}
	if n := r.paymentFailedEmails(); n != 0 {
		t.Errorf("payment-failed emails = %d, want 0", n)
	}
}

// A charger stalls between the stamp and the confirm (a paused process) for
// longer than its hold. Meanwhile the hold ends, the unknown sweep cancels
// the PaymentIntent and frees the invoice, and another charge pays it. When
// the stalled charger wakes and sends its confirm, Stripe refuses it: one
// payment, no duplicate-charge anomaly, the invoice paid on the second
// PaymentIntent.
func TestTwoStep_ChargerStallsPastItsHoldBeforeConfirm_OnePayment(t *testing.T) {
	r := newTwoStepRig(t, "R4 Stalled Charger")
	inv := r.seed("INV-R4-STALL")
	var firstID string
	r.fake.mu.Lock()
	r.fake.beforeConfirm = func() {
		r.fake.mu.Lock()
		r.fake.beforeConfirm = nil
		r.fake.mu.Unlock()
		firstID = r.fake.onlyPI().id
		// The stall: sixteen minutes pass, a billing tick runs, then an
		// auto-charge.
		r.timePasses(inv.ID, 16*time.Minute)
		r.runClean()
		if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentPending {
			t.Errorf("during the stall, after the sweep: %s, want pending", got.PaymentStatus)
		}
		if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
			t.Errorf("the second charger: %v", err)
		}
	}
	r.fake.mu.Unlock()

	_, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1")
	if err == nil {
		t.Fatal("the stalled charger's late confirm succeeded; want Stripe's refusal of a canceled PaymentIntent")
	}
	if st := r.fake.pi(firstID).status; st != "canceled" {
		t.Errorf("the stalled charger's PaymentIntent is %s, want canceled", st)
	}
	paid := r.succeededFor(inv.ID)
	if len(paid) != 1 || paid[0] == firstID {
		t.Fatalf("PaymentIntents that took money: %v, want exactly one (the second charger's)", paid)
	}
	got := r.get(inv.ID)
	if got.Status != domain.InvoicePaid || got.StripePaymentIntentID != paid[0] {
		t.Errorf("invoice %s on %q, want paid on %s: the stalled charger's late answer must not write over it", got.Status, got.StripePaymentIntentID, paid[0])
	}
	if n := r.events(domain.EventPaymentDuplicateCharge, inv.ID); n != 0 {
		t.Errorf("payment.duplicate_charge rows = %d, want 0", n)
	}
	if n := r.receipts(); n != 1 {
		t.Errorf("receipts = %d, want 1", n)
	}
}

// Minor (timeline only): the stalled charger's late confirm is refused, and
// afterConfirmError then writes its "unknown" note on the attempt row the
// sweep had already closed as "never reached Stripe". Correct: a closed
// attempt keeps its outcome; nothing later reads that PaymentIntent again, so
// the row would say "result unknown" for good on a paid invoice.
func TestTwoStep_StalledChargersLateNote_DoesNotReopenAClosedAttempt(t *testing.T) {
	r := newTwoStepRig(t, "R4 Stalled Note")
	inv := r.seed("INV-R4-STALLNOTE")
	var firstID string
	r.fake.mu.Lock()
	r.fake.beforeConfirm = func() {
		r.fake.mu.Lock()
		r.fake.beforeConfirm = nil
		r.fake.mu.Unlock()
		firstID = r.fake.onlyPI().id
		r.timePasses(inv.ID, 16*time.Minute)
		r.runClean()
	}
	r.fake.mu.Unlock()
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the late confirm refused")
	}
	for _, a := range r.attemptsOf(inv.ID) {
		if a.StripePaymentIntentID == firstID && a.Outcome != domain.ChargeAttemptFailed {
			t.Errorf("attempt %s is %q (%q) after the late note; want it left failed, as the sweep closed it", firstID, a.Outcome, a.ProviderReason)
		}
	}
	if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentPending {
		t.Errorf("invoice %s, want pending (the late answer must not move it)", got.PaymentStatus)
	}
}

// afterCreateHook runs hook once, right after Stripe has answered the create
// and before the charger stamps the invoice: something that changes the
// invoice in between.
type afterCreateHook struct {
	*fakeStripe
	hook func()
}

func (c *afterCreateHook) CreateUnconfirmedPaymentIntent(ctx context.Context, p PaymentIntentParams) (PaymentIntentResult, error) {
	res, err := c.fakeStripe.CreateUnconfirmedPaymentIntent(ctx, p)
	if h := c.hook; h != nil && err == nil {
		c.hook = nil
		h()
	}
	return res, err
}

// A credit note is issued between the charge's read of the invoice and its
// stamp (a credit note does not read the charge hold). The PaymentIntent was
// created for the old amount, so the stamp must not apply and nothing may be
// confirmed: the stamp's amount predicate is the only thing that stops a
// charge of the amount before the credit note.
//
// The attempt ends there: the hold is cleared and the counter moved, so the
// next charge creates a new PaymentIntent for the reduced amount under a new
// key. Under the old key Stripe answers "same key, other parameters", which
// reads as an unclear charge: the hold was kept, the invoice waited for the
// stale-hold sweep, and the breaker every tenant shares counted a failure.
func TestTwoStep_CreditNoteBeforeTheStamp_NothingConfirmed_ThenTheNewAmountOnce(t *testing.T) {
	r := newTwoStepRig(t, "CN Before Stamp")
	inv := r.seed("INV-CN-STAMP")
	const credit = 4000
	notes := creditnote.NewPostgresStore(r.db)
	issuer := creditnote.NewService(notes, r.invoices, nil)
	hooked := &afterCreateHook{fakeStripe: r.fake}
	hooked.hook = func() {
		cn, err := notes.Create(r.ctx, r.tenantID, domain.CreditNote{
			InvoiceID: inv.ID, CustomerID: inv.CustomerID, CreditNoteNumber: "CN-STAMP-1",
			Status: domain.CreditNoteDraft, Reason: "fraudulent",
			SubtotalCents: credit, TotalCents: credit, Currency: "USD", RefundStatus: domain.RefundNone,
		})
		if err != nil {
			t.Fatalf("create credit note: %v", err)
		}
		if _, err := issuer.Issue(r.ctx, r.tenantID, cn.ID); err != nil {
			t.Fatalf("issue credit note inside the create: %v", err)
		}
	}
	r.rewire(hooked)

	_, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1")
	if !errors.Is(err, ErrPaymentTransient) || IsUnknownPaymentFailure(err) {
		t.Fatalf("charge 1: err = %v (counts as a Stripe failure: %v), want a skipped charge that counts as none", err, IsUnknownPaymentFailure(err))
	}
	if r.fake.confirms != 0 {
		t.Fatalf("confirms = %d: a PaymentIntent for the amount before the credit note was confirmed", r.fake.confirms)
	}
	first := r.fake.onlyPI()
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentPending || got.StripePaymentIntentID != "" || got.AmountDueCents != shDue-credit {
		t.Fatalf("after charge 1: %s on %q with %d due, want pending on no PaymentIntent with %d due", got.PaymentStatus, got.StripePaymentIntentID, got.AmountDueCents, shDue-credit)
	}
	if h := r.hold(inv.ID); h != nil {
		t.Fatalf("the hold of an attempt that confirmed nothing is still set (%v): the invoice waits for the stale-hold sweep", h)
	}
	if got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 {
		t.Fatalf("charge counter = %d, want %d: the next charge would reuse the key with another amount", got.ChargeAttemptSeq, inv.ChargeAttemptSeq+1)
	}

	paid, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1")
	if err != nil {
		t.Fatalf("charge 2: %v (counts as a Stripe failure: %v), want the reduced amount charged", err, IsUnknownPaymentFailure(err))
	}
	if paid.Status != domain.InvoicePaid {
		t.Fatalf("after charge 2: status %s, want paid", paid.Status)
	}
	if r.fake.creates != 2 || r.fake.countPIs() != 2 {
		t.Fatalf("creates = %d, PaymentIntents = %d, want 2 and 2: one create per key", r.fake.creates, r.fake.countPIs())
	}
	second := r.fake.pi(paid.StripePaymentIntentID)
	if second.id == first.id || second.amount != shDue-credit || second.status != "succeeded" {
		t.Fatalf("the paying PaymentIntent is %s for %d (%s), want a new one for %d, succeeded", second.id, second.amount, second.status, shDue-credit)
	}
	if st := r.fake.pi(first.id).status; st != "requires_confirmation" {
		t.Fatalf("the first PaymentIntent is %s, want it never confirmed", st)
	}
	if ids := r.succeededFor(inv.ID); len(ids) != 1 || ids[0] != second.id {
		t.Fatalf("PaymentIntents that took money = %v, want only %s", ids, second.id)
	}
}
