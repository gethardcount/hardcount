package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/paymentmethods"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// scriptedCharges is the Stripe side of the charge path: each
// CreatePaymentIntent takes the next scripted answer and records which card
// it was asked to charge.
type scriptedCharges struct {
	answers []scriptedCharge
	charged []string // PaymentMethodID of every charge Stripe received
	amounts []int64  // and its amount
}

type scriptedCharge struct {
	pi  string
	err error // a *payment.PaymentError for a decline; nil = succeeded
}

func (s *scriptedCharges) CreatePaymentIntent(_ context.Context, p payment.PaymentIntentParams) (payment.PaymentIntentResult, error) {
	s.charged = append(s.charged, p.PaymentMethodID)
	s.amounts = append(s.amounts, p.AmountCents)
	if len(s.charged) > len(s.answers) {
		return payment.PaymentIntentResult{}, errors.New("scriptedCharges: unexpected charge")
	}
	a := s.answers[len(s.charged)-1]
	if a.err != nil {
		return payment.PaymentIntentResult{}, a.err
	}
	return payment.PaymentIntentResult{ID: a.pi, Status: "succeeded"}, nil
}
func (s *scriptedCharges) CancelPaymentIntent(context.Context, string) error { return nil }
func (s *scriptedCharges) GetPaymentIntent(_ context.Context, id string) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{ID: id, Status: "requires_payment_method"}, nil
}
func (s *scriptedCharges) ListPaymentIntentsForCustomer(context.Context, string, time.Time) ([]payment.PaymentIntentResult, error) {
	return nil, nil
}

// stripeDeclined is the PaymentError classifyStripeError makes of Stripe's
// 402 card_error for this decline code (the payment package's
// TestChargeDecline_RecordsDeclineCodeAndCard decodes the real JSON body
// through stripe-go to prove that shape).
func stripeDeclined(pi, code, message string) error {
	return &payment.PaymentError{Message: message, DeclineCode: code, PaymentIntentID: pi}
}

type hardDeclineRig struct {
	t         *testing.T
	db        *postgres.DB
	ctx       context.Context
	tenantID  string
	customers *customer.PostgresStore
	invoices  *invoice.PostgresStore
	pmSvc     *paymentmethods.Service
	pmStripe  *pmCardStripe
	cards     *latestCardFetcher
	charges   *scriptedCharges
	charger   *payment.Stripe
	retrier   *paymentRetrierAdapter
	invSvc    *invoice.Service
}

func newHardDeclineRig(t *testing.T, name string) *hardDeclineRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	t.Cleanup(cancel)
	r := &hardDeclineRig{t: t, db: db, ctx: ctx, tenantID: testutil.CreateTestTenant(t, db, name)}
	r.customers = customer.NewPostgresStore(db)
	r.invoices = invoice.NewPostgresStore(db)
	r.pmStripe = &pmCardStripe{}
	r.pmSvc = paymentmethods.NewService(paymentmethods.NewPostgresStore(db), r.pmStripe, nil)
	setups := &compositePaymentSetupStore{customers: r.customers, pms: r.pmSvc}
	r.charges = &scriptedCharges{}
	// Production wiring (router.go), minus the dunning starter: these tests
	// drive the retry directly.
	r.charger = payment.NewStripe(r.charges, r.invoices, payment.NewPostgresWebhookStore(db), setups)
	// The card-saving webhooks, wired as router.go does: setup_intent.succeeded
	// and checkout.session.completed attach through the payment-methods
	// service; a SetupIntent with no Velox metadata is matched by its Stripe
	// customer.
	r.cards = &latestCardFetcher{}
	r.charger.SetCardFetcher(r.cards)
	r.charger.SetPaymentMethodAttacher(r.pmSvc)
	r.charger.SetCustomerResolver(&stripeCustomerResolverAdapter{customers: r.customers})
	r.retrier = &paymentRetrierAdapter{charger: r.charger, invoiceStore: r.invoices, paymentSetups: setups}
	r.invSvc = invoice.NewService(r.invoices, nil, nil)
	r.invSvc.SetPaymentMethodReader(setups)
	r.invSvc.SetCardRefusalReader(r.invoices)
	return r
}

