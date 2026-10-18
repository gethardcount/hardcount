package invoice

import (
	"context"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// A draft is a preview: the PDF prints "Draft — not issued" in place of the
// issue date. The due row agrees with the invoice page (and with what
// finalize does): a composed draft (billing_reason manual, or none) prints
// its terms, because finalize sets its due date; an engine draft keeps the
// due date it was built with, so that date is printed.
func TestIssueDateRows_DraftIsNotIssued(t *testing.T) {
	issued := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	due := issued.AddDate(0, 0, 30)

	for _, reason := range []domain.InvoiceBillingReason{domain.BillingReasonManual, ""} {
		draft := domain.Invoice{Status: domain.InvoiceDraft, BillingReason: reason, IssuedAt: &issued, DueAt: &due, NetPaymentTermDays: 30}
		want := [][2]string{{"Status", "Draft — not issued"}, {"Due", "Net 30 from finalizing"}}
		if rows := issueDateRows(draft, time.UTC); !equalRows(rows, want) {
			t.Fatalf("composed draft (reason %q) rows = %v, want %v", reason, rows, want)
		}
	}
	onReceipt := domain.Invoice{Status: domain.InvoiceDraft, BillingReason: domain.BillingReasonManual, DueAt: &due}
	if rows := issueDateRows(onReceipt, time.UTC); rows[1][1] != "Due on receipt once finalized" {
		t.Fatalf("net 0 draft rows = %v", rows)
	}

	cycle := domain.Invoice{Status: domain.InvoiceDraft, BillingReason: domain.BillingReasonSubscriptionCycle, IssuedAt: &issued, DueAt: &due, NetPaymentTermDays: 30}
	want := [][2]string{{"Status", "Draft — not issued"}, {"Due Date", "November 15, 2026"}}
	if rows := issueDateRows(cycle, time.UTC); !equalRows(rows, want) {
		t.Fatalf("cycle draft rows = %v, want %v", rows, want)
	}

	// A cycle draft without a due date prints the terms, as the page does,
	// not a draft with no due row at all.
	cycleNoDue := domain.Invoice{Status: domain.InvoiceDraft, BillingReason: domain.BillingReasonSubscriptionCycle, NetPaymentTermDays: 14}
	want = [][2]string{{"Status", "Draft — not issued"}, {"Due", "Net 14 from finalizing"}}
	if rows := issueDateRows(cycleNoDue, time.UTC); !equalRows(rows, want) {
		t.Fatalf("cycle draft without due date rows = %v, want %v", rows, want)
	}

	open := domain.Invoice{Status: domain.InvoiceFinalized, IssuedAt: &issued, DueAt: &due}
	want = [][2]string{{"Issued", "October 16, 2026"}, {"Due Date", "November 15, 2026"}}
	if rows := issueDateRows(open, time.UTC); !equalRows(rows, want) {
		t.Fatalf("finalized rows = %v, want %v", rows, want)
	}
}

func equalRows(got, want [][2]string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
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
