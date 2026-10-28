package invoice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil/pdftest"
)

// TestRenderPDF_LinesRenderUnchanged pins what the issued invoice document shows
// across B5, which removed the int/decimal duplicates from the line's JSON
// (quantity_decimal, unit_amount_cents). The PDF reads the stored line, not
// the JSON, so its rendering must not move. The check hashes every text run
// on every page (text, font, size, position): the raw PDF bytes differ from
// one process to the next, so they cannot be pinned. The hash below was
// captured on main before B5 (c0f24b68). The lines cover every quantity and
// unit-price shape:
// a fractional usage quantity, a whole usage quantity with a nominal sub-cent
// rate on a tokens meter, a seat base fee, a manual add-on of 1,500 units,
// and a zero-quantity usage line.
//
// If a deliberate PDF change moves the hash, re-capture it in that change and
// say why in its PR; it must never move as a side effect of an API field.
func TestRenderPDF_LinesRenderUnchanged(t *testing.T) {
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
		ID: "hc_inv_golden", InvoiceNumber: "HC-202604-0007", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, Currency: "USD",
		SubtotalCents: 8800, TotalAmountCents: 8800, AmountDueCents: 8800,
		IssuedAt: &issued, DueAt: &due, NetPaymentTermDays: 30,
		BillingPeriodStart: start, BillingPeriodEnd: issued,
	}
	render := func() string {
		// The footer stamps the render time: pin it.
		out, err := RenderPDF(clock.WithEffectiveNow(context.Background(), issued), inv, items, BillToInfo{Name: "Acme Corp"}, nil, CompanyInfo{Name: "Seller Ltd"})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		h := sha256.New()
		for i, p := range pdftest.Read(t, out) {
			_, _ = fmt.Fprintf(h, "page %d %.2fx%.2f\n", i+1, p.Width, p.Height)
			for _, r := range p.Runs {
				_, _ = fmt.Fprintf(h, "%.2f %.2f %s %.1f %q\n", r.X, r.Y, r.Font, r.Size, r.Text)
			}
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	got := render()
	if again := render(); again != got {
		t.Fatalf("the render is not deterministic: %s then %s", got, again)
	}
	const want = "37c8432b5204498407d77add0372b64d0bbc459e799b1e73b75689fde3d516ac"
	if got != want {
		t.Errorf("PDF sha256 = %s, want %s (what the issued document shows changed)", got, want)
	}
}