// customerWithCard creates a customer with a Stripe customer and one card,
// which becomes the default (the first card always does).
func (r *hardDeclineRig) customerWithCard(key, pm string) string {
	r.t.Helper()
	c, err := r.customers.Create(r.ctx, r.tenantID, domain.Customer{ExternalID: "ext_" + key, DisplayName: key, Email: key + "@acme.test"})
	if err != nil {
		r.t.Fatalf("create customer: %v", err)
	}
	if err := r.customers.SetStripeCustomerID(r.ctx, r.tenantID, c.ID, "cus_stripe_"+key); err != nil {
		r.t.Fatalf("stripe customer id: %v", err)
	}
	r.addCard(c.ID, pm)
	return c.ID
}

// addCard records a card by the store's default rule, as every attach that
// is not a Velox setup flow does (a hosted-page payment, a SetupIntent Velox
// did not create): the first card, or one replacing a default the bank
// refused for good, becomes the default; any other is added beside.
func (r *hardDeclineRig) addCard(custID, pm string) paymentmethods.PaymentMethod {
	r.t.Helper()
	r.pmStripe.last4, r.pmStripe.fingerprint = "4242", "fp_"+pm
	got, err := r.pmSvc.AttachFromSetupIntent(r.ctx, r.tenantID, custID, pm, false)
	if err != nil {
		r.t.Fatalf("attach card %s: %v", pm, err)
	}
	return got
}

// setupIntentSucceeded delivers Stripe's setup_intent.succeeded for a card
// saved on the Stripe customer to the real webhook handler. metadata is the
// SetupIntent's own: a Velox setup session stamps velox_customer_id (and the
// flow's velox_purpose); nil is a SetupIntent Velox did not create.
func (r *hardDeclineRig) setupIntentSucceeded(evtID, stripeCustomerID, pm string, metadata map[string]string) {
	r.t.Helper()
	if metadata == nil {
		metadata = map[string]string{}
	}
	r.pmStripe.last4, r.pmStripe.fingerprint = "4242", "fp_"+pm
	body, _ := json.Marshal(map[string]any{"id": evtID, "object": "event", "type": "setup_intent.succeeded",
		"data": map[string]any{"object": map[string]any{
			"id": "seti_" + evtID, "object": "setup_intent", "status": "succeeded",
			"payment_method": pm, "customer": stripeCustomerID, "metadata": metadata,
		}}})
	if err := r.charger.HandleWebhook(r.ctx, r.tenantID, domain.StripeWebhookEvent{
		StripeEventID: evtID, EventType: "setup_intent.succeeded", Payload: map[string]any{"raw": string(body)},
	}); err != nil {
		r.t.Fatalf("setup_intent.succeeded %s: %v", evtID, err)
	}
}

// hostedPayCompleted delivers checkout.session.completed for a hosted-page
// payment that saved pm on the Stripe customer (setup_future_usage, ADR-021).
func (r *hardDeclineRig) hostedPayCompleted(evtID, custID, stripeCustomerID, pm string) {
	r.t.Helper()
	r.cards.card = payment.CardDetails{PaymentMethodID: pm, Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2034}
	r.pmStripe.last4, r.pmStripe.fingerprint = "4242", "fp_"+pm
	body, _ := json.Marshal(map[string]any{"id": evtID, "type": "checkout.session.completed", "data": map[string]any{"object": map[string]any{
		"id": "cs_" + evtID, "mode": "payment", "customer": stripeCustomerID,
		"metadata": map[string]string{"velox_customer_id": custID, "velox_purpose": payment.PurposeHostedInvoicePay},
	}}})
	if err := r.charger.HandleWebhook(r.ctx, r.tenantID, domain.StripeWebhookEvent{
		StripeEventID: evtID, EventType: "checkout.session.completed", Payload: map[string]any{"raw": string(body)},
	}); err != nil {
		r.t.Fatalf("checkout.session.completed %s: %v", evtID, err)
	}
}

