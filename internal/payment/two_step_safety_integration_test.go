package payment

import (
	"context"
	"errors"
	"fmt"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/email"
)

// The two-step charge's safety rule (ADR-137): a confirm is only ever sent for
// a PaymentIntent the invoice already names as an unknown outcome, written by
// a compare-and-set that needs the charger's live hold. These tests break it
// from the sides the independent reviews found (2026-11-10): a stamp that
// fails, a confirm that fails before or after it reached Stripe, a cancel
// Stripe refuses, a hold taken by an earlier version, a PaymentIntent gone
// from the account.

// confirmFailer is the rig's fake Stripe with a confirm that fails before it
// reaches Stripe (as LiveStripeClient's does when the credentials cannot be
// read, or Stripe refuses the key), or that goes through and then answers
// with an error (a resend's 429 hiding the first send).
type confirmFailer struct {
	*fakeStripe
	mu          sync.Mutex
	confirmErrs []error
	chargedThen error
}

func (c *confirmFailer) ConfirmPaymentIntent(ctx context.Context, id string, p PaymentIntentParams) (PaymentIntentResult, error) {
	c.mu.Lock()
	if len(c.confirmErrs) > 0 {
		err := c.confirmErrs[0]
		c.confirmErrs = c.confirmErrs[1:]
		c.mu.Unlock()
		return PaymentIntentResult{}, err
	}
	after := c.chargedThen
	c.chargedThen = nil
	c.mu.Unlock()
	res, err := c.fakeStripe.ConfirmPaymentIntent(ctx, id, p)
	if after != nil && err == nil {
		return PaymentIntentResult{}, after
	}
	return res, err
}

// rewire rebuilds r.stripe (as newSHRig does) on client c.
func (r *shRig) rewire(c StripeClient) {
	sender := email.NewOutboxSender(email.NewOutboxStore(r.db))
	emails := shEmails{store: r.customers}
	r.stripe = NewStripe(c, r.invoices, NewPostgresWebhookStore(r.db), nil, r.dunning)
	r.stripe.SetDunningResolver(r.dunning)
	r.stripe.SetCardFetcher(shCards{})
	r.stripe.SetAnomalyRecorder(r.invoices)
	r.stripe.SetEmailReceipt(sender, emails)
	r.stripe.SetEmailPaymentFailed(sender, emails)
	r.rec.SetSettler(r.stripe)
}

// succeededFor lists the invoice's PaymentIntents at Stripe that took money.
func (r *shRig) succeededFor(invoiceID string) []string {
	r.fake.mu.Lock()
	defer r.fake.mu.Unlock()
	var ids []string
	for _, pi := range r.fake.pis {
		if pi.invoiceID == invoiceID && pi.status == "succeeded" {
			ids = append(ids, pi.id)
		}
	}
	return ids
}

// failPendingSave fails the charge's pending save, as a database fault would.
type failPendingSave struct{ InvoiceUpdater }

func (failPendingSave) StampPendingConfirm(context.Context, string, domain.InvoiceChargeAttempt, domain.ChargeHold, int64) (domain.Invoice, bool, error) {
	return domain.Invoice{}, false, errors.New("database unavailable")
}

