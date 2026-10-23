package invoice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestRenderPDF_LinesByteEqual pins the issued invoice document byte for byte
// across B5, which removed the int/decimal duplicates from the line's JSON
// (quantity_decimal, unit_amount_cents). The PDF reads the stored line, not
// the JSON, so its bytes must not move: the hash below was captured on main
// before B5 (c0f24b68). The lines cover every quantity and unit-price shape:
// a fractional usage quantity, a whole usage quantity with a nominal sub-cent
// rate on a tokens meter, a seat base fee, a manual add-on of 1,500 units,
// and a zero-quantity usage line.
//
// If a deliberate PDF change moves the hash, re-capture it in that change and
// say why in its PR; it must never move as a side effect of an API field.
func TestRenderPDF_LinesByteEqual(t *testing.T) {
	issued := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	due := issued.AddDate(0, 0, 30)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	nominal := decimal.RequireFromString("0.0003")
	items := []domain.InvoiceLineItem{
		{LineType: domain.LineTypeUsage, Description: "GPU hours", Quantity: 1, QuantityDecimal: decimal.RequireFromString("1.5"), UnitAmountCents: 667, AmountCents: 1000, TotalAmountCents: 1000, Currency: "USD", BillingPeriodStart: &start, BillingPeriodEnd: &issued},
		{LineType: domain.LineTypeUsage, Description: "Tokens (tokens)", MeterUnit: "tokens", Quantity: 6000000, QuantityDecimal: decimal.NewFromInt(6000000), AmountCents: 1800, TotalAmountCents: 1800, Currency: "USD", NominalUnitAmountDecimal: &nominal, BillingPeriodStart: &start, BillingPeriodEnd: &issued},
		{LineType: domain.LineTypeBaseFee, Description: "Pro plan", Quantity: 3, UnitAmountCents: 1000, AmountCents: 3000, TotalAmountCents: 3000, Currency: "USD", BillingPeriodStart: &start, BillingPeriodEnd: &issued},
		{LineType: domain.LineTypeAddOn, Description: "Onboarding credits", Quantity: 1500, UnitAmountCents: 2, AmountCents: 3000, TotalAmountCents: 3000, Currency: "USD"},
		{LineType: domain.LineTypeUsage, Description: "API calls (calls)", MeterUnit: "calls", Currency: "USD", BillingPeriodStart: &start, BillingPeriodEnd: &issued},
	}
	inv := domain.Invoice{
		ID: "vlx_inv_golden", InvoiceNumber: "VLX-202604-0007", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, Currency: "USD",
		SubtotalCents: 8800, TotalAmountCents: 8800, AmountDueCents: 8800,
		IssuedAt: &issued, DueAt: &due, NetPaymentTermDays: 30,
		BillingPeriodStart: start, BillingPeriodEnd: issued,
	}
	render := func() string {
		out, err := RenderPDF(context.Background(), inv, items, BillToInfo{Name: "Acme Corp"}, nil, CompanyInfo{Name: "Seller Ltd"})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		sum := sha256.Sum256(out)
		return hex.EncodeToString(sum[:])
	}
	got := render()
	if again := render(); again != got {
		t.Fatalf("the render is not deterministic: %s then %s", got, again)
	}
	const want = "71d4ad1c39581ee985d85a9cf93aad4e534573526edf1d1f1d96e6a6d556bf7b"
	if got != want {
		t.Errorf("PDF sha256 = %s, want %s (the issued document changed)", got, want)
	}
}