// chargeAsCycle charges the invoice the way the cycle's collector does: under
// its own claim (ADR-127: a charge sends nothing without one).
func (r *hardDeclineRig) chargeAsCycle(inv domain.Invoice, stripeCustomerID, pm string) (domain.Invoice, error) {
	r.t.Helper()
	hold, err := r.invoices.ClaimChargeForManualCollect(r.ctx, r.tenantID, inv.ID)
	if err != nil || !hold.Held() {
		r.t.Fatalf("claim: held=%v err=%v", hold.Held(), err)
	}
	return r.charger.ChargeInvoice(r.ctx, r.tenantID, inv, hold, stripeCustomerID, pm)
}

func (r *hardDeclineRig) finalizedInvoice(custID, num string) domain.Invoice {
	r.t.Helper()
	inv, err := r.invoices.Create(r.ctx, r.tenantID, domain.Invoice{
		CustomerID: custID, InvoiceNumber: num, Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour), BillingPeriodEnd: time.Now().UTC(),
	})
	if err != nil {
		r.t.Fatalf("create invoice: %v", err)
	}
	return inv
}

func (r *hardDeclineRig) attentionCode(invID string) string {
	r.t.Helper()
	inv, err := r.invSvc.Get(r.ctx, r.tenantID, invID)
	if err != nil {
		r.t.Fatalf("get invoice: %v", err)
	}
	if inv.Attention == nil {
		return ""
	}
	return inv.Attention.Code
}

// assertOnlyDefault checks that the customer has exactly one active default
// card, and that it is wantPM.
func (r *hardDeclineRig) assertOnlyDefault(custID, wantPM string) {
	r.t.Helper()
	pms, err := r.pmSvc.List(r.ctx, r.tenantID, custID)
	if err != nil {
		r.t.Fatalf("list cards: %v", err)
	}
	var defaults []string
	for _, pm := range pms {
		if pm.IsDefault && pm.DetachedAt == nil {
			defaults = append(defaults, pm.StripePaymentMethodID)
		}
	}
	if len(defaults) != 1 || defaults[0] != wantPM {
		r.t.Fatalf("default cards = %v, want exactly [%s]", defaults, wantPM)
	}
}

func (r *hardDeclineRig) invoice(id string) domain.Invoice {
	r.t.Helper()
	inv, err := r.invoices.Get(r.ctx, r.tenantID, id)
	if err != nil {
		r.t.Fatalf("get invoice: %v", err)
	}
	return inv
}

