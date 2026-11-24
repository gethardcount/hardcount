package payment_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// A top-up (billing_reason 'topup') is a draft until it is paid: each payment
// path issues it, marks it paid and grants its credit in the settle's one
// transaction (issueAndPayTopupLockedTx). These tests drive the payment paths
// on the real stores, wired as wire.go wires them.

const (
	topupPrice = int64(9000)
	topupGrant = int64(10000)
)

// topupStripe answers like Stripe for the saved-card charge: the confirm
// succeeds, or declines the card, and it may move the server's clock while
// the call is out.
type topupStripe struct {
	mu      sync.Mutex
	decline bool
	during  func()
	params  []payment.PaymentIntentParams
}

func (f *topupStripe) CreateUnconfirmedPaymentIntent(_ context.Context, p payment.PaymentIntentParams) (payment.PaymentIntentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.params = append(f.params, p)
	return payment.PaymentIntentResult{ID: "pi_topup_charge", Status: "requires_confirmation"}, nil
}

func (f *topupStripe) ConfirmPaymentIntent(_ context.Context, id string, _ payment.PaymentIntentParams) (payment.PaymentIntentResult, error) {
	if f.during != nil {
		f.during()
	}
	if f.decline {
		return payment.PaymentIntentResult{}, &payment.PaymentError{Message: "Your card was declined.", DeclineCode: "generic_decline",
			PaymentIntentID: id, Card: true, CardMessage: "Your card was declined."}
	}
	return payment.PaymentIntentResult{ID: id, Status: "succeeded"}, nil
}

func (f *topupStripe) CancelPaymentIntent(context.Context, string) error { return nil }

func (f *topupStripe) GetPaymentIntent(_ context.Context, id string) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{ID: id, Status: "succeeded"}, nil
}

type topupRig struct {
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	custID   string
	invStore *invoice.PostgresStore
	invSvc   *invoice.Service
	settings *tenant.SettingsStore
	client   *topupStripe
	stripe   *payment.Stripe
}

