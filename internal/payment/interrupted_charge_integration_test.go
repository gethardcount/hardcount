package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// #976 on a real database (ADR-127). Each test stops a charge at one point —
// a crash before the request leaves, a crash after Stripe charged, the
// caller's deadline ending during the call — or gives Stripe's unclear
// answers, then checks two things: no path charges the invoice a second time
// while the charge record exists (counting the PaymentIntents at Stripe, not
// only the invoice's state), and the reconciler's interrupted-charge check
// ends it the way Stripe says. Only Stripe is faked (fakeStripe).

const icDue = int64(5000)

type icRig struct {
	t         *testing.T
	db        *postgres.DB
	ctx       context.Context
	tenantID  string
	invoices  *invoice.PostgresStore
	customers *customer.PostgresStore
	fake      *fakeStripe
	stripe    *Stripe
	rec       *Reconciler
	checkout  *CheckoutSessionStore
	dunning   *lockedDunningStarter
}

// lockedDunningStarter counts StartDunning calls safely across goroutines.
type lockedDunningStarter struct {
	mu    sync.Mutex
	calls []string
}

func (d *lockedDunningStarter) StartDunning(_ context.Context, _ string, invoiceID, _ string, _ time.Time, _ domain.DunningStartCause) (domain.InvoiceDunningRun, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, invoiceID)
	return domain.InvoiceDunningRun{ID: "drun_" + invoiceID, InvoiceID: invoiceID}, nil
}

func (d *lockedDunningStarter) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

type icEmails struct{ store *customer.PostgresStore }

func (r icEmails) GetCustomerEmail(ctx context.Context, tenantID, customerID string) (string, string, []string, error) {
	c, err := r.store.Get(ctx, tenantID, customerID)
	if err != nil {
		return "", "", nil, err
	}
	return c.Email, c.DisplayName, c.AdditionalEmails, nil
}

type icCards struct{}

func (icCards) FetchCardDetails(context.Context, string) (CardDetails, error) {
	return CardDetails{Brand: "visa", Last4: "4242"}, nil
}

func (icCards) FetchCardForPaymentIntent(context.Context, string) (CardDetails, error) {
	return CardDetails{Brand: "visa", Last4: "4242"}, nil
}

// newICRig wires the charge path and the reconciler as router.go does, on the
// real invoice store, webhook outbox and email outbox. wrap, when set, sits
// between the charge path and the store (a store write that fails).
func newICRig(t *testing.T, name string, wrap func(InvoiceUpdater) InvoiceUpdater) *icRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	r := &icRig{t: t, db: db, ctx: postgres.WithLivemode(context.Background(), false)}
	r.tenantID = testutil.CreateTestTenant(t, db, name)
	r.invoices = invoice.NewPostgresStore(db)
	outbox := webhook.NewOutboxStore(db)
	r.invoices.SetOutboxEnqueuer(outbox)
	r.customers = customer.NewPostgresStore(db)
	sender := email.NewOutboxSender(email.NewOutboxStore(db))
	emails := icEmails{store: r.customers}
	r.fake = newFakeStripe()
	r.dunning = &lockedDunningStarter{}

	var updater InvoiceUpdater = r.invoices
	if wrap != nil {
		updater = wrap(r.invoices)
	}
	r.stripe = NewStripe(r.fake, updater, NewPostgresWebhookStore(db), nil, r.dunning)
	r.stripe.SetCardFetcher(icCards{})
	r.stripe.SetEventDispatcher(webhook.NewOutboxDispatcher(outbox))
	r.stripe.SetAnomalyRecorder(r.invoices)
	r.stripe.SetEmailReceipt(sender, emails)
	r.stripe.SetEmailPaymentFailed(sender, emails)
	r.rec = NewReconciler(r.fake, r.invoices, time.Minute)
	r.rec.SetSettler(r.stripe)
	r.checkout = NewCheckoutSessionStore(db)
	return r
}

// seed creates a customer with an email and a finalized, unpaid invoice
// queued for collection: what every charge starts from.
func (r *icRig) seed(num string) domain.Invoice {
	r.t.Helper()
	cust, err := r.customers.Create(r.ctx, r.tenantID, domain.Customer{
		ExternalID: "ext_" + num, DisplayName: num, Email: "billing@" + num + ".test",
	})
	if err != nil {
		r.t.Fatalf("create customer: %v", err)
	}
	now := time.Now().UTC()
	inv, err := r.invoices.Create(r.ctx, r.tenantID, domain.Invoice{
		CustomerID: cust.ID, InvoiceNumber: num, Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, Currency: "USD", SubtotalCents: icDue,
		TotalAmountCents: icDue, AmountDueCents: icDue,
		BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	})
	if err != nil {
		r.t.Fatalf("create invoice: %v", err)
	}
	return inv
}

func stripeCustomerOf(inv domain.Invoice) string { return "cus_stripe_" + inv.InvoiceNumber }

type chargeKind string

const (
	autoCharge   chargeKind = "auto-charge"
	dunningRetry chargeKind = "dunning retry"
	collect      chargeKind = "Collect payment"
)

// errNotClaimed: the initiator's claim refused the invoice, so it charged
// nothing.
var errNotClaimed = errors.New("claim refused")