// #966, real Postgres and production wiring: the cycle charge's card is
// reported lost. The decline lands on the attempt row with the card; every
// dunning retry then charges nothing while that card is the default. The
// customer then saves a new card through the update link: because the bank
// refused the default for good, the new card replaces it as the default
// (ADR-126; review of #1225 — before, it was added beside the refused card,
// every later retry skipped, and the run exhausted although the customer had
// done what they were asked). The notice returns to an ordinary decline and
// the next retry charges the new card.
func TestHardDecline_RetrySkipsTheRefusedCardUntilANewCardArrives_RealStore(t *testing.T) {
	r := newHardDeclineRig(t, "Hard Decline Retry")
	custID := r.customerWithCard("lost", "pm_lost")
	inv := r.finalizedInvoice(custID, "INV-HD-1")
	r.charges.answers = []scriptedCharge{
		{err: stripeDeclined("pi_lost_1", "lost_card", "Your card was declined.")},
		{pi: "pi_paid"},
	}

	// The cycle charge: Stripe refuses the card as lost.
	if _, err := r.chargeAsCycle(inv, "cus_stripe_lost", "pm_lost"); err == nil {
		t.Fatal("the lost-card charge returned no error")
	}
	if got := r.invoice(inv.ID); got.PaymentStatus != domain.PaymentFailed || got.StripePaymentIntentID != "pi_lost_1" {
		t.Fatalf("after the decline: %s / %q, want failed / pi_lost_1", got.PaymentStatus, got.StripePaymentIntentID)
	}
	attempts, err := r.invoices.ListChargeAttemptsByInvoice(r.ctx, r.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].DeclineCode != "lost_card" || attempts[0].StripePaymentMethodID != "pm_lost" {
		t.Fatalf("attempt rows = %+v, want one with decline lost_card on pm_lost", attempts)
	}
	if code := r.attentionCode(inv.ID); code != "payment.card_refused" {
		t.Errorf("attention %q, want payment.card_refused", code)
	}

	retry := func() error {
		t.Helper()
		return r.retrier.RetryPayment(r.ctx, r.tenantID, inv.ID, custID)
	}

	// Dunning retries skip the refused card. Twice in a row: the first skip
	// must release the charge lease, or the second would come back as an
	// uncounted transient skip instead.
	for i := 1; i <= 2; i++ {
		if err := retry(); !errors.Is(err, domain.ErrCardNeedsReplacing) {
			t.Fatalf("retry %d: %v, want ErrCardNeedsReplacing", i, err)
		}
	}
	if len(r.charges.charged) != 1 {
		t.Fatalf("Stripe charges %v, want only the first — the refused card must not be charged again", r.charges.charged)
	}

	// The customer saves a new card through the update link. The bank
	// refused the default for good, so the new card becomes the default
	// with no operator step: the notice returns to an ordinary decline
	// (Retry is useful again), and the next retry charges the new card.
	fresh := r.addCard(custID, "pm_new")
	if !fresh.IsDefault {
		t.Fatal("the new card was added beside the refused default; it must replace it as the default")
	}
	r.assertOnlyDefault(custID, "pm_new")
	// The hard decline is recorded on the old card only: it does not block
	// the new one.
	if code, err := r.invoices.RetryBlockingDecline(r.ctx, r.tenantID, inv.ID, "pm_new"); err != nil || code != "" {
		t.Fatalf("RetryBlockingDecline(pm_new) = %q, %v; want none — the refusal was on pm_lost", code, err)
	}
	if code := r.attentionCode(inv.ID); code != "payment.declined" {
		t.Errorf("attention after the card was replaced: %q, want payment.declined", code)
	}
	if err := retry(); err != nil {
		t.Fatalf("retry on the replaced card: %v", err)
	}
	if want := []string{"pm_lost", "pm_new"}; len(r.charges.charged) != 2 || r.charges.charged[1] != "pm_new" {
		t.Fatalf("Stripe charges %v, want %v", r.charges.charged, want)
	}
	if got := r.invoice(inv.ID); got.Status != domain.InvoicePaid {
		t.Errorf("invoice %s after the retry on the new card, want paid", got.Status)
	}
}

// Control: an ordinary decline does not stop the next retry from charging
// the same card. A card saved by a path that is not a Velox setup flow is
// added beside the working default and does not change which card the retry
// charges: a hosted-page payment's card (checkout.session.completed, which
// takes the newest card on the Stripe customer as a guess) and a SetupIntent
// Velox did not create (no velox metadata, matched by its Stripe customer).
func TestHardDecline_OrdinaryDeclineIsRetriedOnTheSameCard_RealStore(t *testing.T) {
	r := newHardDeclineRig(t, "Ordinary Decline Retry")
	custID := r.customerWithCard("nsf", "pm_nsf")
	inv := r.finalizedInvoice(custID, "INV-HD-2")
	r.charges.answers = []scriptedCharge{
		{err: stripeDeclined("pi_nsf_1", "insufficient_funds", "Your card has insufficient funds.")},
		{err: stripeDeclined("pi_nsf_2", "insufficient_funds", "Your card has insufficient funds.")},
	}

	_, _ = r.chargeAsCycle(inv, "cus_stripe_nsf", "pm_nsf")
	if code := r.attentionCode(inv.ID); code != "payment.declined" {
		t.Errorf("attention %q, want payment.declined", code)
	}
	r.hostedPayCompleted("evt_nsf_hosted", custID, "cus_stripe_nsf", "pm_hosted")
	r.assertOnlyDefault(custID, "pm_nsf")
	r.setupIntentSucceeded("evt_nsf_foreign", "cus_stripe_nsf", "pm_foreign", nil)
	r.assertOnlyDefault(custID, "pm_nsf")
	if pms, err := r.pmSvc.List(r.ctx, r.tenantID, custID); err != nil || len(pms) != 3 {
		t.Fatalf("cards on file = %d (%v), want 3: the default and the two saved beside it", len(pms), err)
	}

	err := r.retrier.RetryPayment(r.ctx, r.tenantID, inv.ID, custID)
	var re *dunning.RetryError
	if !errors.As(err, &re) || re.PaymentIntentID != "pi_nsf_2" || re.Ask != domain.DeclineAskNone {
		t.Fatalf("retry: %#v, want a counted decline on pi_nsf_2 that does not ask for a new card", err)
	}
	if len(r.charges.charged) != 2 || r.charges.charged[1] != "pm_nsf" {
		t.Errorf("Stripe charges %v, want the same card charged again", r.charges.charged)
	}
}

