package invoice

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// A draft is a preview: the PDF marks it DRAFT and prints "Draft — not
// issued" in place of the issue and due dates, which are only set when the
// draft is finalized.
func TestIssueDateRows_DraftIsNotIssued(t *testing.T) {
	issued := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	due := issued.AddDate(0, 0, 30)

	draft := domain.Invoice{Status: domain.InvoiceDraft, IssuedAt: &issued, DueAt: &due}
	rows := issueDateRows(draft, time.UTC)
	if len(rows) != 1 || rows[0] != [2]string{"Status", "Draft — not issued"} {
		t.Fatalf("draft rows = %v, want only Status: Draft — not issued", rows)
	}

	open := domain.Invoice{Status: domain.InvoiceFinalized, IssuedAt: &issued, DueAt: &due}
	rows = issueDateRows(open, time.UTC)
	want := [][2]string{{"Issued", "October 16, 2026"}, {"Due Date", "November 15, 2026"}}
	if len(rows) != 2 || rows[0] != want[0] || rows[1] != want[1] {
		t.Fatalf("finalized rows = %v, want %v", rows, want)
	}
}

func TestWatermarkFor(t *testing.T) {
	cases := map[domain.InvoiceStatus]string{
		domain.InvoiceDraft:         "DRAFT",
		domain.InvoiceVoided:        "VOID",
		domain.InvoiceFinalized:     "",
		domain.InvoicePaid:          "",
		domain.InvoiceUncollectible: "",
	}
	for status, want := range cases {
		if got := watermarkFor(status); got != want {
			t.Errorf("watermarkFor(%s) = %q, want %q", status, got, want)
		}
	}
}

// The PDF never invents a seller: an unset company name prints nothing
// (it used to print "Velox").
func TestSellerName_NeverAPlaceholder(t *testing.T) {
	if got := sellerName(); got != "" {
		t.Errorf("no company: got %q, want empty", got)
	}
	if got := sellerName(CompanyInfo{Name: "  "}); got != "" {
		t.Errorf("blank company name: got %q, want empty", got)
	}
	if got := sellerName(CompanyInfo{Name: "Acme Ltd"}); got != "Acme Ltd" {
		t.Errorf("got %q, want Acme Ltd", got)
	}
}

// A draft and a seller-less invoice still render.
func TestRenderPDF_DraftWithoutSeller(t *testing.T) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	inv := domain.Invoice{
		ID: "vlx_inv_draft", InvoiceNumber: "VLX-000007", Status: domain.InvoiceDraft,
		Currency: "USD", SubtotalCents: 9900, TotalAmountCents: 9900, AmountDueCents: 9900,
		IssuedAt: &now, Memo: "Draft add-on",
	}
	items := []domain.InvoiceLineItem{{Description: "Add-on", LineType: domain.LineTypeAddOn, Quantity: 1, UnitAmountCents: 9900, AmountCents: 9900}}
	out, err := RenderPDF(context.Background(), inv, items, BillToInfo{Name: "Northwind"}, nil, CompanyInfo{AddressLine1: "1 Main St"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(out) < 1000 {
		t.Fatalf("pdf too small: %d bytes", len(out))
	}
}