func newTopupRig(t *testing.T, name string) *topupRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	outbox := webhook.NewOutboxStore(db)
	invStore := invoice.NewPostgresStore(db)
	invStore.SetOutboxEnqueuer(outbox)
	creditStore := credit.NewPostgresStore(db)
	creditStore.SetOutboxEnqueuer(outbox)
	invStore.SetCommitFunder(credit.NewService(creditStore))
	settings := tenant.NewSettingsStore(db)
	invStore.SetInvoiceNumberer(settings)
	customers := customer.NewPostgresStore(db)
	invSvc := invoice.NewService(invStore, nil, settings)
	invSvc.SetCustomerReader(customers)

	tenantID := testutil.CreateTestTenant(t, db, name)
	cust, err := customers.Create(ctx, tenantID, domain.Customer{ExternalID: "cus_topup", DisplayName: name, Email: "billing@topup.test"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	client := &topupStripe{}
	sender := email.NewOutboxSender(email.NewOutboxStore(db))
	s := payment.NewStripe(client, invStore, payment.NewPostgresWebhookStore(db), nil)
	s.SetAnomalyRecorder(invStore)
	s.SetEmailReceipt(sender, settleRaceEmails{store: customers})
	s.SetEmailPaymentFailed(sender, settleRaceEmails{store: customers})
	return &topupRig{db: db, ctx: ctx, tenantID: tenantID, custID: cust.ID, invStore: invStore,
		invSvc: invSvc, settings: settings, client: client, stripe: s}
}

// create makes a top-up the way POST /v1/invoices does: one commit line,
// nothing else.
func (r *topupRig) create(t *testing.T) domain.Invoice {
	t.Helper()
	grant := topupGrant
	inv, err := r.invSvc.Create(r.ctx, r.tenantID, invoice.CreateInput{
		CustomerID: r.custID, Currency: "USD",
		LineItems: []invoice.AddLineItemInput{{Description: "Prepaid credit", Quantity: 1, UnitAmountCents: topupPrice, CommitGrantedCents: &grant}},
	})
	if err != nil {
		t.Fatalf("create top-up: %v", err)
	}
	return inv
}

// setValidity sets the tenant's topup_credit_valid_days (nil: never expires).
func (r *topupRig) setValidity(t *testing.T, days *int) {
	t.Helper()
	if _, err := r.settings.Update(r.ctx, r.tenantID, func(ts *domain.TenantSettings) error {
		ts.TopupCreditValidDays = days
		return nil
	}); err != nil {
		t.Fatalf("set validity: %v", err)
	}
}

// pay delivers payment_intent.succeeded for a hosted Checkout payment of
// captured cents.
func (r *topupRig) pay(t *testing.T, invID, evtID, piID string, captured int64) error {
	t.Helper()
	return r.stripe.HandleWebhook(r.ctx, r.tenantID, domain.StripeWebhookEvent{
		StripeEventID: evtID, EventType: "payment_intent.succeeded", ObjectType: "payment_intent",
		InvoiceID: invID, PaymentIntentID: piID, PaymentStatus: "succeeded",
		AmountCents: &captured, Currency: "usd", Payload: map[string]any{},
		ReceivedAt: time.Now().UTC(), OccurredAt: time.Now().UTC(),
	})
}

func (r *topupRig) get(t *testing.T, id string) domain.Invoice {
	t.Helper()
	inv, err := r.invStore.Get(r.ctx, r.tenantID, id)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	return inv
}

type topupGrantRow struct {
	amount    int64
	createdAt time.Time
	expiresAt sql.NullTime
}

// grants lists the commit grants the invoice funded.
func (r *topupRig) grants(t *testing.T, invID string) []topupGrantRow {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(r.ctx, `SELECT amount_cents, created_at, expires_at FROM customer_credit_ledger
		WHERE source_invoice_id = $1 AND grant_kind = 'commit'`, invID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []topupGrantRow
	for rows.Next() {
		var g topupGrantRow
		if err := rows.Scan(&g.amount, &g.createdAt, &g.expiresAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, g)
	}
	return out
}

func (r *topupRig) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(r.ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

// charge is the saved-card sale (POST /v1/invoices/{id}/collect): the
// manual-collect claim, then the charge.
func (r *topupRig) charge(t *testing.T, inv domain.Invoice) (domain.Invoice, error) {
	t.Helper()
	hold, err := r.invStore.ClaimChargeForManualCollect(r.ctx, r.tenantID, inv.ID)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !hold.Held() {
		t.Fatal("the manual-collect claim refused the top-up")
	}
	return r.stripe.ChargeInvoiceNow(r.ctx, r.tenantID, inv, hold, "cus_stripe_topup", "pm_topup")
}

// failingFunder makes the grant fail, to show the issue and the paid flip
// roll back with it.
type failingFunder struct{}

func (failingFunder) LockCustomerCreditTx(context.Context, *sql.Tx, string, string) error { return nil }

func (failingFunder) GrantCommitForInvoiceTx(context.Context, *sql.Tx, string, string, string, string, string, int64, *time.Time) (domain.CreditLedgerEntry, error) {
	return domain.CreditLedgerEntry{}, errors.New("ledger unavailable")
}

// The hosted page's payment issues the top-up, pays it and grants its credit
// together: a grant that fails leaves it an unnumbered draft and the webhook
// unacknowledged.
func TestTopup_PaymentSucceeded_IssuesPaysAndGrantsInOneTransaction(t *testing.T) {
	r := newTopupRig(t, "topup-webhook")
	draft := r.create(t)
	if draft.InvoiceNumber != "" || draft.Status != domain.InvoiceDraft || draft.PublicToken == "" {
		t.Fatalf("created %q %s token=%t, want an unnumbered draft with its pay link", draft.InvoiceNumber, draft.Status, draft.PublicToken != "")
	}

	// A grant that fails rolls the whole payment back, and the webhook is
	// not acknowledged, so Stripe delivers it again.
	r.invStore.SetCommitFunder(failingFunder{})
	if err := r.pay(t, draft.ID, "evt_topup_1", "pi_topup_1", topupPrice); err == nil {
		t.Fatal("payment with a failing grant: nil error, want the webhook retried")
	}
	if got := r.get(t, draft.ID); got.Status != domain.InvoiceDraft || got.InvoiceNumber != "" || got.PaymentAnomalyKind != "" {
		t.Fatalf("after a failed grant: %s %q anomaly %q, want an unnumbered draft and no anomaly", got.Status, got.InvoiceNumber, got.PaymentAnomalyKind)
	}

	r.invStore.SetCommitFunder(credit.NewService(credit.NewPostgresStore(r.db)))
	if err := r.pay(t, draft.ID, "evt_topup_1", "pi_topup_1", topupPrice); err != nil {
		t.Fatalf("payment: %v", err)
	}
	paid := r.get(t, draft.ID)
	if paid.Status != domain.InvoicePaid || paid.InvoiceNumber == "" || paid.AmountPaidCents != topupPrice {
		t.Fatalf("after payment: %s %q paid %d, want paid, numbered, %d", paid.Status, paid.InvoiceNumber, paid.AmountPaidCents, topupPrice)
	}
	if paid.IssuedAt == nil || paid.PaidAt == nil || !paid.IssuedAt.Equal(*paid.PaidAt) || paid.DueAt != nil {
		t.Errorf("issued_at %v paid_at %v due_at %v, want issued at the payment and no due date", paid.IssuedAt, paid.PaidAt, paid.DueAt)
	}
	g := r.grants(t, draft.ID)
	if len(g) != 1 || g[0].amount != topupGrant || g[0].expiresAt.Valid {
		t.Fatalf("grants %+v, want one of %d with no expiry", g, topupGrant)
	}
	for _, ev := range []string{domain.EventInvoiceFinalized, domain.EventInvoicePaid} {
		if n := r.count(t, `SELECT count(*) FROM webhook_outbox WHERE event_type = $1 AND payload->'invoice'->>'id' = $2`, ev, draft.ID); n != 1 {
			t.Errorf("%s events = %d, want 1", ev, n)
		}
	}
}

// The saved-card sale charges the draft; a decline leaves it a draft to
// charge again, and the customer, who owes nothing, gets no email.
func TestTopup_SavedCard_SuccessIssuesAndDeclineLeavesADraftWithNoEmail(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		r := newTopupRig(t, "topup-card-ok")
		paid, err := r.charge(t, r.create(t))
		if err != nil {
			t.Fatalf("charge: %v", err)
		}
		if paid.Status != domain.InvoicePaid || paid.InvoiceNumber == "" || len(r.grants(t, paid.ID)) != 1 {
			t.Fatalf("after the charge: %s %q, want paid, numbered and granted once", paid.Status, paid.InvoiceNumber)
		}
		p := r.client.params[0]
		if p.Metadata["hardcount_purpose"] != "topup" || p.Description != "Prepaid credit" || p.AmountCents != topupPrice {
			t.Errorf("PaymentIntent purpose %q description %q amount %d, want topup, Prepaid credit, %d",
				p.Metadata["hardcount_purpose"], p.Description, p.AmountCents, topupPrice)
		}
	})
	t.Run("decline", func(t *testing.T) {
		r := newTopupRig(t, "topup-card-declined")
		r.client.decline = true
		draft := r.create(t)
		if _, err := r.charge(t, draft); err == nil {
			t.Fatal("declined charge: nil error")
		}
		got := r.get(t, draft.ID)
		if got.Status != domain.InvoiceDraft || got.InvoiceNumber != "" || got.PaymentStatus != domain.PaymentFailed {
			t.Fatalf("after the decline: %s %q %s, want an unnumbered draft, payment failed", got.Status, got.InvoiceNumber, got.PaymentStatus)
		}
		if len(r.grants(t, draft.ID)) != 0 {
			t.Error("a declined top-up granted credit")
		}
		if n := r.count(t, `SELECT count(*) FROM email_outbox WHERE tenant_id = $1 AND email_type = $2`, r.tenantID, email.TypePaymentFailed); n != 0 {
			t.Errorf("payment-failed emails = %d, want 0: a top-up owes nothing until it is paid", n)
		}
	})
}

// A second payment finds the top-up paid: one number, one grant, and the
// extra money reported for a refund.
func TestTopup_PaidTwice_SecondIsADuplicateCharge(t *testing.T) {
	r := newTopupRig(t, "topup-twice")
	draft := r.create(t)
	if err := r.pay(t, draft.ID, "evt_first", "pi_first", topupPrice); err != nil {
		t.Fatalf("first payment: %v", err)
	}
	number := r.get(t, draft.ID).InvoiceNumber
	if err := r.pay(t, draft.ID, "evt_second", "pi_second", topupPrice); err != nil {
		t.Fatalf("second payment: %v", err)
	}
	got := r.get(t, draft.ID)
	if got.PaymentAnomalyKind != domain.EventPaymentDuplicateCharge || got.InvoiceNumber != number || got.StripePaymentIntentID != "pi_first" {
		t.Errorf("anomaly %q number %q PI %q, want duplicate_charge, %q, pi_first", got.PaymentAnomalyKind, got.InvoiceNumber, number, got.StripePaymentIntentID)
	}
	if n := len(r.grants(t, draft.ID)); n != 1 {
		t.Errorf("grants = %d, want 1", n)
	}
}

// A withdrawn top-up stays unnumbered, and a payment that still lands is
// reported for a refund and grants nothing.
func TestTopup_PaidAfterVoid_ReceivedOnVoidedInvoice(t *testing.T) {
	r := newTopupRig(t, "topup-void")
	draft := r.create(t)
	if _, err := r.invSvc.Void(r.ctx, r.tenantID, draft.ID); err != nil {
		t.Fatalf("void: %v", err)
	}
	if err := r.pay(t, draft.ID, "evt_late", "pi_late", topupPrice); err != nil {
		t.Fatalf("late payment: %v", err)
	}
	got := r.get(t, draft.ID)
	if got.Status != domain.InvoiceVoided || got.InvoiceNumber != "" || got.PaymentAnomalyKind != domain.EventPaymentReceivedOnVoidedInvoice {
		t.Errorf("after a late payment: %s %q anomaly %q, want voided, unnumbered, received_on_voided_invoice", got.Status, got.InvoiceNumber, got.PaymentAnomalyKind)
	}
	if n := len(r.grants(t, draft.ID)); n != 0 {
		t.Errorf("grants = %d, want 0", n)
	}
}

// A capture of another amount is recorded and reported; the grant is the
// commit amount frozen at create.
func TestTopup_CapturedAmountDiffers_AmountMismatchAndTheFrozenGrant(t *testing.T) {
	r := newTopupRig(t, "topup-mismatch")
	draft := r.create(t)
	if err := r.pay(t, draft.ID, "evt_mismatch", "pi_mismatch", topupPrice-100); err != nil {
		t.Fatalf("payment: %v", err)
	}
	got := r.get(t, draft.ID)
	if got.Status != domain.InvoicePaid || got.PaymentAnomalyKind != domain.EventPaymentAmountMismatch {
		t.Errorf("after payment: %s anomaly %q, want paid with amount_mismatch", got.Status, got.PaymentAnomalyKind)
	}
	if g := r.grants(t, draft.ID); len(g) != 1 || g[0].amount != topupGrant {
		t.Errorf("grants %+v, want one of the frozen %d", g, topupGrant)
	}
}

// The tax was decided at create, so a provider switched since then is never
// asked, and nothing at payment can refuse it.
func TestTopup_TaxProviderSwitchedAfterCreate_PaymentStillIssuesAndGrants(t *testing.T) {
	r := newTopupRig(t, "topup-provider")
	draft := r.create(t)
	if _, err := r.settings.Update(r.ctx, r.tenantID, func(ts *domain.TenantSettings) error {
		ts.TaxProvider = "stripe_tax"
		return nil
	}); err != nil {
		t.Fatalf("switch provider: %v", err)
	}
	if err := r.pay(t, draft.ID, "evt_provider", "pi_provider", topupPrice); err != nil {
		t.Fatalf("payment: %v", err)
	}
	got := r.get(t, draft.ID)
	if got.Status != domain.InvoicePaid || got.TaxAmountCents != 0 || got.TaxCalculationID != "" || got.TaxStatus != domain.InvoiceTaxOK {
		t.Errorf("after payment: %s tax %d calc %q status %s, want paid with the tax decided at create", got.Status, got.TaxAmountCents, got.TaxCalculationID, got.TaxStatus)
	}
	if n := len(r.grants(t, draft.ID)); n != 1 {
		t.Errorf("grants = %d, want 1", n)
	}
}

// The grant is dated when Stripe's answer arrived, not when the charge was
// asked for.
func TestTopup_GrantDatedWhenThePaymentArrived(t *testing.T) {
	r := newTopupRig(t, "topup-arrival")
	asked := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	arrived := asked.Add(7 * time.Minute)
	server := clock.NewFake(asked)
	r.stripe.SetClock(server)
	r.client.during = func() { server.Set(arrived) }
	r.ctx = clock.WithEffectiveNow(r.ctx, asked)

	paid, err := r.charge(t, r.create(t))
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	g := r.grants(t, paid.ID)
	if len(g) != 1 || !g[0].createdAt.Equal(arrived) || paid.PaidAt == nil || !paid.PaidAt.Equal(arrived) {
		t.Fatalf("grant %+v paid_at %v, want both at the answer's time %v", g, paid.PaidAt, arrived)
	}
}

// The validity is copied when the top-up is made, so a later change of the
// setting leaves an unpaid one as it was.
func TestTopup_CreditValidity_FrozenAtCreate(t *testing.T) {
	r := newTopupRig(t, "topup-validity")
	year, month := 365, 30
	r.setValidity(t, &year)
	expiring := r.create(t)
	r.setValidity(t, nil)
	forever := r.create(t)
	r.setValidity(t, &month)

	for i, inv := range []domain.Invoice{expiring, forever} {
		if err := r.pay(t, inv.ID, "evt_validity_"+inv.ID, "pi_validity_"+inv.ID, topupPrice); err != nil {
			t.Fatalf("payment %d: %v", i, err)
		}
	}
	paid := r.get(t, expiring.ID)
	want := domain.AddDaysIn(*paid.PaidAt, year, domain.LoadLocationOrUTC(paid.BillingTimezone))
	if g := r.grants(t, expiring.ID); len(g) != 1 || !g[0].expiresAt.Valid || !g[0].expiresAt.Time.Equal(want) {
		t.Errorf("365-day top-up grants %+v, want one expiring %v", g, want)
	}
	if g := r.grants(t, forever.ID); len(g) != 1 || g[0].expiresAt.Valid {
		t.Errorf("top-up made with no setting grants %+v, want one that never expires", g)
	}
}