// A failed pending save stops the charge before the confirm: nothing is
// confirmed, and the attempt is over. The hold is released and the counter
// moved together, so the retry creates a new PaymentIntent under a new key
// and confirms that one. The first stays unconfirmed at Stripe and takes no
// money. (The old key is not reused: the charger cannot tell why its stamp
// failed, and a key whose amount has changed since is answered by Stripe as
// an unclear charge.)
func TestTwoStep_PendingSaveFails_NoConfirmSent(t *testing.T) {
	r := newSHRig(t, "TS Save Fails")
	inv := r.seed("INV-TS-SAVEFAIL")
	r.stripe.invoices = failPendingSave{InvoiceUpdater: r.invoices}
	_, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1")
	r.stripe.invoices = r.invoices
	if !errors.Is(err, ErrPaymentTransient) || IsUnknownPaymentFailure(err) {
		t.Fatalf("err = %v, want ErrPaymentTransient (nothing confirmed), not counted as a Stripe failure", err)
	}
	if r.fake.confirms != 0 {
		t.Fatalf("confirms = %d after the pending save failed, want 0", r.fake.confirms)
	}
	if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentPending || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 || r.hold(inv.ID) != nil {
		t.Fatalf("after the failed save: %s seq %d hold %v, want pending, seq %d, hold released", got.PaymentStatus, got.ChargeAttemptSeq, r.hold(inv.ID), inv.ChargeAttemptSeq+1)
	}
	first := r.fake.onlyPI()
	paid, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if paid.StripePaymentIntentID == first.id || r.fake.countPIs() != 2 {
		t.Fatalf("the retry paid on %s with %d PaymentIntents at Stripe, want a new one (2 in all)", paid.StripePaymentIntentID, r.fake.countPIs())
	}
	if st := r.fake.pi(first.id).status; st != "requires_confirmation" {
		t.Fatalf("the first PaymentIntent is %s, want it never confirmed", st)
	}
	got := r.get(inv.ID)
	if got.Status != domain.InvoicePaid || got.AmountPaidCents != shDue || r.hold(inv.ID) != nil {
		t.Fatalf("after the retry: %s with %d paid, hold %v; want paid in full and no hold", got.Status, got.AmountPaidCents, r.hold(inv.ID))
	}
	if ids := r.succeededFor(inv.ID); len(ids) != 1 || ids[0] != paid.StripePaymentIntentID {
		t.Fatalf("PaymentIntents that took money = %v, want only %s", ids, paid.StripePaymentIntentID)
	}
	if n := r.events(domain.EventPaymentSucceeded, inv.ID); n != 1 {
		t.Errorf("payment.succeeded rows = %d, want 1", n)
	}
}

// A confirm that fails before it is sent (the credentials could not be read)
// and one that charged and then got a 429 on stripe-go's resend both leave
// the invoice unknown on the PaymentIntent it was stamped with. So neither
// needs a rule of its own: the unknown sweep reads that PaymentIntent, and
// either cancels it (the next charge pays once on a new one) or records its
// payment. Under the first form of this design these were the replayed-create
// double charges of review 1.
func TestTwoStep_ConfirmFails_ResolvedByReadingTheStampedPaymentIntent(t *testing.T) {
	t.Run("never sent: credentials unreadable", func(t *testing.T) {
		r := newSHRig(t, "TS Confirm Not Sent")
		r.rewire(&confirmFailer{fakeStripe: r.fake, confirmErrs: []error{ErrStripeCredentialsUnavailable}})
		inv := r.seed("INV-TS-NOTSENT")
		_, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1")
		if !IsUnknownPaymentFailure(err) {
			t.Fatalf("err = %v, want an unknown outcome", err)
		}
		piA := r.fake.onlyPI()
		if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != piA.id || piA.status != "requires_confirmation" {
			t.Fatalf("after the charge: %s on %q, PaymentIntent %s; want unknown on %s, unconfirmed", got.PaymentStatus, got.StripePaymentIntentID, piA.status, piA.id)
		}
		r.assertNoSecondCharge(inv.ID)
		r.ageUpdated(inv.ID, 6*time.Minute)
		r.runClean()
		if st := r.fake.pi(piA.id).status; st != "canceled" {
			t.Fatalf("the never-confirmed PaymentIntent is %s, want canceled", st)
		}
		if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
			t.Fatalf("the next charge: %v", err)
		}
		if ids := r.succeededFor(inv.ID); len(ids) != 1 || ids[0] == piA.id {
			t.Fatalf("succeeded PaymentIntents = %v, want one, and not the canceled %s", ids, piA.id)
		}
	})
	t.Run("charged, then a 429 on the resend", func(t *testing.T) {
		r := newSHRig(t, "TS Confirm 429")
		r.rewire(&confirmFailer{fakeStripe: r.fake, chargedThen: fmt.Errorf("%w: stripe rate limit: Request rate limit exceeded", ErrPaymentTransient)})
		inv := r.seed("INV-TS-429")
		_, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1")
		if !IsUnknownPaymentFailure(err) {
			t.Fatalf("err = %v, want an unknown outcome", err)
		}
		piA := r.fake.onlyPI()
		if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != piA.id || r.hold(inv.ID) == nil {
			t.Fatalf("after the charge: %s on %q hold %v; want unknown on %s, held", got.PaymentStatus, got.StripePaymentIntentID, r.hold(inv.ID), piA.id)
		}
		r.assertNoSecondCharge(inv.ID)
		r.ageUpdated(inv.ID, 6*time.Minute)
		r.runClean()
		r.assertPaidNowHoldClearedLater(inv.ID, piA.id)
		if ids := r.succeededFor(inv.ID); len(ids) != 1 {
			t.Fatalf("succeeded PaymentIntents = %v, want only %s", ids, piA.id)
		}
	})
}