// E2/H3 (Stripe sandbox walk, 2026-10-22): after an ordinary decline the
// customer, or the operator for them, saves a new card through a Velox setup
// flow. That card becomes the default (ADR-021, amended), as the success
// page promises, and the next retry charges it. Before, it sat beside the
// declining card, and every retry charged the old card again. Driven through
// the real setup_intent.succeeded handler, once per setup flow: the
// operator's Add payment method link, the customer's payment-update link,
// and legacy POST /v1/checkout/setup (no purpose).
func TestSetupLinkCard_AfterOrdinaryDecline_NextRetryChargesIt_RealStore(t *testing.T) {
	for _, purpose := range []string{"portal_add_payment_method", "payment_update_token", ""} {
		name := purpose
		if name == "" {
			name = "legacy_no_purpose"
		}
		t.Run(name, func(t *testing.T) {
			r := newHardDeclineRig(t, "Setup Link Card "+name)
			custID := r.customerWithCard("e2", "pm_0341")
			inv := r.finalizedInvoice(custID, "INV-E2-1")
			r.charges.answers = []scriptedCharge{
				{err: stripeDeclined("pi_e2_1", "generic_decline", "Your card was declined.")},
				{pi: "pi_e2_paid"},
			}
			if _, err := r.chargeAsCycle(inv, "cus_stripe_e2", "pm_0341"); err == nil {
				t.Fatal("the declined charge returned no error")
			}
			if code := r.attentionCode(inv.ID); code != "payment.declined" {
				t.Errorf("attention %q, want payment.declined", code)
			}

			meta := map[string]string{"velox_customer_id": custID, "velox_tenant_id": r.tenantID}
			if purpose != "" {
				meta["velox_purpose"] = purpose
			}
			r.setupIntentSucceeded("evt_e2_"+name, "cus_stripe_e2", "pm_4242", meta)
			r.assertOnlyDefault(custID, "pm_4242")

			if err := r.retrier.RetryPayment(r.ctx, r.tenantID, inv.ID, custID); err != nil {
				t.Fatalf("retry on the new card: %v", err)
			}
			if len(r.charges.charged) != 2 || r.charges.charged[1] != "pm_4242" {
				t.Fatalf("Stripe charges %v, want the retry to charge pm_4242", r.charges.charged)
			}
			if got := r.invoice(inv.ID); got.Status != domain.InvoicePaid {
				t.Errorf("invoice %s after the retry on the new card, want paid", got.Status)
			}
		})
	}
}