// chargeAs runs one charge initiator the way production does: take its claim
// on the invoice, re-read it, then charge the given card.
func (r *icRig) chargeAs(ctx context.Context, kind chargeKind, invoiceID, card string) (domain.Invoice, error) {
	var claimed bool
	var err error
	switch kind {
	case autoCharge:
		claimed, err = r.invoices.ClaimAutoCharge(r.ctx, r.tenantID, invoiceID)
	case dunningRetry:
		claimed, err = r.invoices.ClaimChargeForDunningRetry(r.ctx, r.tenantID, invoiceID)
	case collect:
		claimed, err = r.invoices.ClaimChargeForManualCollect(r.ctx, r.tenantID, invoiceID)
	}
	if err != nil {
		return domain.Invoice{}, fmt.Errorf("claim: %w", err)
	}
	if !claimed {
		return domain.Invoice{}, errNotClaimed
	}
	inv, err := r.invoices.Get(r.ctx, r.tenantID, invoiceID)
	if err != nil {
		return domain.Invoice{}, err
	}
	if kind == dunningRetry {
		return r.stripe.ChargeInvoiceForDunningRetry(ctx, r.tenantID, inv, stripeCustomerOf(inv), card)
	}
	return r.stripe.ChargeInvoice(ctx, r.tenantID, inv, stripeCustomerOf(inv), card)
}

// crash runs fn and reports whether it died at a fakeCrash: what a crash
// leaves committed is what the rest of the test sees.
func crash(fn func()) (crashed bool) {
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(fakeCrash); !ok {
				panic(p)
			}
			crashed = true
		}
	}()
	fn()
	return false
}

func (r *icRig) exec(q string, args ...any) {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(r.ctx, q, args...); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatal(err)
	}
}

func (r *icRig) count(q string, args ...any) int {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(r.ctx, q, args...).Scan(&n); err != nil {
		r.t.Fatalf("count %q: %v", q, err)
	}
	return n
}

type icRecord struct {
	set              bool
	startedAt        time.Time
	seq              int64
	key              string
	stripeCustomerID string
}

func (r *icRig) record(invoiceID string) icRecord {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var at sql.NullTime
	var seq sql.NullInt64
	var key, cust sql.NullString
	if err := tx.QueryRowContext(r.ctx, `SELECT charge_started_at, charge_started_seq, charge_started_key, charge_started_stripe_customer
		FROM invoices WHERE id = $1`, invoiceID).Scan(&at, &seq, &key, &cust); err != nil {
		r.t.Fatalf("read charge record: %v", err)
	}
	return icRecord{set: at.Valid, startedAt: at.Time, seq: seq.Int64, key: key.String, stripeCustomerID: cust.String}
}

func (r *icRig) get(invoiceID string) domain.Invoice {
	r.t.Helper()
	inv, err := r.invoices.Get(r.ctx, r.tenantID, invoiceID)
	if err != nil {
		r.t.Fatalf("get invoice: %v", err)
	}
	return inv
}

// lapseHold ends the 5-minute charge hold a crashed or timed-out charger left:
// the moment the invoice used to become chargeable again.
func (r *icRig) lapseHold(invoiceID string) {
	r.exec(`UPDATE invoices SET auto_charge_claimed_until = now() - interval '1 second' WHERE id = $1`, invoiceID)
}

// ageRecord moves the record's start back, so the check considers it, and
// forgets any earlier failed check.
func (r *icRig) ageRecord(invoiceID string, age time.Duration) {
	r.exec(`UPDATE invoices SET charge_started_at = now() - make_interval(secs => $2), provider_synced_at = NULL
		WHERE id = $1 AND charge_started_at IS NOT NULL`, invoiceID, age.Seconds())
}

func (r *icRig) runCheck() {
	r.t.Helper()
	if _, errs := r.rec.Run(r.ctx, 50); len(errs) > 0 {
		r.t.Fatalf("reconciler: %v", errs)
	}
}

// assertNoSecondCharge runs every way a second payment could start, with the
// customer's default card changed, and requires each to charge nothing: the
// auto-charge sweep, a dunning retry, Collect payment and the hosted page.
// It counts the PaymentIntents at Stripe, so a claim that wrongly let a
// charge through fails here even if the invoice state looked fine.
func (r *icRig) assertNoSecondCharge(invoiceID string) {
	r.t.Helper()
	before := r.fake.countPIs()
	for _, kind := range []chargeKind{autoCharge, dunningRetry, collect} {
		_, err := r.chargeAs(r.ctx, kind, invoiceID, "pm_card_changed")
		if !errors.Is(err, errNotClaimed) {
			r.t.Errorf("%s with a changed card: err = %v, want its claim refused while the charge record exists", kind, err)
		}
	}
	inv := r.get(invoiceID)
	if _, _, err := r.checkout.ClaimOpen(r.ctx, r.tenantID, invoiceID, inv.AmountDueCents, inv.Currency, false); !errors.Is(err, ErrChargeInFlight) {
		r.t.Errorf("hosted page claim: err = %v, want ErrChargeInFlight while the charge record exists", err)
		if err == nil {
			// The customer pays on the hosted page: Checkout's own PaymentIntent.
			r.fake.addPI(stripeCustomerOf(inv), invoiceID, "", "succeeded", inv.AmountDueCents)
		}
	}
	if after := r.fake.countPIs(); after != before {
		r.t.Errorf("PaymentIntents at Stripe went from %d to %d: a second payment started while the first may have charged", before, after)
	}
}