// The reconciler's never-confirmed branch: if its cancel is refused (a late
// confirm went through), the invoice stays unknown on its payment and is then
// recorded paid.
func TestTwoStep_Reconciler_CancelRefused_DoesNotEndTheAttempt(t *testing.T) {
	r := newSHRig(t, "TS Reconciler Cancel Refused")
	inv := r.seed("INV-TS-RECCANCEL")
	r.fake.queueConfirm(fakeMode{kind: fakeConfirmNeverArrived})
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the unclear error")
	}
	pi := r.fake.onlyPI()
	var once sync.Once
	r.fake.afterRead = func() { once.Do(func() { r.fake.setStatus(pi.id, "succeeded") }) }
	r.ageUpdated(inv.ID, 5*time.Minute)
	_ = r.run() // the refused cancel is a reconciler error: expected
	r.fake.afterRead = nil

	if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != pi.id {
		t.Fatalf("after a refused cancel: %s on %q, want still unknown on %q", got.PaymentStatus, got.StripePaymentIntentID, pi.id)
	}
	r.assertNoSecondCharge(inv.ID)
	r.ageUpdated(inv.ID, 5*time.Minute)
	r.runClean()
	r.assertPaidNowHoldClearedLater(inv.ID, pi.id)
}

// A dunning retry whose confirm never reached Stripe goes back to 'failed' (the
// run retries it), with last_payment_error cleared and no email.
func TestTwoStep_ConfirmNeverArrived_DunningRetryGoesBackToFailed(t *testing.T) {
	r := newSHRig(t, "TS Never Arrived Dunning")
	inv := r.seed("INV-TS-NEVERDUN")
	r.exec(`UPDATE invoices SET payment_status = 'failed', last_payment_error = 'Your card was declined.' WHERE id = $1`, inv.ID)
	inv = r.get(inv.ID)
	r.fake.queueConfirm(fakeMode{kind: fakeConfirmNeverArrived})
	if _, err := r.chargeAs(r.ctx, dunningRetry, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the unclear error")
	}
	first := r.fake.onlyPI()
	r.ageUpdated(inv.ID, 5*time.Minute)
	r.runClean()
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentFailed || r.hold(inv.ID) != nil || got.LastPaymentError != "" {
		t.Fatalf("after the reconciler: %s hold %v error %q; want failed, no hold, no error text", got.PaymentStatus, r.hold(inv.ID), got.LastPaymentError)
	}
	if n := r.paymentFailedEmails(); n != 0 {
		t.Errorf("payment-failed emails = %d, want 0", n)
	}
	if _, err := r.chargeAs(r.ctx, dunningRetry, inv.ID, "pm_card_1"); err != nil {
		t.Fatalf("the dunning retry after: %v", err)
	}
	if got := r.get(inv.ID); got.Status != domain.InvoicePaid || got.StripePaymentIntentID == first.id || r.fake.countPIs() != 2 {
		t.Fatalf("after the retry: %s on %q with %d PaymentIntents, want paid on a new one", got.Status, got.StripePaymentIntentID, r.fake.countPIs())
	}
}

// An unclear charge Stripe can never be asked about (the key revoked) can be
// written off: the exit the sweep's error names.
func TestTwoStep_UnclearCharge_StripeRefusesForGood_WriteOffIsTheExit(t *testing.T) {
	r := newSHRig(t, "TS Unclear Write Off")
	inv := r.seed("INV-TS-UNCLEARWO")
	r.fake.queueConfirm(fakeMode{kind: fakeSave500})
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the 500")
	}
	r.fake.setCustomer(stripeCustomerOf(inv), "auth")
	r.ageHold(inv.ID, staleSweepAge)
	r.ageUpdated(inv.ID, 5*time.Minute)
	_ = r.run()
	if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentUnknown {
		t.Fatalf("setup: %s, want unknown (Stripe cannot be asked)", got.PaymentStatus)
	}
	if _, err := r.invSvc.MarkUncollectible(r.ctx, r.tenantID, inv.ID); err != nil {
		t.Fatalf("write-off of an unclear charge Stripe cannot be asked about: %v, want allowed", err)
	}
	// Stripe answers again later: the payment it took settles the write-off.
	r.fake.setCustomer(stripeCustomerOf(inv), "")
	r.ageUpdated(inv.ID, 5*time.Minute)
	r.runClean()
	if got := r.get(inv.ID); got.Status != domain.InvoicePaid {
		t.Fatalf("after Stripe answered: %s, want paid (uncollectible → paid)", got.Status)
	}
}

