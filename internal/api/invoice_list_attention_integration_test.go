package api

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/paymentmethods"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// tenantTxCounter counts tenant-scoped transactions: every TxTenant sets
// app.tenant_id exactly once (postgres.DB.BeginTx).
type tenantTxCounter struct{ n atomic.Int64 }

func (c *tenantTxCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "set_config('app.tenant_id'") {
		c.n.Add(1)
	}
	return ctx
}
func (c *tenantTxCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type stripeAlwaysConnected struct{}

func (stripeAlwaysConnected) HasFor(context.Context, string, bool) bool { return true }

// Issue #1142: GET /v1/invoices took ~110 ms for 25 rows because every row
// read its own attention facts (payment setup, Stripe credentials, and for
// unpaid rows customer, dunning run, pause, payment page), each its own
// transaction. Real Postgres, production wiring (router.go): the page now
// costs the same number of transactions at 7 rows as at 47, and every
// batched fact still reaches its row's banner, the same banner Get shows.
func TestInvoiceList_AttentionCostIsPerPage_RealStore(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()
	tenantID := testutil.CreateTestTenant(t, db, "Invoice List Attention")

	exec := func(q string, args ...any) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("seed begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("seed commit: %v", err)
		}
	}
	seedCust := customer.NewPostgresStore(db)
	seedInv := invoice.NewPostgresStore(db)
	newCustomer := func(key, email string, card bool) string {
		t.Helper()
		c, err := seedCust.Create(ctx, tenantID, domain.Customer{ExternalID: "ext_" + key, DisplayName: key, Email: email})
		if err != nil {
			t.Fatalf("create customer: %v", err)
		}
		if card {
			if err := seedCust.SetStripeCustomerID(ctx, tenantID, c.ID, "cus_stripe_"+key); err != nil {
				t.Fatalf("stripe id: %v", err)
			}
			exec(`INSERT INTO payment_methods (id, tenant_id, customer_id, stripe_payment_method_id, type, is_default)
				VALUES ($1, $2, $3, $4, 'card', true)`, "vlx_pm_"+key, tenantID, c.ID, "pm_"+key)
		}
		return c.ID
	}
	newInvoice := func(key, custID, subID string, status domain.InvoiceStatus, pay domain.InvoicePaymentStatus) string {
		t.Helper()
		in := domain.Invoice{
			CustomerID: custID, SubscriptionID: subID, InvoiceNumber: "INV-" + key,
			Status: status, PaymentStatus: pay, Currency: "USD",
			SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
			BillingPeriodStart: time.Now().UTC().Add(-30 * 24 * time.Hour), BillingPeriodEnd: time.Now().UTC(),
		}
		if status == domain.InvoicePaid {
			now := time.Now().UTC()
			in.AmountDueCents, in.AmountPaidCents, in.PaidAt = 0, 5000, &now
		}
		inv, err := seedInv.Create(ctx, tenantID, in)
		if err != nil {
			t.Fatalf("create invoice %s: %v", key, err)
		}
		return inv.ID
	}

	// One row per batched fact. Create queues a finalized, pending invoice for
	// collection (collectionQueuedAtFinalize), so a ready card reads "scheduled".
	scheduled := newInvoice("scheduled", newCustomer("ready", "ready@acme.test", true), "", domain.InvoiceFinalized, domain.PaymentPending)

	pausedCust := newCustomer("paused", "paused@acme.test", true)
	exec(`INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, billing_time, pause_collection_behavior)
		VALUES ('vlx_sub_paused', $1, 'paused', 'paused', $2, 'active', 'anniversary', 'keep_as_draft')`, tenantID, pausedCust)
	paused := newInvoice("paused", pausedCust, "vlx_sub_paused", domain.InvoiceFinalized, domain.PaymentPending)
	if err := seedInv.SetAutoChargePending(ctx, tenantID, paused, true); err != nil {
		t.Fatalf("queue: %v", err)
	}

	noCardEmail := newInvoice("nocard-email", newCustomer("nocard-email", "nocard@acme.test", false), "", domain.InvoiceFinalized, domain.PaymentPending)
	noCardNoEmail := newInvoice("nocard-noemail", newCustomer("nocard-noemail", "", false), "", domain.InvoiceFinalized, domain.PaymentPending)

	escalatedCust := newCustomer("escalated", "esc@acme.test", false)
	escalated := newInvoice("escalated", escalatedCust, "", domain.InvoiceFinalized, domain.PaymentPending)
	exec(`INSERT INTO dunning_policies (id, tenant_id, name) VALUES ('vlx_dpol_1142', $1, 'p')`, tenantID)
	exec(`INSERT INTO invoice_dunning_runs (id, tenant_id, invoice_id, customer_id, policy_id, state, attempt_count, last_attempt_at, resolved_at, resolution)
		VALUES ('vlx_drun_1142', $1, $2, $3, 'vlx_dpol_1142', 'escalated', 3, now(), now(), 'retries_exhausted')`, tenantID, escalated, escalatedCust)

	pageOpen := newInvoice("page", newCustomer("page", "page@acme.test", false), "", domain.InvoiceFinalized, domain.PaymentPending)
	exec(`INSERT INTO checkout_sessions (id, tenant_id, invoice_id, livemode, amount_cents, currency, status)
		VALUES ($1, $2, $3, false, 5000, 'USD', 'open')`, postgres.NewID("vlx_cks"), tenantID, pageOpen)

	paid := newInvoice("paid", newCustomer("paid", "paid@acme.test", true), "", domain.InvoicePaid, domain.PaymentSucceeded)

	// The service under test, wired as router.go wires it, on a traced pool.
	counter := &tenantTxCounter{}
	traced := testutil.OpenTracedDB(t, counter)
	custStore := customer.NewPostgresStore(traced)
	invStore := invoice.NewPostgresStore(traced)
	svc := invoice.NewService(invStore, nil, nil)
	svc.SetPaymentMethodReader(&compositePaymentSetupStore{
		customers: custStore,
		pms:       paymentmethods.NewService(paymentmethods.NewPostgresStore(traced), nil, nil),
	})
	svc.SetCustomerReader(custStore)
	svc.SetDunningRunReader(dunning.NewPostgresStore(traced))
	svc.SetSubscriptionPauseReader(subscription.NewPostgresStore(traced))
	svc.SetPaymentPageReader(invStore)
	svc.SetStripeChecker(stripeAlwaysConnected{})

	list := func() ([]domain.Invoice, int64) {
		t.Helper()
		before := counter.n.Load()
		got, _, err := svc.List(ctx, invoice.ListFilter{TenantID: tenantID, Limit: 100})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return got, counter.n.Load() - before
	}

	rows, smallTx := list()
	byID := map[string]domain.Invoice{}
	for _, inv := range rows {
		byID[inv.ID] = inv
	}
	code := func(id string) string {
		if a := byID[id].Attention; a != nil {
			return a.Code
		}
		return ""
	}
	hasResend := func(id string) bool {
		for _, a := range byID[id].Attention.Actions {
			if a.Code == domain.AttentionActionSendReminder {
				return true
			}
		}
		return false
	}
	for id, want := range map[string]string{
		scheduled:     "payment.scheduled",         // ready card, queued at finalize: payment readiness
		paused:        "payment.collection_paused", // queued on a paused sub: pause state
		noCardEmail:   "payment.no_payment_method", // no card
		noCardNoEmail: "payment.no_payment_method", // no card
		escalated:     "payment.dunning_exhausted", // escalated run: dunning
		pageOpen:      "payment.page_open",         // live checkout: payment pages
		paid:          "",                          // settled: no banner
	} {
		if got := code(id); got != want {
			t.Errorf("%s: attention code %q, want %q", id, got, want)
		}
	}
	if !hasResend(noCardEmail) {
		t.Error("customer with an email: no Resend setup link (email fact lost)")
	}
	if hasResend(noCardNoEmail) {
		t.Error("customer without an email: offered Resend setup link")
	}

	// The list row is the banner Get shows.
	for _, inv := range rows {
		one, err := svc.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("get %s: %v", inv.ID, err)
		}
		if !reflect.DeepEqual(one.Attention, inv.Attention) || one.BillingPeriodDisplay != inv.BillingPeriodDisplay {
			t.Errorf("%s: list row %+v differs from Get %+v", inv.ID, inv.Attention, one.Attention)
		}
	}

	// Forty more unpaid, card-less rows, each its own customer: every per-row
	// read the old path made is now in play for each of them.
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("bulk%02d", i)
		newInvoice(key, newCustomer(key, key+"@acme.test", false), "", domain.InvoiceFinalized, domain.PaymentPending)
	}
	bigRows, bigTx := list()
	if len(bigRows) != len(rows)+40 {
		t.Fatalf("rows = %d, want %d", len(bigRows), len(rows)+40)
	}
	if bigTx != smallTx {
		t.Errorf("tenant transactions: %d for %d rows, %d for %d rows — the cost grows with the page (N+1)",
			smallTx, len(rows), bigTx, len(bigRows))
	}
	// List (1) + readiness (customers, payment methods: 2) + email (1) +
	// dunning (1) + pause (1) + payment pages (1).
	if bigTx > 7 {
		t.Errorf("tenant transactions for a %d-row page = %d, want <= 7", len(bigRows), bigTx)
	}
}