// assertPaidOnce checks everything one payment leaves behind, once.
func (r *icRig) assertPaidOnce(invoiceID, piID string) {
	r.t.Helper()
	got := r.get(invoiceID)
	if got.Status != domain.InvoicePaid || got.PaymentStatus != domain.PaymentSucceeded || got.StripePaymentIntentID != piID {
		r.t.Errorf("invoice %s/%s with PI %q, want paid/succeeded with %q", got.Status, got.PaymentStatus, got.StripePaymentIntentID, piID)
	}
	if got.AmountPaidCents != icDue {
		r.t.Errorf("amount_paid = %d, want %d", got.AmountPaidCents, icDue)
	}
	if rec := r.record(invoiceID); rec.set {
		r.t.Errorf("charge record still on a paid invoice: %+v", rec)
	}
	events := func(eventType string) int {
		return r.count(`SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2 AND payload->>'invoice_id' = $3`,
			r.tenantID, eventType, invoiceID)
	}
	if n := events(domain.EventPaymentSucceeded); n != 1 {
		r.t.Errorf("payment.succeeded rows = %d, want 1", n)
	}
	if n := events(domain.EventPaymentDuplicateCharge); n != 0 {
		r.t.Errorf("payment.duplicate_charge rows = %d, want 0", n)
	}
	if n := r.count(`SELECT count(*) FROM email_outbox WHERE tenant_id = $1 AND email_type = $2`, r.tenantID, email.TypePaymentReceipt); n != 1 {
		r.t.Errorf("payment receipts = %d, want 1", n)
	}
	if n := r.fake.countPIs(); n != 1 {
		r.t.Errorf("PaymentIntents at Stripe = %d, want exactly 1", n)
	}
}

// assertRecorded checks the record a charge wrote before calling Stripe.
func (r *icRig) assertRecorded(inv domain.Invoice, purpose, card string) icRecord {
	r.t.Helper()
	rec := r.record(inv.ID)
	if !rec.set {
		r.t.Fatalf("no charge record on the invoice: nothing says a charge was sent, so the invoice is chargeable again once the hold lapses")
	}
	if want := ChargeIdempotencyKey(inv.ID, inv.ChargeAttemptSeq, purpose, card); rec.key != want || rec.seq != inv.ChargeAttemptSeq {
		r.t.Errorf("record key %q seq %d, want %q seq %d", rec.key, rec.seq, want, inv.ChargeAttemptSeq)
	}
	if rec.stripeCustomerID != stripeCustomerOf(inv) {
		r.t.Errorf("record Stripe customer %q, want %q", rec.stripeCustomerID, stripeCustomerOf(inv))
	}
	return rec
}

// C4 by crash: Stripe took the money and the process died before the result
// was saved. None of the four ways to a second payment gets through — a
// changed card, another charge kind, a key Stripe no longer remembers after
// 24 hours, the hosted page — and the check records the payment Stripe took.
func TestInterruptedCharge_CrashAfterStripeCharged_NoSecondPayment(t *testing.T) {
	r := newICRig(t, "IC Crash After Charge", nil)
	inv := r.seed("INV-C4-CRASH")
	r.fake.queue(fakeMode{kind: fakeCrashAfterCreate})

	if !crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") }) {
		t.Fatal("the charge did not reach the crash point")
	}
	r.assertRecorded(inv, "", "pm_card_1")
	if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentPending || got.StripePaymentIntentID != "" {
		t.Fatalf("after the crash: %s with PI %q, want pending with none (nothing saved)", got.PaymentStatus, got.StripePaymentIntentID)
	}
	charged := r.fake.onlyPI()
	if charged.id == "" || charged.status != "succeeded" {
		t.Fatalf("setup: Stripe should hold exactly one succeeded PaymentIntent, has %d", r.fake.countPIs())
	}

	r.lapseHold(inv.ID)
	r.assertNoSecondCharge(inv.ID) // (i) changed card, (ii) other kinds, (iv) hosted page

	// (iii) Stripe forgets the key, and the record is 25 hours old.
	r.fake.prune()
	r.ageRecord(inv.ID, 25*time.Hour)
	r.assertNoSecondCharge(inv.ID)

	r.runCheck()
	r.assertPaidOnce(inv.ID, charged.id)
}