// The charge's PaymentIntent is not in the tenant's Stripe account any more
// (the account was switched): a refusal that will not heal. The invoice stays
// unknown on it, so nothing charges it again, the check is stamped so it
// rotates, and write-off is the exit.
func TestUnknown_PaymentIntentGoneFromAccount_KeptAndStamped(t *testing.T) {
	r := newSHRig(t, "SH PI Gone")
	inv := r.seed("INV-SH-PIGONE")
	charged := r.crashAfterCharge(inv, autoCharge, "")
	r.fake.mu.Lock()
	r.fake.pis = nil // the account Stripe reads no longer has it
	r.fake.mu.Unlock()
	r.ageHold(inv.ID, staleSweepAge)
	_ = r.run()
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != charged.id {
		t.Fatalf("after the check: %s on %q; want still unknown on %q", got.PaymentStatus, got.StripePaymentIntentID, charged.id)
	}
	if got.ProviderSyncedAt == nil {
		t.Error("the check was not stamped")
	}
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); !errors.Is(err, errNotClaimed) {
		t.Errorf("a charge on the unknown invoice: err = %v, want its claim refused", err)
	}
	if _, err := r.invSvc.MarkUncollectible(r.ctx, r.tenantID, inv.ID); err != nil {
		t.Fatalf("write-off: %v, want allowed (the exit when Stripe can never answer)", err)
	}
}

// Review 2, finding 2. The confirm charges, the reply is lost, and stripe-go's
// resend gets a 401 (the key rolled in those seconds). The hold is kept, so a
// charge on another card (a new key) cannot start; once the key works the
// sweep reads the attempt's PaymentIntent and records the payment. Main
// released the hold here and relied on the webhook.
func TestTwoStep_ConfirmKeyRefusedAfterCharge_HoldKept_NoSecondCharge(t *testing.T) {
	r := newSHRig(t, "TS Confirm 401")
	cf := &confirmFailer{fakeStripe: r.fake, chargedThen: &StripeKeyError{Reason: StripeKeyRejected, Message: "Invalid API Key provided"}}
	r.rewire(cf)
	inv := r.seed("INV-TS-401")
	_, err := r.chargeAs(r.ctx, collect, inv.ID, "pm_card_1")
	var ke *StripeKeyError
	if !errors.As(err, &ke) || !errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("charge 1: err = %v, want the key error (still transient)", err)
	}
	first := r.fake.onlyPI()
	if r.hold(inv.ID) == nil {
		t.Fatal("hold released on a 401 after the confirm: its first send may have charged")
	}
	if _, err := r.chargeAs(r.ctx, collect, inv.ID, "pm_card_2"); !errors.Is(err, errNotClaimed) {
		t.Fatalf("charge 2 on another card: err = %v, want its claim refused while the hold stands", err)
	}
	r.ageHold(inv.ID, staleSweepAge)
	r.runClean()
	if ids := r.succeededFor(inv.ID); len(ids) != 1 {
		t.Fatalf("succeeded PaymentIntents = %v, want only %s", ids, first.id)
	}
	r.assertPaidOnce(inv.ID, first.id)
}

// The same refusal on the create moves no money: the hold is released and
// the next charge goes ahead once the key works.
func TestTwoStep_CreateKeyRefused_HoldReleased(t *testing.T) {
	r := newSHRig(t, "TS Create 401")
	r.rewire(&createFailer{fakeStripe: r.fake, err: &StripeKeyError{Reason: StripeKeyRejected, Message: "Invalid API Key provided"}})
	inv := r.seed("INV-TS-C401")
	var ke *StripeKeyError
	if _, err := r.chargeAs(r.ctx, collect, inv.ID, "pm_card_1"); !errors.As(err, &ke) {
		t.Fatalf("err = %v, want the key error", err)
	}
	if r.hold(inv.ID) != nil {
		t.Fatal("hold kept on a create-time key refusal: no money can have moved, Collect would wait for nothing")
	}
	if _, err := r.chargeAs(r.ctx, collect, inv.ID, "pm_card_1"); err != nil {
		t.Fatalf("after the key was fixed: %v", err)
	}
}

