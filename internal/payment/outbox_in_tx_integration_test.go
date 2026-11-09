package payment_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/paymentmethods"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// The four payment anomalies (payment.duplicate_charge, .amount_mismatch,
// .currency_mismatch and .received_on_voided_invoice) and the two
// payment_method.* events are queued in the transaction that writes their
// change (ADR-040): the anomaly marker on the invoice, or the card's
// payment_methods row. The change and its event commit together or not at
// all, and nothing after that commit has to run for the event to be sent.
//
// The tests below check three things on the real invoice store, webhook
// outbox and payment-methods service, wired as router.go wires them:
//   - each event is saved with its change, once, with the data its page in
//     docs/api/webhook-events.md promises;
//   - when the event's insert fails, its change is not saved either, and
//     the error reaches the caller, so Stripe or the reconciler tries again;
//   - one change reported twice, by a redelivery, by the reconciler or by
//     two deliveries at once, sends one event.

const pevDue = int64(5000)

var errEventInsertFailed = errors.New("webhook outbox insert failed: connection reset")

// pevOutbox is the real webhook outbox, except that inserts of failType
// fail, as a dropped connection does, until failOn("") heals it.
type pevOutbox struct {
	outbox   *webhook.OutboxStore
	mu       sync.Mutex
	failType string
}

func (o *pevOutbox) failOn(eventType string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failType = eventType
}

func (o *pevOutbox) fails(eventType string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.failType != "" && o.failType == eventType
}

func (o *pevOutbox) Enqueue(ctx context.Context, tx *sql.Tx, tenantID, eventType string, payload map[string]any) (string, error) {
	if o.fails(eventType) {
		return "", errEventInsertFailed
	}
	return o.outbox.Enqueue(ctx, tx, tenantID, eventType, payload)
}

// pevCards answers the adapter's card reads. The card a Checkout saved is
// savedPM. The card a payment was made with is unknown, so no card stamp
// writes the invoice after its settle commits and the invoice GET returns
// is the row the settle committed.
type pevCards struct{ savedPM string }

func (c *pevCards) FetchCardDetails(context.Context, string) (payment.CardDetails, error) {
	return payment.CardDetails{PaymentMethodID: c.savedPM, Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030}, nil
}

func (c *pevCards) FetchCardForPaymentIntent(context.Context, string) (payment.CardDetails, error) {
	return payment.CardDetails{}, nil
}

// pevPMStripe is the payment-methods service's Stripe: every card is a Visa
// 4242 whose fingerprint is its own, so no two cards are merged.
type pevPMStripe struct{}

func (pevPMStripe) CreateSetupCheckoutSession(context.Context, string, string, string, map[string]string) (string, string, error) {
	return "", "", errors.New("not used by these tests")
}

func (pevPMStripe) EnsureStripeCustomer(context.Context, string, string) (string, error) {
	return "", errors.New("not used by these tests")
}

func (pevPMStripe) DetachPaymentMethod(context.Context, string) error { return nil }

func (pevPMStripe) SetDefaultPaymentMethod(context.Context, string, string, string) error { return nil }

func (pevPMStripe) FetchPaymentMethodCard(_ context.Context, pmID string) (paymentmethods.CardMetadata, error) {
	return paymentmethods.CardMetadata{Brand: "visa", Last4: "4242", ExpMonth: 12, ExpYear: 2030, Fingerprint: "fp_" + pmID}, nil
}

const pevStripeCustomer = "cus_stripe_pev"

// pevRig is one tenant with one customer, and the payment adapter wired as
// router.go wires it.
type pevRig struct {
	db         *postgres.DB
	ctx        context.Context
	tenantID   string
	customerID string
	invoices   *invoice.PostgresStore
	pms        *paymentmethods.Service
	outbox     *pevOutbox
	cards      *pevCards
	stripe     *payment.Stripe
}