// C4 by timeout, far more common than a crash: the caller's deadline ends
// while Stripe is still working (a dunning retry's 15 s, auto-charge's 30 s,
// a Collect request whose browser went away). The invoice is neither failed
// nor parked; the record stays, nothing charges again, and the check records
// the payment.
func TestInterruptedCharge_CallerDeadlineEndsDuringTheCall(t *testing.T) {
	for _, tc := range []struct {
		kind    chargeKind
		purpose string
		cancel  bool // the request context is cancelled, not timed out
	}{
		{kind: dunningRetry, purpose: piPurposeDunningRetry},
		{kind: autoCharge},
		{kind: collect, cancel: true},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			r := newICRig(t, "IC Timeout "+string(tc.kind), nil)
			inv := r.seed("INV-C4-TIMEOUT")
			r.fake.queue(fakeMode{kind: fakeOutliveCtx})

			var ctx context.Context
			var cancel context.CancelFunc
			if tc.cancel {
				ctx, cancel = context.WithCancel(r.ctx)
				time.AfterFunc(300*time.Millisecond, cancel)
			} else {
				ctx, cancel = context.WithTimeout(r.ctx, 300*time.Millisecond)
			}
			defer cancel()
			_, err := r.chargeAs(ctx, tc.kind, inv.ID, "pm_card_1")
			var pe *PaymentError
			if !errors.As(err, &pe) || !pe.Unknown {
				t.Fatalf("err = %v, want an unclear outcome", err)
			}

			got := r.get(inv.ID)
			if got.PaymentStatus != domain.PaymentPending || got.StripePaymentIntentID != "" || got.ChargeAttemptSeq != inv.ChargeAttemptSeq {
				t.Errorf("after the timeout: %s with PI %q seq %d, want pending, none, %d: no stamp and no park",
					got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq, inv.ChargeAttemptSeq)
			}
			r.assertRecorded(inv, tc.purpose, "pm_card_1")
			charged := r.fake.onlyPI()
			if charged.id == "" {
				t.Fatalf("setup: the request outliving its context should have created one PaymentIntent, Stripe has %d", r.fake.countPIs())
			}

			r.lapseHold(inv.ID)
			r.assertNoSecondCharge(inv.ID)
			r.ageRecord(inv.ID, 11*time.Minute)
			r.runCheck()
			r.assertPaidOnce(inv.ID, charged.id)
		})
	}
}

// C2: the process died before the request left. The check finds nothing,
// releases the invoice and moves the counter, and the next charge goes out
// under a new key: one payment in all.
func TestInterruptedCharge_CrashBeforeSend_ReleasedAndChargedOnce(t *testing.T) {
	r := newICRig(t, "IC Crash Before Send", nil)
	inv := r.seed("INV-C2")
	r.fake.queue(fakeMode{kind: fakeCrashBeforeSend})

	if !crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") }) {
		t.Fatal("the charge did not reach the crash point")
	}
	r.assertRecorded(inv, "", "pm_card_1")
	r.lapseHold(inv.ID)
	r.assertNoSecondCharge(inv.ID)

	r.ageRecord(inv.ID, 11*time.Minute)
	r.runCheck()
	got := r.get(inv.ID)
	if rec := r.record(inv.ID); rec.set {
		t.Fatalf("record kept after Stripe was shown to have nothing: %+v", rec)
	}
	if got.PaymentStatus != domain.PaymentPending || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 {
		t.Fatalf("after the release: %s seq %d, want pending seq %d (a fresh key for the next charge)", got.PaymentStatus, got.ChargeAttemptSeq, inv.ChargeAttemptSeq+1)
	}

	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
		t.Fatalf("the charge after the release: %v", err)
	}
	pi := r.fake.onlyPI()
	if want := ChargeIdempotencyKey(inv.ID, inv.ChargeAttemptSeq+1, "", "pm_card_1"); pi.attemptKey != want {
		t.Errorf("the new charge's key = %q, want %q", pi.attemptKey, want)
	}
	r.assertPaidOnce(inv.ID, pi.id)
}

// Stripe saved a 500 for the key with no PaymentIntent (results are saved
// "including 500 errors"). The check finds nothing and releases; the next
// charge uses the next key, so it does not get the saved 500 back.
func TestInterruptedCharge_Saved500_NextChargeUsesTheNextKey(t *testing.T) {
	r := newICRig(t, "IC Saved 500", nil)
	inv := r.seed("INV-500")
	r.fake.queue(fakeMode{kind: fakeSave500})

	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the 500's error")
	}
	r.assertRecorded(inv, "", "pm_card_1")
	if n := r.fake.countPIs(); n != 0 {
		t.Fatalf("setup: Stripe should hold no PaymentIntent, has %d", n)
	}
	r.lapseHold(inv.ID)
	r.ageRecord(inv.ID, 11*time.Minute)
	r.runCheck()
	if got := r.get(inv.ID); got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 || r.record(inv.ID).set {
		t.Fatalf("after the check: seq %d record %v, want seq %d and no record", got.ChargeAttemptSeq, r.record(inv.ID).set, inv.ChargeAttemptSeq+1)
	}
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
		t.Fatalf("the charge under the next key got: %v (the saved 500 came back?)", err)
	}
	r.assertPaidOnce(inv.ID, r.fake.onlyPI().id)
}

