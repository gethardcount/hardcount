package creditnote

import (
	"context"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/testutil/pdftest"
)

type cnStaticSettings struct{ ts domain.TenantSettings }

func (s cnStaticSettings) Get(context.Context, string) (domain.TenantSettings, error) {
	return s.ts, nil
}

// The dashboard says the support URL is printed for customers. The credit-note
// PDF did not print it (sweep 2026-10-21, C6). It is branding, so it is the
// current setting even on an issued credit note whose seller block is the
// copy taken at issue (ADR-109).
func TestCreditNotePDF_PrintsSupportURL(t *testing.T) {
	settings := cnStaticSettings{ts: domain.TenantSettings{
		CompanyName: "Seller Now Ltd",
		SupportURL:  "https://seller.test/help",
	}}
	h := NewHandler(nil, HandlerDeps{
		Customers: cnStaticCustomers{cust: domain.Customer{DisplayName: "Acme Corp"}},
		Settings:  settings,
		Invoices:  cnStaticInvoices{inv: domain.Invoice{InvoiceNumber: "VLX-202604-0042", Currency: "USD"}},
	})

	for _, c := range []struct {
		name     string
		snapshot *domain.DocumentSupplier
	}{
		{"issued, seller copied at issue", &domain.DocumentSupplier{Name: "Seller At Issue Ltd"}},
		{"no copy (draft or issued before copies)", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			cn, items := longCreditNote(2, domain.CreditNoteIssued)
			cn.SupplierSnapshot = c.snapshot
			bt, ci, orig := h.assemblePDFContext(context.Background(), "t1", cn)
			out, err := RenderPDF(context.Background(), cn, items, orig, bt, ci)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			found := false
			for _, p := range pdftest.Read(t, out) {
				found = found || p.Has("Need help? https://seller.test/help")
			}
			if !found {
				t.Error("credit-note PDF does not print the support URL")
			}
		})
	}
}
