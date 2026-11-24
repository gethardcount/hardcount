package invoice_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// commitHarness wires the REAL production seams (ADR-078): the invoice store
// with credit.Service injected as commit funder and the tenant settings store
// as the number a top-up takes when it is paid, the invoice service with
// credit.Service as credit reverser — the same wiring the router does.
type commitHarness struct {
	db        *postgres.DB
	tenantID  string
	custID    string
	invStore  *invoice.PostgresStore
	invSvc    *invoice.Service
	creditSvc *credit.Service
}

func newCommitHarness(t *testing.T, name string) (*commitHarness, context.Context) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)

	tenantID := testutil.CreateTestTenant(t, db, name)
	cust, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{
		ExternalID: "cus_" + strings.ReplaceAll(strings.ToLower(name), " ", "_"), DisplayName: name,
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}

	creditStore := credit.NewPostgresStore(db)
	creditStore.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	creditSvc := credit.NewService(creditStore)
	settings := tenant.NewSettingsStore(db)
	invStore := invoice.NewPostgresStore(db)
	invStore.SetCommitFunder(creditSvc)
	invStore.SetInvoiceNumberer(settings)
	invSvc := invoice.NewService(invStore, nil, settings)
	invSvc.SetCreditReverser(creditSvc)

	return &commitHarness{
		db: db, tenantID: tenantID, custID: cust.ID,
		invStore: invStore, invSvc: invSvc, creditSvc: creditSvc,
	}, ctx
}

// armLowThreshold sets (or clears with 0) the tenant's balance_low threshold.
func (h *commitHarness) armLowThreshold(t *testing.T, ctx context.Context, cents int64) {
	t.Helper()
	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, h.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var v any
	if cents > 0 {
		v = cents
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tenant_settings (tenant_id, credit_balance_low_threshold_cents)
		VALUES ($1, $2)
		ON CONFLICT (tenant_id) DO UPDATE SET credit_balance_low_threshold_cents = $2
	`, h.tenantID, v); err != nil {
		t.Fatalf("arm threshold: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// newCustomer creates a fresh customer for per-round isolation.
func (h *commitHarness) newCustomer(t *testing.T, ctx context.Context, round int) string {
	t.Helper()
	cust, err := customer.NewPostgresStore(h.db).Create(ctx, h.tenantID, domain.Customer{
		ExternalID: fmt.Sprintf("cus_round_%d", round), DisplayName: fmt.Sprintf("Round %d", round),
	})
	if err != nil {
		t.Fatalf("round customer: %v", err)
	}
	return cust.ID
}

// seedPayable inserts a finalized non-commit invoice as a drain target.
func (h *commitHarness) seedPayable(t *testing.T, ctx context.Context, custID string, dueCents int64, round int) string {
	t.Helper()
	inv, err := invoice.NewPostgresStore(h.db).CreateWithLineItems(ctx, h.tenantID, domain.Invoice{
		CustomerID: custID, InvoiceNumber: fmt.Sprintf("HC-CONC-%d", round),
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		Currency: "USD", SubtotalCents: dueCents, TotalAmountCents: dueCents, AmountDueCents: dueCents,
		BillingPeriodStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		BillingPeriodEnd:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}, nil)
	if err != nil {
		t.Fatalf("seed payable: %v", err)
	}
	return inv.ID
}

// countEvents counts outbox rows of one event type whose payload names the
// customer.
func (h *commitHarness) countEvents(t *testing.T, ctx context.Context, custID, eventType string) int {
	t.Helper()
	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, h.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM webhook_outbox
		WHERE tenant_id = $1 AND event_type = $2 AND payload->'credit_balance'->>'customer_id' = $3
	`, h.tenantID, eventType, custID).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// createTopup makes a top-up selling grantCents of credit for priceCents in
// currency, as POST /v1/invoices does. Its payment (payOffline) issues it and
// grants the credit.
func (h *commitHarness) createTopup(t *testing.T, ctx context.Context, currency string, priceCents, grantCents int64) domain.Invoice {
	t.Helper()
	inv, err := h.invSvc.Create(ctx, h.tenantID, invoice.CreateInput{
		CustomerID: h.custID,
		Currency:   currency,
		LineItems: []invoice.AddLineItemInput{{
			Description:        "Prepaid credit",
			Quantity:           1,
			UnitAmountCents:    priceCents,
			CommitGrantedCents: &grantCents,
		}},
	})
	if err != nil {
		t.Fatalf("create top-up: %v", err)
	}
	return inv
}

func (h *commitHarness) balance(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	bal, err := testutil.BalanceUSD(h.creditSvc.GetBalance(ctx, h.tenantID, h.custID))
	if err != nil {
		t.Fatalf("get balance: %v", err)
	}
	return bal.BalanceCents
}

// commitGrant returns the commit grant row funded by the invoice, or false.
func (h *commitHarness) commitGrant(t *testing.T, ctx context.Context, invoiceID string) (domain.CreditLedgerEntry, bool) {
	t.Helper()
	entries, err := h.creditSvc.ListEntries(ctx, credit.ListFilter{
		TenantID: h.tenantID, CustomerID: h.custID,
	})
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	for _, e := range entries {
		if e.SourceInvoiceID == invoiceID && e.GrantKind == domain.GrantKindCommit {
			return e, true
		}
	}
	return domain.CreditLedgerEntry{}, false
}

// TestUpdateStatusWithReversalPrior_CAS_RejectsPaidToVoid pins ADR-078 D5 at
// the STORE level: the void flip carries an in-SQL allowed-source predicate,
// so a pay-vs-void race (service guard read a stale pre-paid snapshot) cannot
// flip a PAID top-up to voided after its payment granted the credit.
func TestUpdateStatusWithReversalPrior_CAS_RejectsPaidToVoid(t *testing.T) {
	h, ctx := newCommitHarness(t, "CAS Paid To Void")

	inv := h.createTopup(t, ctx, "USD", 10000, 10000)
	if _, err := h.invStore.MarkPaid(ctx, h.tenantID, inv.ID, "pi_cas_test", time.Now()); err != nil {
		t.Fatalf("mark paid: %v", err)
	}

	// Direct store-level flip, simulating a Void whose service guard passed
	// BEFORE the payment landed.
	_, err := h.invStore.UpdateStatusWithReversalPrior(ctx, h.tenantID, inv.ID, domain.InvoiceVoided, nil)
	if err == nil {
		t.Fatal("paid→voided must be rejected by the in-tx CAS")
	}
	if !strings.Contains(err.Error(), "cannot transition") {
		t.Errorf("error = %v, want the CAS transition rejection", err)
	}
	if got := h.balance(t, ctx); got != 10000 {
		t.Errorf("balance = %d, want 10000 (paid commit's credits untouched)", got)
	}
}