// A network error, stripe-go's same-key resend, then 409 "key in use" while
// Stripe still runs the first request: not a decline, no dunning, the record
// stays, and the check finds the first request's PaymentIntent.
func TestInterruptedCharge_KeyInUse_FirstRequestFoundBytheCheck(t *testing.T) {
	r := newICRig(t, "IC Key In Use", nil)
	inv := r.seed("INV-409")
	r.fake.queue(fakeMode{kind: fakeKeyInUse})

	_, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1")
	var pe *PaymentError
	if !errors.As(err, &pe) || !pe.Unknown {
		t.Fatalf("err = %v, want an unclear outcome", err)
	}
	if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentPending {
		t.Fatalf("after 409 key in use: %s, want pending (not a decline, not parked)", got.PaymentStatus)
	}
	if n := r.dunning.count(); n != 0 {
		t.Fatalf("dunning started %d times on an unclear answer", n)
	}
	r.assertRecorded(inv, "", "pm_card_1")

	r.fake.completeInFlight() // Stripe finishes the first request: it charged.
	r.lapseHold(inv.ID)
	r.assertNoSecondCharge(inv.ID)
	r.ageRecord(inv.ID, 11*time.Minute)
	r.runCheck()
	r.assertPaidOnce(inv.ID, r.fake.onlyPI().id)
}

// A 429 keeps the record (it can answer a resend of a request Stripe still
// runs); when Stripe has nothing, the check releases.
func TestInterruptedCharge_RateLimited_KeptThenReleased(t *testing.T) {
	r := newICRig(t, "IC 429", nil)
	inv := r.seed("INV-429")
	r.fake.queue(fakeMode{kind: fakeRateLimit})

	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); !errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("err = %v, want ErrPaymentTransient", err)
	}
	r.assertRecorded(inv, "", "pm_card_1")
	r.lapseHold(inv.ID)
	r.assertNoSecondCharge(inv.ID)
	r.ageRecord(inv.ID, 11*time.Minute)
	r.runCheck()
	if got := r.get(inv.ID); r.record(inv.ID).set || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 {
		t.Fatalf("after the check: record %v seq %d, want released with seq %d", r.record(inv.ID).set, got.ChargeAttemptSeq, inv.ChargeAttemptSeq+1)
	}
}

// declineSaveFails fails the store's failure report while failing is set.
type declineSaveFails struct {
	InvoiceUpdater
	mu      sync.Mutex
	failing bool
}

func (d *declineSaveFails) setFailing(v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failing = v
}

func (d *declineSaveFails) MarkPaymentFailedReportingTransition(ctx context.Context, tenantID, id string, report domain.PaymentFailureReport, then func(tx *sql.Tx, fresh domain.Invoice) error) (domain.Invoice, domain.PaymentFailureResult, error) {
	d.mu.Lock()
	failing := d.failing
	d.mu.Unlock()
	if failing {
		return domain.Invoice{}, domain.PaymentFailureResult{}, errors.New("connection reset by peer")
	}
	return d.InvoiceUpdater.MarkPaymentFailedReportingTransition(ctx, tenantID, id, report, then)
}

// A decline whose save failed keeps the record (before, the invoice could be
// left with nobody collecting it). The check saves the decline through the
// same code the webhook uses, and dunning starts.
func TestInterruptedCharge_DeclineSaveFailed_CheckSavesItAndDunningStarts(t *testing.T) {
	var wrapper *declineSaveFails
	r := newICRig(t, "IC Decline Save Failed", func(u InvoiceUpdater) InvoiceUpdater {
		wrapper = &declineSaveFails{InvoiceUpdater: u, failing: true}
		return wrapper
	})
	inv := r.seed("INV-DECLINE")
	r.fake.queue(fakeMode{kind: fakeDecline})

	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err == nil {
		t.Fatal("want the decline's error")
	}
	if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentPending {
		t.Fatalf("setup: the decline should not be saved, invoice is %s", got.PaymentStatus)
	}
	r.assertRecorded(inv, "", "pm_card_1")
	wrapper.setFailing(false)

	r.ageRecord(inv.ID, 11*time.Minute)
	r.runCheck()
	declined := r.fake.onlyPI()
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentFailed || got.StripePaymentIntentID != declined.id {
		t.Fatalf("after the check: %s with PI %q, want failed with %q", got.PaymentStatus, got.StripePaymentIntentID, declined.id)
	}
	if r.record(inv.ID).set {
		t.Error("record kept after the decline was saved")
	}
	if n := r.dunning.count(); n != 1 {
		t.Errorf("dunning started %d times, want 1", n)
	}
	if n := r.count(`SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2`, r.tenantID, domain.EventPaymentFailed); n != 1 {
		t.Errorf("payment.failed rows = %d, want 1", n)
	}
}