// createFailer is the rig's fake Stripe with a create that fails once.
type createFailer struct {
	*fakeStripe
	mu  sync.Mutex
	err error
}

func (c *createFailer) CreateUnconfirmedPaymentIntent(ctx context.Context, p PaymentIntentParams) (PaymentIntentResult, error) {
	c.mu.Lock()
	err := c.err
	c.err = nil
	c.mu.Unlock()
	if err != nil {
		return PaymentIntentResult{}, err
	}
	return c.fakeStripe.CreateUnconfirmedPaymentIntent(ctx, p)
}

// upgradeCheck runs migration 00225's upgrade check against the test
// database, as the migration does before adding its columns, inside a
// transaction that is rolled back: setup runs first in it, so the test
// database (shared with other runs) is never changed.
func (r *shRig) upgradeCheck(setup ...string) error {
	r.t.Helper()
	raw, err := os.ReadFile("../platform/migrate/sql/00225_charge_hold_token.sql")
	if err != nil {
		r.t.Fatalf("read the migration: %v", err)
	}
	src := string(raw)
	i := strings.Index(src, "-- +goose StatementBegin")
	j := strings.Index(src, "-- +goose StatementEnd")
	if i < 0 || j < i {
		r.t.Fatal("the migration has no upgrade check block")
	}
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	for _, q := range setup {
		if _, err := tx.ExecContext(r.ctx, q); err != nil {
			r.t.Fatalf("setup %q: %v", q, err)
		}
	}
	_, err = tx.ExecContext(r.ctx, src[i+len("-- +goose StatementBegin"):j])
	return err
}

// The upgrade refuses to run over a charge hold or a parked charge the
// previous version left, on an open invoice or a written-off one, and runs
// once there are none. The previous version still settles those on a
// written-off invoice (it records a payment it finds, and ends a park); this
// version does not, so a written-off park that got past the check would stay
// parked for good, with its credit note drafts waiting.
func TestUpgradeCheck_RefusesEarlierVersionHoldsAndParks(t *testing.T) {
	r := newSHRig(t, "Upgrade Check")
	inv := r.seed("INV-UP-CHECK")
	none := `UPDATE invoices SET auto_charge_claimed_until = NULL,
		payment_status = CASE WHEN payment_status = 'unknown' AND COALESCE(stripe_payment_intent_id, '') = '' THEN 'pending' ELSE payment_status END`
	if err := r.upgradeCheck(none); err != nil {
		t.Fatalf("with nothing in flight: %v, want the upgrade to run", err)
	}
	for _, tc := range []struct{ name, set string }{
		{"a charge hold", `auto_charge_claimed_until = now() - interval '1 minute'`},
		{"a parked charge", `payment_status = 'unknown', stripe_payment_intent_id = NULL`},
	} {
		err := r.upgradeCheck(none, `UPDATE invoices SET `+tc.set+` WHERE id = '`+inv.ID+`'`)
		if err == nil || !strings.Contains(err.Error(), "Hardcount upgrade stopped: 1 open or written-off invoice(s)") {
			t.Errorf("with %s: err = %v, want the upgrade stopped", tc.name, err)
		}
		err = r.upgradeCheck(none, `UPDATE invoices SET `+tc.set+`, status = 'uncollectible' WHERE id = '`+inv.ID+`'`)
		if err == nil || !strings.Contains(err.Error(), "Hardcount upgrade stopped: 1 open or written-off invoice(s)") {
			t.Errorf("with %s on a written-off invoice: err = %v, want the upgrade stopped", tc.name, err)
		}
		// On a paid or voided invoice nothing is left to settle: it does not
		// block the upgrade, and this version clears its hold.
		for _, closed := range []string{"paid", "voided"} {
			if err := r.upgradeCheck(none, `UPDATE invoices SET `+tc.set+`, status = '`+closed+`' WHERE id = '`+inv.ID+`'`); err != nil {
				t.Errorf("with %s on a %s invoice: %v, want the upgrade to run", tc.name, closed, err)
			}
		}
	}
}