func newPEVRig(t *testing.T, db *postgres.DB, name string) *pevRig {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, name)
	customers := customer.NewPostgresStore(db)
	cust, err := customers.Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_" + name, DisplayName: name, Email: "ap@" + name + ".test",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	outbox := &pevOutbox{outbox: webhook.NewOutboxStore(db)}
	invoices := invoice.NewPostgresStore(db)
	invoices.SetOutboxEnqueuer(outbox)
	pmStore := paymentmethods.NewPostgresStore(db)
	pmStore.SetOutboxEnqueuer(outbox)
	pms := paymentmethods.NewService(pmStore, pevPMStripe{}, nil)
	cards := &pevCards{}

	s := payment.NewStripe(&settleRaceStripe{gets: map[string]int{}}, invoices, payment.NewPostgresWebhookStore(db),
		catchUpPaymentSetups{customers: customers, pms: pms})
	s.SetCardFetcher(cards)
	s.SetAnomalyRecorder(invoices)
	s.SetPaymentMethodAttacher(pms)

	return &pevRig{db: db, ctx: ctx, tenantID: tenantID, customerID: cust.ID, invoices: invoices,
		pms: pms, outbox: outbox, cards: cards, stripe: s}
}

// newInvoice creates a finalized invoice of pevDue, waiting for a card
// payment. It goes through a store with no outbox, so it queues no event.
func (r *pevRig) newInvoice(t *testing.T, number string) domain.Invoice {
	t.Helper()
	now := time.Now().UTC()
	inv, err := invoice.NewPostgresStore(r.db).CreateWithLineItems(r.ctx, r.tenantID, domain.Invoice{
		CustomerID: r.customerID, InvoiceNumber: number, Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, Currency: "USD", SubtotalCents: pevDue,
		TotalAmountCents: pevDue, AmountDueCents: pevDue,
		BillingPeriodStart: now.Add(-time.Hour), BillingPeriodEnd: now, IssuedAt: &now,
	}, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	return inv
}

func (r *pevRig) get(t *testing.T, invoiceID string) domain.Invoice {
	t.Helper()
	inv, err := r.invoices.Get(r.ctx, r.tenantID, invoiceID)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	return inv
}

// paid settles inv with piID for its full amount and returns the paid row.
func (r *pevRig) paid(t *testing.T, inv domain.Invoice, piID string) domain.Invoice {
	t.Helper()
	if err := r.stripe.SettleSucceeded(r.ctx, r.tenantID, inv, piID, pevDue, payment.SourceWebhook); err != nil {
		t.Fatalf("settle %s: %v", piID, err)
	}
	got := r.get(t, inv.ID)
	if got.Status != domain.InvoicePaid || got.StripePaymentIntentID != piID {
		t.Fatalf("invoice %s by %q after the settle, want paid by %s", got.Status, got.StripePaymentIntentID, piID)
	}
	return got
}

func (r *pevRig) listCards(t *testing.T) []paymentmethods.PaymentMethod {
	t.Helper()
	pms, err := r.pms.List(r.ctx, r.tenantID, r.customerID)
	if err != nil {
		t.Fatalf("list payment methods: %v", err)
	}
	return pms
}

// queued returns the data of each eventType row of the tenant, oldest first,
// read under its row-level security. For a payment.* event only the rows
// about invoiceID count.
func (r *pevRig) queued(t *testing.T, eventType, invoiceID string) []map[string]any {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(r.ctx, `SELECT payload FROM webhook_outbox
		WHERE event_type = $1 AND ($2 = '' OR payload->'invoice'->>'id' = $2)
		ORDER BY created_at, id`, eventType, invoiceID)
	if err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan outbox: %v", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatalf("decode outbox payload: %v", err)
		}
		out = append(out, data)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	return out
}