// What the check does with a PaymentIntent it finds that has not ended.
func TestInterruptedCharge_FoundStillRunning(t *testing.T) {
	t.Run("processing, invoice at the record's counter: stamped processing", func(t *testing.T) {
		r := newICRig(t, "IC Found Processing", nil)
		inv := r.seed("INV-FOUND-PROC")
		r.fake.queue(fakeMode{kind: fakeCrashAfterCreate, status: "processing"})
		crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") })
		r.ageRecord(inv.ID, 11*time.Minute)
		r.runCheck()
		pi := r.fake.onlyPI()
		if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentProcessing || got.StripePaymentIntentID != pi.id {
			t.Fatalf("after the check: %s with PI %q, want processing with %q", got.PaymentStatus, got.StripePaymentIntentID, pi.id)
		}
		if r.record(inv.ID).set {
			t.Error("record kept after the PaymentIntent was saved on the invoice")
		}
	})

	t.Run("requires_action on an invoice that moved on: canceled at Stripe, cleared", func(t *testing.T) {
		r := newICRig(t, "IC Orphan Requires Action", nil)
		inv := r.seed("INV-ORPHAN")
		r.fake.queue(fakeMode{kind: fakeCrashAfterCreate, status: "requires_action"})
		crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") })
		orphan := r.fake.onlyPI()
		// An older attempt's late success settles the invoice meanwhile.
		if _, _, err := r.invoices.MarkPaidCardSettlementTransition(r.ctx, r.tenantID, inv.ID, "pi_older_attempt", time.Now().UTC(), nil); err != nil {
			t.Fatal(err)
		}
		r.ageRecord(inv.ID, 11*time.Minute)
		r.runCheck()
		if got := r.fake.pi(orphan.id); got.status != "canceled" {
			t.Errorf("orphaned PaymentIntent is %q at Stripe, want canceled (it never ends on its own)", got.status)
		}
		if r.record(inv.ID).set {
			t.Error("record kept after the orphan was canceled")
		}
	})

	t.Run("processing on an invoice that moved on: kept until it ends", func(t *testing.T) {
		r := newICRig(t, "IC Processing Moved On", nil)
		inv := r.seed("INV-PROC-MOVED")
		r.fake.queue(fakeMode{kind: fakeCrashAfterCreate, status: "processing"})
		crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") })
		if _, _, err := r.invoices.MarkPaidCardSettlementTransition(r.ctx, r.tenantID, inv.ID, "pi_older_attempt", time.Now().UTC(), nil); err != nil {
			t.Fatal(err)
		}
		r.ageRecord(inv.ID, 11*time.Minute)
		r.runCheck()
		if !r.record(inv.ID).set {
			t.Error("record cleared while its PaymentIntent is still processing at Stripe")
		}
		if len(r.fake.cancels) != 0 {
			t.Errorf("canceled %v: a processing PaymentIntent always ends, so the check waits", r.fake.cancels)
		}
	})
}

// A declined PaymentIntent the check finds is saved like the webhook's decline.
func TestInterruptedCharge_FoundDeclined_SavedAndDunningStarts(t *testing.T) {
	r := newICRig(t, "IC Found Declined", nil)
	inv := r.seed("INV-FOUND-DECLINED")
	r.fake.queue(fakeMode{kind: fakeCrashAfterCreate, status: "requires_payment_method"})
	crash(func() { _, _ = r.chargeAs(r.ctx, dunningRetry, inv.ID, "pm_card_1") })
	r.ageRecord(inv.ID, 11*time.Minute)
	r.runCheck()
	pi := r.fake.onlyPI()
	if got := r.get(inv.ID); got.PaymentStatus != domain.PaymentFailed || got.StripePaymentIntentID != pi.id {
		t.Fatalf("after the check: %s with PI %q, want failed with %q", got.PaymentStatus, got.StripePaymentIntentID, pi.id)
	}
	if n := r.dunning.count(); n != 1 {
		t.Errorf("dunning started %d times, want 1", n)
	}
	// A dunning retry's decline skips the generic payment-failed email.
	if n := r.count(`SELECT count(*) FROM email_outbox WHERE tenant_id = $1 AND email_type = $2`, r.tenantID, email.TypePaymentFailed); n != 0 {
		t.Errorf("payment-failed emails = %d, want 0 for a dunning retry", n)
	}
	if r.record(inv.ID).set {
		t.Error("record kept after the decline was saved")
	}
}

// endFails fails clearing the record while failing is set: C5, the result is
// saved and the record is not cleared.
type endFails struct {
	InvoiceUpdater
	mu      sync.Mutex
	failing bool
}

func (e *endFails) EndChargeAttempt(ctx context.Context, tenantID, id string, seq int64) (bool, error) {
	e.mu.Lock()
	failing := e.failing
	e.mu.Unlock()
	if failing {
		return false, errors.New("connection reset by peer")
	}
	return e.InvoiceUpdater.EndChargeAttempt(ctx, tenantID, id, seq)
}

// C5: the result is saved but the record was not cleared. The check finds the
// payment already recorded and only clears: one receipt, one event.
func TestInterruptedCharge_ResultSavedRecordLeft_CheckOnlyClears(t *testing.T) {
	var wrapper *endFails
	r := newICRig(t, "IC C5", func(u InvoiceUpdater) InvoiceUpdater {
		wrapper = &endFails{InvoiceUpdater: u, failing: true}
		return wrapper
	})
	inv := r.seed("INV-C5")
	if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
		t.Fatalf("charge: %v", err)
	}
	if !r.record(inv.ID).set {
		t.Fatal("setup: the record should be left when clearing fails")
	}
	r.ageRecord(inv.ID, 11*time.Minute)
	r.runCheck()
	r.assertPaidOnce(inv.ID, r.fake.onlyPI().id)
}

