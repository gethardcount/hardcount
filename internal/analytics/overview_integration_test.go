package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestOverview_CreditAndCurrency covers two medium-severity audit findings:
//   - credit_balance_total used DISTINCT ON balance_after (latest row by
//     created_at), which mis-reports after out-of-order expiry inserts; it now
//     sums amount_cents (order-independent, matches credit.GetBalance).
//   - revenue/AR/avg summed total_amount_cents across currencies; they're now
//     scoped to the tenant's default currency so a EUR invoice can't corrupt a
//     USD-denominated total.
func TestOverview_CreditAndCurrency(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Analytics Overview")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_ov", DisplayName: "Overview",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	// Default currency = USD (no tenant_settings row → handler falls back to USD).
	invoiceStore := invoice.NewPostgresStore(db)
	now := time.Now().UTC()
	usd := makePaidInvoice(t, ctx, invoiceStore, tenantID, cust.ID, "USD", 100_00)
	_ = makePaidInvoice(t, ctx, invoiceStore, tenantID, cust.ID, "EUR", 999_00)
	markPaidAt(t, db, usd.ID, now.Add(-1*time.Hour))
	// EUR invoice is paid too but in a non-default currency — must be excluded.
	eur := makePaidInvoice(t, ctx, invoiceStore, tenantID, cust.ID, "EUR", 500_00)
	markPaidAt(t, db, eur.ID, now.Add(-1*time.Hour))

	// Credit ledger: a grant (+5000) and an expiry (-2000) inserted OUT of
	// created_at order — the expiry's created_at is EARLIER than the grant's,
	// so "latest row by created_at" (the grant, balance_after=5000) disagrees
	// with the true balance (3000). Authoritative SUM(amount_cents)=3000.
	seedLedger(t, db, tenantID, cust.ID, now)

	h := NewHandler(db)
	req := httptest.NewRequest("GET", "/overview?period=30d", nil)
	req = req.WithContext(auth.WithTenantID(postgres.WithLivemode(req.Context(), false), tenantID))
	rr := httptest.NewRecorder()
	h.overview(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status: got %d, body=%s", rr.Code, rr.Body.String())
	}
	var resp OverviewResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Currency != "USD" {
		t.Errorf("currency: got %q, want USD", resp.Currency)
	}
	if resp.Revenue != 100_00 {
		t.Errorf("revenue: got %d, want 10000 (USD only — EUR invoices excluded, not summed)", resp.Revenue)
	}
	if resp.CreditBalance != 3000 {
		t.Errorf("credit_balance_total: got %d, want 3000 (SUM(amount_cents), not latest balance_after)", resp.CreditBalance)
	}
}

func makePaidInvoice(t *testing.T, ctx context.Context, store *invoice.PostgresStore, tenantID, custID, currency string, total int64) domain.Invoice {
	t.Helper()
	now := time.Now().UTC()
	issued := now
	inv, err := store.Create(ctx, tenantID, domain.Invoice{
		CustomerID:         custID,
		InvoiceNumber:      fmt.Sprintf("INV-%s-%d", currency, total),
		Status:             domain.InvoicePaid,
		PaymentStatus:      domain.PaymentSucceeded,
		Currency:           currency,
		SubtotalCents:      total,
		TotalAmountCents:   total,
		AmountDueCents:     0,
		AmountPaidCents:    total,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   now,
		IssuedAt:           &issued,
	})
	if err != nil {
		t.Fatalf("create %s invoice: %v", currency, err)
	}
	return inv
}

func markPaidAt(t *testing.T, db *postgres.DB, invoiceID string, at time.Time) {
	t.Helper()
	execBypass(t, db, `UPDATE invoices SET paid_at = $2 WHERE id = $1`, invoiceID, at)
}

func seedLedger(t *testing.T, db *postgres.DB, tenantID, custID string, now time.Time) {
	t.Helper()
	// Insert in a tenant tx so app.livemode is set (false) and the ledger
	// rows land in the same partition the handler reads.
	ctx := postgres.WithLivemode(context.Background(), false)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin tenant tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	// grant +5000 at T (created later), expiry -2000 at T-2h (created earlier).
	if _, err := tx.Exec(`
		INSERT INTO customer_credit_ledger (id, tenant_id, customer_id, entry_type, amount_cents, balance_after, description, created_at)
		VALUES ($1,$2,$3,'expiry',-2000,3000,'expiry', $5),
		       ($4,$2,$3,'grant',  5000,5000,'grant',  $6)
	`, postgres.NewID("vlx_ccl"), tenantID, custID, postgres.NewID("vlx_ccl"), now.Add(-2*time.Hour), now); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit ledger: %v", err)
	}
}

func execBypass(t *testing.T, db *postgres.DB, q string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin bypass: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(q, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestOverview_OutstandingByCurrency: the dashboard's "N open invoices · X
// outstanding" row put a count over every currency next to a default-currency
// sum, so a tenant owed €300 and $200 read "2 open invoices · $200.00
// outstanding". outstanding_by_currency names every currency the open
// invoices are owed in, default currency first; outstanding_ar keeps its
// default-currency-only meaning for API callers.
func TestOverview_OutstandingByCurrency(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, "Analytics Outstanding")
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_out", DisplayName: "Outstanding",
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	store := invoice.NewPostgresStore(db)

	open := func(currency string, due int64) {
		t.Helper()
		inv := makePaidInvoice(t, ctx, store, tenantID, cust.ID, currency, due)
		execBypass(t, db, `UPDATE invoices SET status = 'finalized', payment_status = 'pending',
			paid_at = NULL, amount_paid_cents = 0, amount_due_cents = $2 WHERE id = $1`, inv.ID, due)
	}
	open("EUR", 300_00) // larger than the USD total: ordering must still put USD first
	open("USD", 150_00)
	open("USD", 50_00)
	open("GBP", 10_00)
	// Paid invoices owe nothing and must not appear.
	_ = makePaidInvoice(t, ctx, store, tenantID, cust.ID, "JPY", 999_00)

	resp := getOverview(t, db, tenantID)

	if resp.OpenInvoices != 4 {
		t.Errorf("open_invoices: got %d, want 4", resp.OpenInvoices)
	}
	if resp.OutstandingAR != 200_00 {
		t.Errorf("outstanding_ar: got %d, want 20000 (default currency only)", resp.OutstandingAR)
	}
	want := []CurrencyAmount{
		{Currency: "USD", AmountCents: 200_00},
		{Currency: "EUR", AmountCents: 300_00},
		{Currency: "GBP", AmountCents: 10_00},
	}
	if len(resp.OutstandingByCurrency) != len(want) {
		t.Fatalf("outstanding_by_currency: got %+v, want %+v", resp.OutstandingByCurrency, want)
	}
	for i := range want {
		if resp.OutstandingByCurrency[i] != want[i] {
			t.Errorf("outstanding_by_currency[%d]: got %+v, want %+v", i, resp.OutstandingByCurrency[i], want[i])
		}
	}
}