// pevData is v as a webhook receiver decodes it.
func pevData(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	enc, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(enc, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// oneEvent checks that exactly one eventType row was queued (about
// invoiceID, for a payment.* event) and that its data is want.
func (r *pevRig) oneEvent(t *testing.T, eventType, invoiceID string, want map[string]any) {
	t.Helper()
	got := r.queued(t, eventType, invoiceID)
	if len(got) != 1 {
		t.Errorf("%d %s rows, want 1: the change committed, so its event must be saved with it, once", len(got), eventType)
		return
	}
	if err := domain.CheckWebhookData(eventType, got[0]); err != nil {
		t.Error(err)
	}
	if w := pevData(t, want); !reflect.DeepEqual(got[0], w) {
		t.Errorf("%s data differs:\n got %v\nwant %v", eventType, got[0], w)
	}
}

func (r *pevRig) count(t *testing.T, eventType, invoiceID string) int {
	t.Helper()
	return len(r.queued(t, eventType, invoiceID))
}

// anomaly is the data of a payment anomaly event: the invoice and the
// charge that raised it.
func anomaly(inv domain.Invoice, piID string, captured int64, extra map[string]any) map[string]any {
	data := map[string]any{"incoming_payment_intent_id": piID, "captured_cents": captured}
	for k, v := range extra {
		data[k] = v
	}
	return domain.InvoiceWebhookData(inv, data)
}

func checkMarker(t *testing.T, inv domain.Invoice, kind, piID string, captured int64) {
	t.Helper()
	if inv.PaymentAnomalyKind != kind || inv.PaymentAnomalyPaymentIntentID != piID || inv.PaymentAnomalyCapturedCents != captured {
		t.Errorf("anomaly marker %q / %q / %d, want %q / %q / %d", inv.PaymentAnomalyKind, inv.PaymentAnomalyPaymentIntentID,
			inv.PaymentAnomalyCapturedCents, kind, piID, captured)
	}
}

// setupIntentSucceeded is Stripe's setup_intent.succeeded for a card saved
// through a Hardcount setup session for the rig's customer.
func (r *pevRig) setupIntentSucceeded(t *testing.T, evtID, pmID string) domain.StripeWebhookEvent {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id": evtID, "object": "event", "type": "setup_intent.succeeded",
		"data": map[string]any{"object": map[string]any{
			"id": "seti_" + evtID, "object": "setup_intent", "status": "succeeded",
			"payment_method": pmID, "customer": pevStripeCustomer,
			"metadata": map[string]any{"hardcount_customer_id": r.customerID},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	return domain.StripeWebhookEvent{
		StripeEventID: evtID, EventType: "setup_intent.succeeded", ObjectType: "setup_intent",
		Payload: map[string]any{"raw": string(raw)}, ReceivedAt: now, OccurredAt: now,
	}
}

// otherCurrencySucceeded is Stripe's payment_intent.succeeded for a charge
// of the invoice made in euros.
func otherCurrencySucceeded(evtID, piID, invoiceID string) domain.StripeWebhookEvent {
	ev := succeededEvent(evtID, piID, invoiceID)
	ev.Currency = "eur"
	return ev
}

func TestPaymentEvents_SavedWithTheirChange(t *testing.T) {
	db := testutil.SetupTestDB(t)

	t.Run("a second charge on a paid invoice", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-dup-paid")
		inv := r.paid(t, r.newInvoice(t, "INV-DUP-PAID"), "pi_first")
		if err := r.stripe.SettleSucceeded(r.ctx, r.tenantID, inv, "pi_second", pevDue, payment.SourceWebhook); err != nil {
			t.Fatalf("settle the second charge: %v", err)
		}
		// The invoice as the caller passed it: it names the first charge.
		r.oneEvent(t, domain.EventPaymentDuplicateCharge, inv.ID, anomaly(inv, "pi_second", pevDue, nil))
		checkMarker(t, r.get(t, inv.ID), domain.EventPaymentDuplicateCharge, "pi_second", pevDue)
	})

	t.Run("a second charge that lost the settle race", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-dup-race")
		unpaid := r.newInvoice(t, "INV-DUP-RACE")
		// Read before the first charge settled, as a racing delivery reads it.
		paid := r.paid(t, unpaid, "pi_first")
		if err := r.stripe.SettleSucceeded(r.ctx, r.tenantID, unpaid, "pi_second", pevDue, payment.SourceWebhook); err != nil {
			t.Fatalf("settle the second charge: %v", err)
		}
		// The invoice as the settle's row lock found it: paid by the first.
		r.oneEvent(t, domain.EventPaymentDuplicateCharge, unpaid.ID, anomaly(paid, "pi_second", pevDue, nil))
		checkMarker(t, r.get(t, unpaid.ID), domain.EventPaymentDuplicateCharge, "pi_second", pevDue)
	})

	t.Run("a payment on a voided invoice", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-voided")
		inv := r.newInvoice(t, "INV-VOIDED")
		if _, err := r.invoices.UpdateStatusWithReversalPrior(r.ctx, r.tenantID, inv.ID, domain.InvoiceVoided, nil); err != nil {
			t.Fatalf("void: %v", err)
		}
		voided := r.get(t, inv.ID)
		if err := r.stripe.SettleSucceeded(r.ctx, r.tenantID, voided, "pi_late", pevDue, payment.SourceWebhook); err != nil {
			t.Fatalf("settle on the voided invoice must be absorbed, got %v", err)
		}
		r.oneEvent(t, domain.EventPaymentReceivedOnVoidedInvoice, inv.ID, anomaly(voided, "pi_late", pevDue, nil))
		checkMarker(t, r.get(t, inv.ID), domain.EventPaymentReceivedOnVoidedInvoice, "pi_late", pevDue)
	})

	t.Run("a charge in another currency", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-currency")
		before := r.newInvoice(t, "INV-CURRENCY")
		if err := r.stripe.HandleWebhook(r.ctx, r.tenantID, otherCurrencySucceeded("evt_pev_eur", "pi_eur", before.ID)); err != nil {
			t.Fatalf("webhook: %v", err)
		}
		// The invoice as it was when the charge arrived; the payment then
		// settles as usual.
		r.oneEvent(t, domain.EventPaymentCurrencyMismatch, before.ID, anomaly(before, "pi_eur", pevDue, map[string]any{"charge_currency": "EUR"}))
		after := r.get(t, before.ID)
		if after.Status != domain.InvoicePaid {
			t.Errorf("invoice %s after the charge, want paid", after.Status)
		}
		checkMarker(t, after, domain.EventPaymentCurrencyMismatch, "pi_eur", pevDue)
	})

	t.Run("a charge for less than the invoice owed", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-short")
		inv := r.newInvoice(t, "INV-SHORT")
		if err := r.stripe.SettleSucceeded(r.ctx, r.tenantID, inv, "pi_short", pevDue-1000, payment.SourceWebhook); err != nil {
			t.Fatalf("settle: %v", err)
		}
		// One transaction pays the invoice and stamps the anomaly, so every
		// event it queues carries the row as it committed, marker included.
		after := r.get(t, inv.ID)
		if after.Status != domain.InvoicePaid || after.AmountPaidCents != pevDue {
			t.Errorf("invoice %s, paid %d; want paid, %d booked", after.Status, after.AmountPaidCents, pevDue)
		}
		checkMarker(t, after, domain.EventPaymentAmountMismatch, "pi_short", pevDue-1000)
		r.oneEvent(t, domain.EventPaymentAmountMismatch, inv.ID, anomaly(after, "pi_short", pevDue-1000, nil))
		r.oneEvent(t, domain.EventInvoicePaid, inv.ID, domain.InvoiceWebhookData(after, nil))
		r.oneEvent(t, domain.EventPaymentSucceeded, inv.ID, domain.InvoiceWebhookData(after, nil))
	})

	t.Run("a card saved by a setup session", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-attached")
		if err := r.stripe.HandleWebhook(r.ctx, r.tenantID, r.setupIntentSucceeded(t, "evt_pev_seti", "pm_saved")); err != nil {
			t.Fatalf("webhook: %v", err)
		}
		pms := r.listCards(t)
		if len(pms) != 1 || !pms[0].IsDefault {
			t.Fatalf("cards after the setup session: %+v, want one default", pms)
		}
		r.oneEvent(t, domain.EventPaymentMethodAttached, "",
			domain.PaymentMethodWebhookData(pms[0].View(), map[string]any{"stripe_payment_method_id": "pm_saved"}))
	})

	t.Run("a card saved by a Checkout", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-updated")
		r.cards.savedPM = "pm_checkout"
		if err := r.stripe.RecordCheckoutCard(r.ctx, r.tenantID, r.customerID, pevStripeCustomer, ""); err != nil {
			t.Fatalf("record the checkout's card: %v", err)
		}
		pms := r.listCards(t)
		if len(pms) != 1 {
			t.Fatalf("cards after the checkout: %+v, want one", pms)
		}
		r.oneEvent(t, domain.EventPaymentMethodUpdated, "",
			domain.PaymentMethodWebhookData(pms[0].View(), map[string]any{"stripe_customer_id": pevStripeCustomer}))
	})
}