// Stripe cannot be read: the record stays and the invoice is not charged.
// After 24 hours of failed reads the invoice parks (ADR-107), and no path
// charges it.
func TestInterruptedCharge_FailedReads_KeepThenParkAfter24Hours(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(f *fakeStripe, stripeCustomer string)
	}{
		{"an error part-way through the pages", func(f *fakeStripe, _ string) { f.listPageFails = true }},
		{"the customer is missing (another Stripe account)", func(f *fakeStripe, c string) { f.customers[c] = "missing" }},
		{"the customer is deleted", func(f *fakeStripe, c string) { f.customers[c] = "deleted" }},
		{"Stripe disconnected for this mode", func(f *fakeStripe, _ string) { f.notConfigured = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newICRig(t, "IC Failed Read", nil)
			inv := r.seed("INV-READ")
			r.fake.queue(fakeMode{kind: fakeCrashAfterCreate})
			crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") })
			r.fake.mu.Lock()
			tc.break_(r.fake, stripeCustomerOf(inv))
			r.fake.mu.Unlock()
			r.lapseHold(inv.ID)

			r.ageRecord(inv.ID, 11*time.Minute)
			r.runCheck()
			got := r.get(inv.ID)
			if !r.record(inv.ID).set || got.PaymentStatus != domain.PaymentPending || got.ChargeAttemptSeq != inv.ChargeAttemptSeq {
				t.Fatalf("after a failed read: record %v, %s seq %d; want the record kept and the invoice unchanged (a failed read is never 'not found')",
					r.record(inv.ID).set, got.PaymentStatus, got.ChargeAttemptSeq)
			}
			if got.ProviderSyncedAt == nil {
				t.Error("the failed check was not stamped (provider_synced_at), so the next run asks again at once")
			}
			r.assertNoSecondCharge(inv.ID)

			r.ageRecord(inv.ID, 25*time.Hour)
			r.runCheck()
			got = r.get(inv.ID)
			if got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != "" || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 {
				t.Fatalf("after 24 hours of failed reads: %s with PI %q seq %d, want parked (unknown, no PaymentIntent, seq %d)",
					got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq, inv.ChargeAttemptSeq+1)
			}
			if r.record(inv.ID).set {
				t.Error("record kept on a parked invoice")
			}
			r.assertNoSecondCharge(inv.ID)
		})
	}
}

// The check matches on the exact key alone. Another attempt's PaymentIntent
// for the same invoice (same velox_invoice_id) is not this attempt's.
func TestInterruptedCharge_MatchesTheExactKeyOnly(t *testing.T) {
	r := newICRig(t, "IC Key Match", nil)
	inv := r.seed("INV-MATCH")
	older := ChargeIdempotencyKey(inv.ID, inv.ChargeAttemptSeq, piPurposeDunningRetry, "pm_card_0")
	r.fake.addPI(stripeCustomerOf(inv), inv.ID, older, "canceled", icDue)
	r.fake.queue(fakeMode{kind: fakeCrashBeforeSend})
	crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") })

	r.ageRecord(inv.ID, 11*time.Minute)
	r.runCheck()
	got := r.get(inv.ID)
	if got.PaymentStatus != domain.PaymentPending || r.record(inv.ID).set || got.ChargeAttemptSeq != inv.ChargeAttemptSeq+1 {
		t.Fatalf("after the check: %s, record %v, seq %d; want released (pending, seq %d): another attempt's canceled PaymentIntent is not this attempt's decline",
			got.PaymentStatus, r.record(inv.ID).set, got.ChargeAttemptSeq, inv.ChargeAttemptSeq+1)
	}
	if n := r.dunning.count(); n != 0 {
		t.Errorf("dunning started %d times", n)
	}
}

// A payment the check finds on an invoice already paid another way is a
// duplicate, flagged for the operator, never hidden as "already settled".
func TestInterruptedCharge_FoundSucceededOnPaidInvoice_FlagsTheDuplicate(t *testing.T) {
	r := newICRig(t, "IC Duplicate", nil)
	inv := r.seed("INV-DUP")
	r.fake.queue(fakeMode{kind: fakeCrashAfterCreate})
	crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") })
	// An older attempt's late success settled it meanwhile (residual 1).
	if _, _, err := r.invoices.MarkPaidCardSettlementTransition(r.ctx, r.tenantID, inv.ID, "pi_older_attempt", time.Now().UTC(), nil); err != nil {
		t.Fatal(err)
	}
	r.ageRecord(inv.ID, 11*time.Minute)
	r.runCheck()
	if n := r.count(`SELECT count(*) FROM webhook_outbox WHERE tenant_id = $1 AND event_type = $2 AND payload->>'invoice_id' = $3`,
		r.tenantID, domain.EventPaymentDuplicateCharge, inv.ID); n != 1 {
		t.Errorf("payment.duplicate_charge rows = %d, want 1: the second capture must reach the operator", n)
	}
	if got := r.get(inv.ID); got.PaymentAnomalyKind != domain.EventPaymentDuplicateCharge {
		t.Errorf("anomaly marker = %q, want %q", got.PaymentAnomalyKind, domain.EventPaymentDuplicateCharge)
	}
	if r.record(inv.ID).set {
		t.Error("record kept after the check reported the duplicate")
	}
}

