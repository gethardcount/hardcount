package invoice

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/testutil/pdftest"
)

// The invoice footer and the support URL are settings the dashboard says
// print on invoices. Neither did: nothing read the footer, and the support
// URL reached emails only (sweep 2026-10-21, C1 and C6). These tests render
// through BuildPDFContext, the one builder every invoice PDF (email,
// download, hosted) uses, and read the PDF back.

// renderedText renders inv through BuildPDFContext with ts as the live
// settings and returns every page.
func renderedText(t *testing.T, inv domain.Invoice, items []domain.InvoiceLineItem, ts domain.TenantSettings) []pdftest.Page {
	t.Helper()
	customers := &fakePDFCustomers{cust: domain.Customer{DisplayName: "Acme Corp"}}
	bt, ci, cns := BuildPDFContext(context.Background(), customers, &fakePDFSettings{ts: ts}, nil, "t1", &inv)
	out, err := RenderPDF(context.Background(), inv, items, bt, cns, ci)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return pdftest.Read(t, out)
}

func onAnyPage(pages []pdftest.Page, s string) bool {
	for _, p := range pages {
		if p.Has(s) {
			return true
		}
	}
	return false
}

// An issued invoice prints the footer it was issued with (inv.Footer), not
// the setting as it is now: a footer carries payment details and legal text,
// which belong to the issued document (ADR-109). A draft is a preview and
// prints the current setting, so the operator sees what finalizing will
// issue. The support URL is branding and is always the current one.
func TestRenderPDF_FooterAndSupportURL(t *testing.T) {
	ts := domain.TenantSettings{
		CompanyName:   "Seller Ltd",
		InvoiceFooter: "Pay by bank transfer.\nIBAN DE00 1234 5678 9000",
		SupportURL:    "https://seller.test/help",
	}
	const support = "Need help? https://seller.test/help"

	t.Run("draft prints the current footer", func(t *testing.T) {
		inv, items := longInvoice(2, domain.InvoiceDraft)
		pages := renderedText(t, inv, items, ts)
		for _, s := range []string{"Pay by bank transfer.", "IBAN DE00 1234 5678 9000", support} {
			if !onAnyPage(pages, s) {
				t.Errorf("draft PDF does not print %q", s)
			}
		}
	})

	t.Run("issued prints the footer it was issued with", func(t *testing.T) {
		inv, items := longInvoice(2, domain.InvoiceFinalized)
		inv.Footer = "Old bank: IBAN GB00 0000"
		pages := renderedText(t, inv, items, ts)
		if !onAnyPage(pages, "Old bank: IBAN GB00 0000") {
			t.Error("issued PDF does not print the invoice's own footer")
		}
		if onAnyPage(pages, "Pay by bank transfer.") {
			t.Error("issued PDF prints the current setting; a later edit must not change an issued invoice")
		}
		if !onAnyPage(pages, support) {
			t.Errorf("issued PDF does not print %q", support)
		}
	})

	t.Run("issued before footers were copied prints none", func(t *testing.T) {
		inv, items := longInvoice(2, domain.InvoicePaid)
		pages := renderedText(t, inv, items, ts)
		if onAnyPage(pages, "Pay by bank transfer.") {
			t.Error("an invoice issued with no footer gained the current one")
		}
	})

	t.Run("nothing set prints nothing", func(t *testing.T) {
		inv, items := longInvoice(2, domain.InvoiceDraft)
		pages := renderedText(t, inv, items, domain.TenantSettings{CompanyName: "Seller Ltd"})
		for _, p := range pages {
			for _, r := range p.Runs {
				if strings.HasPrefix(r.Text, "Need help?") {
					t.Errorf("support line %q printed with no support URL set", r.Text)
				}
			}
		}
	})
}

// A footer may be 1,000 characters across several lines. It wraps inside
// the page and moves to the next page with the rest of the closing block,
// at every invoice length that pushes that block toward a page break.
func TestRenderPDF_LongFooterStaysOnThePage(t *testing.T) {
	var words []string
	for i := 1; len(strings.Join(words, " ")) < 980; i++ {
		words = append(words, fmt.Sprintf("ftr%03d", i))
	}
	footer := strings.Join(words[:len(words)/2], " ") + "\n" + strings.Join(words[len(words)/2:], " ")
	ts := domain.TenantSettings{InvoiceFooter: footer, SupportURL: "https://seller.test/help"}
	for n := 20; n <= 45; n++ {
		t.Run(fmt.Sprintf("lines=%d", n), func(t *testing.T) {
			inv, items := longInvoice(n, domain.InvoiceDraft)
			pages := renderedText(t, inv, items, ts)
			checkPages(t, pages, items, formatCentsIn("$", inv.AmountDueCents))
			var printed []string
			for _, p := range pages {
				for _, r := range p.Runs {
					if strings.Contains(r.Text, "ftr") {
						printed = append(printed, strings.Fields(r.Text)...)
					}
				}
			}
			if got := strings.Join(printed, " "); got != strings.Join(words, " ") {
				t.Errorf("footer printed as %q, want every word once, in order", got)
			}
			if !onAnyPage(pages, "Need help? https://seller.test/help") {
				t.Error("support line missing")
			}
		})
	}
}