func TestPaymentEvents_EventInsertFails_NothingIsSaved(t *testing.T) {
	db := testutil.SetupTestDB(t)

	t.Run("a second charge on a paid invoice", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-fail-dup")
		inv := r.paid(t, r.newInvoice(t, "INV-FAIL-DUP"), "pi_first")
		r.outbox.failOn(domain.EventPaymentDuplicateCharge)
		err := r.stripe.SettleSucceeded(r.ctx, r.tenantID, inv, "pi_second", pevDue, payment.SourceWebhook)
		if !errors.Is(err, errEventInsertFailed) {
			t.Errorf("settle: err %v, want the outbox error, so Stripe delivers the charge again", err)
		}
		checkMarker(t, r.get(t, inv.ID), "", "", 0)
		if n := r.count(t, domain.EventPaymentDuplicateCharge, inv.ID); n != 0 {
			t.Errorf("%d payment.duplicate_charge rows after the failed insert, want 0", n)
		}
		// The redelivery saves both.
		r.outbox.failOn("")
		if err := r.stripe.SettleSucceeded(r.ctx, r.tenantID, inv, "pi_second", pevDue, payment.SourceWebhook); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
		checkMarker(t, r.get(t, inv.ID), domain.EventPaymentDuplicateCharge, "pi_second", pevDue)
		r.oneEvent(t, domain.EventPaymentDuplicateCharge, inv.ID, anomaly(inv, "pi_second", pevDue, nil))
	})

	t.Run("a charge for less than the invoice owed", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-fail-short")
		inv := r.newInvoice(t, "INV-FAIL-SHORT")
		r.outbox.failOn(domain.EventPaymentAmountMismatch)
		err := r.stripe.SettleSucceeded(r.ctx, r.tenantID, inv, "pi_short", pevDue-1000, payment.SourceWebhook)
		if !errors.Is(err, errEventInsertFailed) {
			t.Errorf("settle: err %v, want the outbox error: the payment and its anomaly commit together or not at all", err)
		}
		after := r.get(t, inv.ID)
		if after.Status != domain.InvoiceFinalized {
			t.Errorf("invoice %s after the failed insert, want finalized: its payment.amount_mismatch was not saved, so neither is the payment", after.Status)
		}
		checkMarker(t, after, "", "", 0)
		for _, ev := range []string{domain.EventPaymentAmountMismatch, domain.EventInvoicePaid, domain.EventPaymentSucceeded} {
			if n := r.count(t, ev, inv.ID); n != 0 {
				t.Errorf("%d %s rows after the failed insert, want 0", n, ev)
			}
		}
		r.outbox.failOn("")
		if err := r.stripe.SettleSucceeded(r.ctx, r.tenantID, inv, "pi_short", pevDue-1000, payment.SourceWebhook); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
		after = r.get(t, inv.ID)
		r.oneEvent(t, domain.EventPaymentAmountMismatch, inv.ID, anomaly(after, "pi_short", pevDue-1000, nil))
		r.oneEvent(t, domain.EventInvoicePaid, inv.ID, domain.InvoiceWebhookData(after, nil))
	})

	t.Run("a charge in another currency", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-fail-currency")
		before := r.newInvoice(t, "INV-FAIL-CURRENCY")
		r.outbox.failOn(domain.EventPaymentCurrencyMismatch)
		err := r.stripe.HandleWebhook(r.ctx, r.tenantID, otherCurrencySucceeded("evt_pev_fail_eur", "pi_eur", before.ID))
		if !errors.Is(err, errEventInsertFailed) {
			t.Errorf("webhook: err %v, want the outbox error, so Stripe delivers the event again", err)
		}
		checkMarker(t, r.get(t, before.ID), "", "", 0)
		r.outbox.failOn("")
		if err := r.stripe.HandleWebhook(r.ctx, r.tenantID, otherCurrencySucceeded("evt_pev_fail_eur", "pi_eur", before.ID)); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
		r.oneEvent(t, domain.EventPaymentCurrencyMismatch, before.ID, anomaly(before, "pi_eur", pevDue, map[string]any{"charge_currency": "EUR"}))
		if got := r.get(t, before.ID); got.Status != domain.InvoicePaid {
			t.Errorf("invoice %s after the redelivery, want paid", got.Status)
		}
	})

	t.Run("a card saved by a setup session", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-fail-attached")
		r.outbox.failOn(domain.EventPaymentMethodAttached)
		ev := r.setupIntentSucceeded(t, "evt_pev_fail_seti", "pm_saved")
		if err := r.stripe.HandleWebhook(r.ctx, r.tenantID, ev); !errors.Is(err, errEventInsertFailed) {
			t.Errorf("webhook: err %v, want the outbox error, so Stripe delivers the event again", err)
		}
		if pms := r.listCards(t); len(pms) != 0 {
			t.Errorf("cards after the failed insert: %+v, want none: its payment_method.attached was not saved, so neither is the card", pms)
		}
		r.outbox.failOn("")
		if err := r.stripe.HandleWebhook(r.ctx, r.tenantID, ev); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
		pms := r.listCards(t)
		if len(pms) != 1 {
			t.Fatalf("cards after the redelivery: %+v, want one", pms)
		}
		r.oneEvent(t, domain.EventPaymentMethodAttached, "",
			domain.PaymentMethodWebhookData(pms[0].View(), map[string]any{"stripe_payment_method_id": "pm_saved"}))
	})
}

