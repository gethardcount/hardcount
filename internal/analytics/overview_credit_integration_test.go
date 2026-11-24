package analytics

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// seededInvoice is an invoice written through the store and then set to the
// given state, the way the overview reads it.
type seededInvoice struct {
	currency  string
	status    domain.InvoiceStatus
	reason    domain.InvoiceBillingReason
	paidCents int64
	paidAt    *time.Time
	issuedAt  *time.Time
	lines     []domain.InvoiceLineItem
}

func seedInvoice(t *testing.T, db *postgres.DB, tenantID, custID string, s seededInvoice) {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	var total int64
	for i := range s.lines {
		s.lines[i].Currency = s.currency
		s.lines[i].Quantity = 1
		s.lines[i].UnitAmountCents = s.lines[i].AmountCents
		s.lines[i].TotalAmountCents = s.lines[i].AmountCents
		s.lines[i].Description = string(s.lines[i].LineType)
		total += s.lines[i].AmountCents
	}
	now := time.Now().UTC()
	inv, err := invoice.NewPostgresStore(db).CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID:         custID,
		InvoiceNumber:      postgres.NewID("INV"),
		Status:             domain.InvoiceFinalized,
		PaymentStatus:      domain.PaymentPending,
		Currency:           s.currency,
		SubtotalCents:      total,
		TotalAmountCents:   total,
		AmountDueCents:     total,
		BillingReason:      s.reason,
		BillingPeriodStart: now.Add(-30 * 24 * time.Hour),
		BillingPeriodEnd:   now,
	}, s.lines)
	if err != nil {
		t.Fatalf("create %s invoice: %v", s.currency, err)
	}
	execBypass(t, db, `UPDATE invoices SET status = $2, billing_reason = NULLIF($3, ''), amount_paid_cents = $4,
		paid_at = $5, issued_at = $6 WHERE id = $1`,
		inv.ID, string(s.status), string(s.reason), s.paidCents, s.paidAt, s.issuedAt)
}

func usageLine(cents int64) domain.InvoiceLineItem {
	return domain.InvoiceLineItem{LineType: domain.LineTypeUsage, AmountCents: cents}
}

func commitLine(paid, granted int64) domain.InvoiceLineItem {
	return domain.InvoiceLineItem{LineType: domain.LineTypeAddOn, AmountCents: paid, CommitGrantedCents: &granted}
}

// TestOverview_CreditSoldAndUsageBilled: credit sold is the credit the paid
// top-ups granted, not what was paid for them; usage billed is the usage
// lines of the invoices issued in the window. Each is one figure per
// currency, the default currency first.
//
// Seeded world (default currency USD, period=30d):
//
//	top-up USD paid 1h ago:  pays 9000, grants 10000   sold, revenue 9000
//	top-up USD unpaid draft: grants 5000               not sold
//	top-up USD paid 40d ago: grants 7000               not sold (before)
//	top-up EUR paid 2h ago:  pays 3000, grants 3000    sold
//	USD invoice issued 2d ago: usage 1500 + base 500   usage 1500
//	USD invoice issued 3d ago, paid: usage 250         usage 250
//	EUR invoice issued 1d ago, uncollectible: usage 800 usage 800
//	USD invoice issued 40d ago: usage 9999             not in the window
//	USD invoice issued 1d ago, voided: usage 4444      not billed
//	USD draft, never issued: usage 3333                not billed
//
// Mutation checks (each makes this test fail):
//   - credit sold summing amount_cents (what was paid) → USD 9000
//   - usage without the issued_at bound → USD 11749
//   - usage without the status list → USD 6194
//   - either list without GROUP BY currency → one row
func TestOverview_CreditSoldAndUsageBilled(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Analytics Credit Sold")
	c, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{ExternalID: "cus_sold", DisplayName: "Sold"})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	now := time.Now().UTC()
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	day := 24 * time.Hour

	topup := func(currency string, status domain.InvoiceStatus, paid, granted int64, paidAt *time.Time) {
		t.Helper()
		s := seededInvoice{currency: currency, status: status, reason: domain.BillingReasonTopup,
			lines: []domain.InvoiceLineItem{commitLine(paid, granted)}}
		if status == domain.InvoicePaid {
			s.paidCents, s.paidAt, s.issuedAt = paid, paidAt, paidAt
		}
		seedInvoice(t, db, tenantID, c.ID, s)
	}
	topup("USD", domain.InvoicePaid, 9000, 10000, ago(time.Hour))
	topup("USD", domain.InvoiceDraft, 5000, 5000, nil)
	topup("USD", domain.InvoicePaid, 7000, 7000, ago(40*day))
	topup("EUR", domain.InvoicePaid, 3000, 3000, ago(2*time.Hour))

	billed := func(currency string, status domain.InvoiceStatus, issuedAt *time.Time, lines ...domain.InvoiceLineItem) {
		t.Helper()
		seedInvoice(t, db, tenantID, c.ID, seededInvoice{currency: currency, status: status, issuedAt: issuedAt, lines: lines})
	}
	billed("USD", domain.InvoiceFinalized, ago(2*day), usageLine(1500), domain.InvoiceLineItem{LineType: domain.LineTypeBaseFee, AmountCents: 500})
	billed("USD", domain.InvoicePaid, ago(3*day), usageLine(250))
	billed("EUR", domain.InvoiceUncollectible, ago(day), usageLine(800))
	billed("USD", domain.InvoiceFinalized, ago(40*day), usageLine(9999))
	billed("USD", domain.InvoiceVoided, ago(day), usageLine(4444))
	billed("USD", domain.InvoiceDraft, nil, usageLine(3333))

	resp := getOverview(t, db, tenantID)

	wantSold := []CurrencyAmount{{Currency: "USD", AmountCents: 10000}, {Currency: "EUR", AmountCents: 3000}}
	if !equalAmounts(resp.CreditSoldByCurrency, wantSold) {
		t.Errorf("credit_sold_by_currency = %+v, want %+v (the credit granted, not the 9000 paid; unpaid and earlier top-ups left out)", resp.CreditSoldByCurrency, wantSold)
	}
	if resp.Revenue != 9000 {
		t.Errorf("revenue = %d, want 9000 (what the USD top-up paid in the window)", resp.Revenue)
	}
	wantUsage := []CurrencyAmount{{Currency: "USD", AmountCents: 1750}, {Currency: "EUR", AmountCents: 800}}
	if !equalAmounts(resp.UsageBilledByCurrency, wantUsage) {
		t.Errorf("usage_billed_by_currency = %+v, want %+v (usage lines of invoices issued in the window, not voided)", resp.UsageBilledByCurrency, wantUsage)
	}
}

func equalAmounts(got, want []CurrencyAmount) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