// The decline reaches Velox only through the payment_intent.payment_failed
// webhook (the charge call's own report was lost). The webhook puts the
// decline code and card on record just the same, so the retry still skips
// the card, and a retry that itself draws a hard decline is flagged for the
// "use a different card" warning.
func TestHardDecline_WebhookReportStopsTheRetry_RealStore(t *testing.T) {
	r := newHardDeclineRig(t, "Hard Decline Webhook")
	custID := r.customerWithCard("stolen", "pm_stolen")
	inv := r.finalizedInvoice(custID, "INV-HD-3")

	body := `{"id": "evt_hd_3", "object": "event", "type": "payment_intent.payment_failed",
	  "data": {"object": {"id": "pi_stolen_1", "object": "payment_intent", "amount": 5000, "currency": "usd",
	    "last_payment_error": {"code": "card_declined", "decline_code": "stolen_card", "message": "Your card was declined.",
	      "payment_method": {"id": "pm_stolen", "object": "payment_method", "type": "card"}, "type": "card_error"},
	    "metadata": {"velox_invoice_id": "` + inv.ID + `", "velox_tenant_id": "` + r.tenantID + `"},
	    "payment_method": null, "status": "requires_payment_method"}}}`
	if err := r.charger.HandleWebhook(r.ctx, r.tenantID, domain.StripeWebhookEvent{
		StripeEventID: "evt_hd_3", EventType: "payment_intent.payment_failed", PaymentIntentID: "pi_stolen_1",
		InvoiceID: inv.ID, FailureMessage: "Your card was declined.", Payload: map[string]any{"raw": body},
	}); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if code, err := r.invoices.RetryBlockingDecline(r.ctx, r.tenantID, inv.ID, "pm_stolen"); err != nil || code != "stolen_card" {
		t.Fatalf("RetryBlockingDecline = %q, %v, want stolen_card", code, err)
	}
	if err := r.retrier.RetryPayment(r.ctx, r.tenantID, inv.ID, custID); !errors.Is(err, domain.ErrCardNeedsReplacing) {
		t.Fatalf("retry: %v, want ErrCardNeedsReplacing", err)
	}
	if len(r.charges.charged) != 0 {
		t.Errorf("Stripe charges %v, want none", r.charges.charged)
	}

	// A second card arrives and replaces the refused default by itself. The
	// bank refuses it too (picked up): that retry is a counted decline flagged
	// for the new-card warning.
	if second := r.addCard(custID, "pm_second"); !second.IsDefault {
		t.Fatal("the second card did not replace the refused default")
	}
	r.charges.answers = []scriptedCharge{{err: stripeDeclined("pi_pickup_1", "pickup_card", "Your card was declined.")}}
	err := r.retrier.RetryPayment(r.ctx, r.tenantID, inv.ID, custID)
	var re *dunning.RetryError
	if !errors.As(err, &re) || re.PaymentIntentID != "pi_pickup_1" || re.Ask != domain.DeclineAskNewCard {
		t.Fatalf("retry on the second card: %#v, want a counted pickup_card decline asking for a new card", err)
	}
	if err := r.retrier.RetryPayment(r.ctx, r.tenantID, inv.ID, custID); !errors.Is(err, domain.ErrCardNeedsReplacing) {
		t.Fatalf("next retry: %v, want ErrCardNeedsReplacing (the second card is refused too)", err)
	}
	if len(r.charges.charged) != 1 {
		t.Errorf("Stripe charges %v, want one (the second card, once)", r.charges.charged)
	}
}