func TestPaymentEvents_SameChangeTwice_OneEvent(t *testing.T) {
	db := testutil.SetupTestDB(t)

	t.Run("a second charge seen by the webhook, then by the reconciler", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-twice-dup")
		inv := r.paid(t, r.newInvoice(t, "INV-TWICE-DUP"), "pi_first")
		if err := r.stripe.SettleSucceeded(r.ctx, r.tenantID, inv, "pi_second", pevDue, payment.SourceWebhook); err != nil {
			t.Fatalf("webhook: %v", err)
		}
		if err := r.stripe.SettleSucceeded(r.ctx, r.tenantID, r.get(t, inv.ID), "pi_second", pevDue, payment.SourceReconciler); err != nil {
			t.Fatalf("reconciler: %v", err)
		}
		r.oneEvent(t, domain.EventPaymentDuplicateCharge, inv.ID, anomaly(inv, "pi_second", pevDue, nil))
		// A third charge is another anomaly, with its own event.
		if err := r.stripe.SettleSucceeded(r.ctx, r.tenantID, r.get(t, inv.ID), "pi_third", pevDue, payment.SourceWebhook); err != nil {
			t.Fatalf("third charge: %v", err)
		}
		if n := r.count(t, domain.EventPaymentDuplicateCharge, inv.ID); n != 2 {
			t.Errorf("%d payment.duplicate_charge rows after a third charge, want 2: one per charge", n)
		}
	})

	t.Run("one second charge reported twice at once", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-race-dup")
		inv := r.paid(t, r.newInvoice(t, "INV-RACE-DUP"), "pi_first")
		settle := func() error {
			return r.stripe.SettleSucceeded(r.ctx, r.tenantID, inv, "pi_second", pevDue, payment.SourceWebhook)
		}
		for i, err := range race([]func() error{settle, settle}) {
			if err != nil {
				t.Errorf("racer %d: %v", i, err)
			}
		}
		r.oneEvent(t, domain.EventPaymentDuplicateCharge, inv.ID, anomaly(inv, "pi_second", pevDue, nil))
	})

	t.Run("a short charge delivered again", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-twice-short")
		inv := r.newInvoice(t, "INV-TWICE-SHORT")
		for i := range 2 {
			if err := r.stripe.SettleSucceeded(r.ctx, r.tenantID, inv, "pi_short", pevDue-1000, payment.SourceWebhook); err != nil {
				t.Fatalf("delivery %d: %v", i+1, err)
			}
		}
		if n := r.count(t, domain.EventPaymentAmountMismatch, inv.ID); n != 1 {
			t.Errorf("%d payment.amount_mismatch rows, want 1", n)
		}
	})

	t.Run("a setup intent delivered again", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-twice-seti")
		// Stripe sends the event again when it did not get the first
		// delivery's answer. To the handler that is a second event for the
		// same card.
		for _, evtID := range []string{"evt_pev_seti_1", "evt_pev_seti_2"} {
			if err := r.stripe.HandleWebhook(r.ctx, r.tenantID, r.setupIntentSucceeded(t, evtID, "pm_saved")); err != nil {
				t.Fatalf("webhook %s: %v", evtID, err)
			}
		}
		if n := r.count(t, domain.EventPaymentMethodAttached, ""); n != 1 {
			t.Errorf("%d payment_method.attached rows, want 1: the second delivery changed nothing", n)
		}
	})

	t.Run("a card added beside the default, recorded twice", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-twice-beside")
		if _, err := r.pms.AttachFromSetupIntent(r.ctx, r.tenantID, r.customerID, "pm_default", false); err != nil {
			t.Fatalf("attach the default card: %v", err)
		}
		// A hosted payment's card goes beside the working default, so the
		// "already recorded" check, which looks at the default, never stops
		// the second report.
		r.cards.savedPM = "pm_beside"
		for i := range 2 {
			if err := r.stripe.RecordCheckoutCard(r.ctx, r.tenantID, r.customerID, pevStripeCustomer, payment.PurposeHostedInvoicePay); err != nil {
				t.Fatalf("record %d: %v", i+1, err)
			}
		}
		if n := r.count(t, domain.EventPaymentMethodUpdated, ""); n != 1 {
			t.Errorf("%d payment_method.updated rows, want 1: the second report changed nothing", n)
		}
	})

	t.Run("a setup session's second event for its card", func(t *testing.T) {
		r := newPEVRig(t, db, "pev-session-pair")
		// One setup session sends checkout.session.completed and
		// setup_intent.succeeded for one card. Here the Checkout's is
		// handled first and saves the card as the customer's first, so the
		// default: the setup intent then changes nothing.
		r.cards.savedPM = "pm_session"
		if err := r.stripe.RecordCheckoutCard(r.ctx, r.tenantID, r.customerID, pevStripeCustomer, ""); err != nil {
			t.Fatalf("checkout: %v", err)
		}
		if err := r.stripe.HandleWebhook(r.ctx, r.tenantID, r.setupIntentSucceeded(t, "evt_pev_session", "pm_session")); err != nil {
			t.Fatalf("setup intent: %v", err)
		}
		if n := r.count(t, domain.EventPaymentMethodUpdated, ""); n != 1 {
			t.Errorf("%d payment_method.updated rows, want 1", n)
		}
		if n := r.count(t, domain.EventPaymentMethodAttached, ""); n != 0 {
			t.Errorf("%d payment_method.attached rows, want 0: the setup intent found the card saved as the default already", n)
		}
	})
}