// A charger that stalled until its hold lapsed does not call Stripe.
func TestInterruptedCharge_StalledCharger_StripeNotCalled(t *testing.T) {
	r := newICRig(t, "IC Stalled", nil)
	inv := r.seed("INV-STALL")
	ok, err := r.invoices.ClaimAutoCharge(r.ctx, r.tenantID, inv.ID)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	r.lapseHold(inv.ID) // the charger stalls past five minutes here
	if _, err := r.stripe.ChargeInvoice(r.ctx, r.tenantID, r.get(inv.ID), stripeCustomerOf(inv), "pm_card_1"); !errors.Is(err, ErrPaymentTransient) {
		t.Fatalf("err = %v, want ErrPaymentTransient", err)
	}
	if r.fake.creates != 0 {
		t.Fatalf("Stripe called %d times by a charger that no longer holds the invoice", r.fake.creates)
	}
	if r.record(inv.ID).set {
		t.Error("a record was written without the hold")
	}
}

// Concurrency, on real Postgres with goroutines (§7.3 of the design).
func TestInterruptedCharge_Concurrency(t *testing.T) {
	t.Run("every initiator races a recorded invoice: none charges", func(t *testing.T) {
		r := newICRig(t, "IC Race Claims", nil)
		inv := r.seed("INV-RACE-CLAIMS")
		r.fake.queue(fakeMode{kind: fakeCrashAfterCreate})
		crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") })
		r.lapseHold(inv.ID)

		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 8)
		kinds := []chargeKind{autoCharge, dunningRetry, collect, autoCharge, dunningRetry, collect}
		for i, k := range kinds {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = r.chargeAs(r.ctx, k, inv.ID, "pm_card_changed")
			}()
		}
		for i := len(kinds); i < len(errs); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _, errs[i] = r.checkout.ClaimOpen(r.ctx, r.tenantID, inv.ID, icDue, "USD", false)
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if i < len(kinds) && !errors.Is(err, errNotClaimed) {
				t.Errorf("%s: err = %v, want its claim refused", kinds[i], err)
			}
			if i >= len(kinds) && !errors.Is(err, ErrChargeInFlight) {
				t.Errorf("hosted claim: err = %v, want ErrChargeInFlight", err)
			}
		}
		if n := r.fake.countPIs(); n != 1 {
			t.Fatalf("PaymentIntents at Stripe = %d, want 1", n)
		}
	})

	t.Run("the check's release races the initiators: exactly one charge, after the release", func(t *testing.T) {
		r := newICRig(t, "IC Race Release", nil)
		inv := r.seed("INV-RACE-RELEASE")
		r.fake.queue(fakeMode{kind: fakeCrashBeforeSend})
		crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") })
		r.lapseHold(inv.ID)
		r.ageRecord(inv.ID, 11*time.Minute)

		var wg sync.WaitGroup
		start := make(chan struct{})
		var runErrs []error
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, runErrs = r.rec.Run(r.ctx, 50)
		}()
		for _, k := range []chargeKind{autoCharge, collect, autoCharge, collect} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _ = r.chargeAs(r.ctx, k, inv.ID, "pm_card_1")
			}()
		}
		close(start)
		wg.Wait()
		if len(runErrs) > 0 {
			t.Fatalf("reconciler: %v", runErrs)
		}
		// Whoever lost the race tries once more, as the next tick would.
		if r.get(inv.ID).PaymentStatus != domain.PaymentSucceeded {
			if _, err := r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1"); err != nil {
				t.Fatalf("the charge after the release: %v", err)
			}
		}
		pi := r.fake.onlyPI()
		if pi.id == "" {
			t.Fatalf("PaymentIntents at Stripe = %d, want exactly 1", r.fake.countPIs())
		}
		if want := ChargeIdempotencyKey(inv.ID, inv.ChargeAttemptSeq+1, "", "pm_card_1"); pi.attemptKey != want {
			t.Errorf("the one charge used key %q, want %q (sent only after the release moved the counter)", pi.attemptKey, want)
		}
		r.assertPaidOnce(inv.ID, pi.id)
	})

	t.Run("a webhook settles the payment during the check's list: one paid transition", func(t *testing.T) {
		r := newICRig(t, "IC Race Webhook", nil)
		inv := r.seed("INV-RACE-WEBHOOK")
		r.fake.queue(fakeMode{kind: fakeCrashAfterCreate})
		crash(func() { _, _ = r.chargeAs(r.ctx, autoCharge, inv.ID, "pm_card_1") })
		charged := r.fake.onlyPI()
		var webhookErr error
		var wg sync.WaitGroup
		r.fake.beforeList = func() {
			wg.Add(1)
			go func() {
				defer wg.Done()
				amt := icDue
				webhookErr = r.stripe.HandleWebhook(r.ctx, r.tenantID, domain.StripeWebhookEvent{
					StripeEventID: "evt_" + charged.id, EventType: "payment_intent.succeeded", ObjectType: "payment_intent",
					InvoiceID: inv.ID, PaymentIntentID: charged.id, PaymentStatus: "succeeded",
					AmountCents: &amt, Currency: "usd", Payload: map[string]any{},
					ReceivedAt: time.Now().UTC(), OccurredAt: time.Now().UTC(),
				})
			}()
		}
		r.ageRecord(inv.ID, 11*time.Minute)
		r.runCheck()
		wg.Wait()
		if webhookErr != nil {
			t.Fatalf("webhook: %v", webhookErr)
		}
		r.assertPaidOnce(inv.ID, charged.id)
	})
}