// The fail-safe behind the upgrade check: a hold without its token (an old
// version still running against the new schema), or a park, is never cleared
// on a guess while the invoice is open. Kept, stamped so it rotates, Stripe
// not read (there is no saved PaymentIntent to read), every charge blocked.
// On an invoice nothing can charge any more, a hold without its token is
// cleared.
func TestStaleHold_EarlierVersionHold_KeptForAPerson(t *testing.T) {
	r := newSHRig(t, "SH Earlier Version")
	inv := r.seed("INV-SH-EARLIER")
	paid := r.fake.addPI(stripeCustomerOf(inv), inv.ID, "", "succeeded", shDue) // the old version's one-call charge
	r.exec(`UPDATE invoices SET auto_charge_claimed_until = now() - interval '11 minutes', charge_hold_token = NULL WHERE id = $1`, inv.ID)
	reads := r.fake.readCount()
	r.runClean()
	if r.hold(inv.ID) == nil || r.get(inv.ID).ProviderSyncedAt == nil {
		t.Fatal("an earlier version's hold was cleared on a guess, or not stamped")
	}
	if n := r.fake.readCount() - reads; n != 0 {
		t.Errorf("Stripe reads = %d, want 0", n)
	}
	for _, kind := range []chargeKind{autoCharge, dunningRetry, collect} {
		if _, err := r.chargeAs(r.ctx, kind, inv.ID, "pm_card_1"); !errors.Is(err, errNotClaimed) {
			t.Fatalf("%s: err = %v, want its claim refused", kind, err)
		}
	}
	if ids := r.succeededFor(inv.ID); len(ids) != 1 || ids[0] != paid.id {
		t.Fatalf("succeeded PaymentIntents = %v, want only the old version's", ids)
	}

	closed := r.seed("INV-SH-EARLIER-VOID")
	r.exec(`UPDATE invoices SET status = 'voided', auto_charge_claimed_until = now() - interval '11 minutes', charge_hold_token = NULL WHERE id = $1`, closed.ID)
	r.runClean()
	if r.hold(closed.ID) != nil {
		t.Error("an earlier version's hold kept on a voided invoice: nothing can charge it")
	}
}

// Docs review, 2026-11-10. An unknown invoice whose PaymentIntent Stripe will
// not return is stamped when the read fails, so it moves to the back of the
// unknown sweep's queue instead of heading it every tick and starving newer
// unclear charges.
func TestReconcile_ReadFails_StampedSoTheQueueRotates(t *testing.T) {
	r := newSHRig(t, "Reconcile Read Fails")
	inv := r.seed("INV-RC-READFAIL")
	r.exec(`UPDATE invoices SET payment_status = 'unknown', stripe_payment_intent_id = 'pi_not_in_this_account',
		updated_at = now() - interval '10 minutes', provider_synced_at = NULL WHERE id = $1`, inv.ID)
	if errs := r.run(); len(errs) == 0 {
		t.Fatal("setup: want the reconciler's read to fail")
	}
	got := r.get(inv.ID)
	if got.ProviderSyncedAt == nil {
		t.Fatal("a failed read left the row unstamped: it heads the unknown sweep's queue every tick")
	}
	if got.PaymentStatus != domain.PaymentUnknown {
		t.Errorf("payment_status = %s, want still unknown", got.PaymentStatus)
	}
}

// Review 3, minor: a 429 on the create means at most an unconfirmed
// PaymentIntent exists, which can take nothing, so the hold is released at
// once (not kept about 15 minutes for the sweep), and the next charge gets the
// same PaymentIntent back under the same key and pays once.
func TestTwoStep_Create429_HoldReleased_NextChargeReusesThePaymentIntent(t *testing.T) {
	r := newSHRig(t, "TS Create 429")
	r.rewire(&createFailer{fakeStripe: r.fake, err: fmt.Errorf("%w: stripe rate limit: Request rate limit exceeded", ErrPaymentTransient)})
	inv := r.seed("INV-TS-C429")
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); !errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("err = %v, want transient", err)
	}
	if r.hold(inv.ID) != nil {
		t.Fatal("hold kept after a 429 on the create: no money can have moved")
	}
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
		t.Fatalf("the next charge: %v", err)
	}
	if ids := r.succeededFor(inv.ID); len(ids) != 1 {
		t.Fatalf("succeeded PaymentIntents = %v, want one", ids)
	}
}