// A card saved before the refusal, beside a working default, is attached again
// after the bank refuses the default for good (Stripe redelivers its
// setup_intent.succeeded, or the customer saves it again). The attach takes
// the store's update path, not its insert, and must still make it the default
// and leave the customer exactly one default — never none.
func TestHardDecline_CardAlreadySavedBesideReplacesTheRefusedDefault_RealStore(t *testing.T) {
	r := newHardDeclineRig(t, "Hard Decline Re-attach")
	custID := r.customerWithCard("revoked", "pm_revoked")
	if spare := r.addCard(custID, "pm_spare"); spare.IsDefault {
		t.Fatal("a second card beside a working default became the default")
	}
	inv := r.finalizedInvoice(custID, "INV-HD-4")
	r.charges.answers = []scriptedCharge{
		{err: stripeDeclined("pi_revoked_1", "revocation_of_authorization", "Your card was declined.")},
		{pi: "pi_spare_paid"},
	}
	if _, err := r.chargeAsCycle(inv, "cus_stripe_revoked", "pm_revoked"); err == nil {
		t.Fatal("the revoked-card charge returned no error")
	}
	// The refusal alone does not move the default: the card beside it was
	// saved before, and nothing about it changed.
	r.assertOnlyDefault(custID, "pm_revoked")

	if again := r.addCard(custID, "pm_spare"); !again.IsDefault {
		t.Fatal("re-attaching the spare card did not replace the refused default")
	}
	r.assertOnlyDefault(custID, "pm_spare")
	if err := r.retrier.RetryPayment(r.ctx, r.tenantID, inv.ID, custID); err != nil {
		t.Fatalf("retry on the spare card: %v", err)
	}
	if len(r.charges.charged) != 2 || r.charges.charged[1] != "pm_spare" {
		t.Errorf("Stripe charges %v, want the spare card charged second", r.charges.charged)
	}
}

// Review of #1360 (round 2): the payment-failed email for a card the bank
// refused for good must not tell the customer the card won't be charged
// again. Only dunning retries skip it (RetryBlockingDecline is per invoice
// and per retry): Collect payment on the same invoice, and the charge of the
// customer's next invoice, both charge that card, and each sends the email
// again. Real Postgres, the production outbox sender.
func TestHardDecline_PaymentFailedEmailDoesNotPromiseTheCardIsNotChargedAgain_RealStore(t *testing.T) {
	r := newHardDeclineRig(t, "Hard Decline Email Copy")
	r.charger.SetEmailPaymentFailed(email.NewOutboxSender(email.NewOutboxStore(r.db)), &customerEmailFetcherAdapter{store: r.customers})
	custID := r.customerWithCard("lostx", "pm_lostx")
	lost := func(pi string) scriptedCharge {
		return scriptedCharge{err: stripeDeclined(pi, "lost_card", "Your card was declined.")}
	}
	r.charges.answers = []scriptedCharge{lost("pi_x_1"), lost("pi_x_2"), lost("pi_x_3")}

	first := r.finalizedInvoice(custID, "INV-X-1")
	if _, err := r.chargeAsCycle(first, "cus_stripe_lostx", "pm_lostx"); err == nil {
		t.Fatal("the lost-card charge returned no error")
	}
	// Collect payment on the same invoice: the same claim and charge call.
	if _, err := r.chargeAsCycle(r.invoice(first.ID), "cus_stripe_lostx", "pm_lostx"); err == nil {
		t.Fatal("Collect on the lost card returned no error")
	}
	// The customer's next invoice charges the default card, still pm_lostx.
	second := r.finalizedInvoice(custID, "INV-X-2")
	if _, err := r.chargeAsCycle(second, "cus_stripe_lostx", "pm_lostx"); err == nil {
		t.Fatal("the next invoice's charge returned no error")
	}
	if want := []string{"pm_lostx", "pm_lostx", "pm_lostx"}; strings.Join(r.charges.charged, ",") != strings.Join(want, ",") {
		t.Fatalf("Stripe charges %v, want %v", r.charges.charged, want)
	}

	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(r.ctx, `SELECT COALESCE(payload->>'reason', '') FROM email_outbox
		WHERE tenant_id = $1 AND email_type = 'payment_failed' ORDER BY created_at, id`, r.tenantID)
	if err != nil {
		t.Fatalf("read email outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var reasons []string
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			t.Fatalf("scan email outbox: %v", err)
		}
		reasons = append(reasons, reason)
	}
	if len(reasons) != 3 {
		t.Fatalf("payment-failed emails %q, want one per charge", reasons)
	}
	for i, reason := range reasons {
		if !strings.Contains(reason, "different card") {
			t.Errorf("email %d reason %q does not ask for a different card", i+1, reason)
		}
		if strings.Contains(strings.ToLower(reason), "again") {
			t.Errorf("email %d reason %q says the card won't be tried again; Collect and the next invoice charge it", i+1, reason)
		}
	}
}
