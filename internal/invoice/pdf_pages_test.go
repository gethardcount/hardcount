package invoice

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/testutil/pdftest"
)

// The invoice PDF used to call AddPage once and move down 18pt per line
// item with no check, so on a 60-line invoice rows 37-60 and the totals
// were drawn below the bottom edge: in the file, but on no page. These tests
// read the rendered PDF back and check where each row landed.

// Where text may sit, in points from the page edges. "Page N of M" has its
// baseline about 26pt up from the bottom; everything else must stay above
// that band, or it collides with the number or runs off the page.
const (
	edgeGap   = 20.0
	bodyFloor = 40.0
)

func isPageNumber(r pdftest.Run) bool { return strings.HasPrefix(r.Text, "Page ") }

func longInvoice(n int, status domain.InvoiceStatus) (domain.Invoice, []domain.InvoiceLineItem) {
	issued := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	due := issued.AddDate(0, 0, 30)
	var items []domain.InvoiceLineItem
	var sub int64
	for i := 1; i <= n; i++ {
		amount := int64(i) * 1000
		items = append(items, domain.InvoiceLineItem{
			LineType:        domain.LineTypeUsage,
			Description:     fmt.Sprintf("Usage line %02d", i),
			Quantity:        int64(i),
			UnitAmountCents: 1000,
			AmountCents:     amount,
		})
		sub += amount
	}
	inv := domain.Invoice{
		ID:                 "hc_inv_long",
		InvoiceNumber:      "HC-202604-0042",
		Status:             status,
		PaymentStatus:      domain.PaymentPending,
		Currency:           "USD",
		SubtotalCents:      sub,
		TotalAmountCents:   sub,
		AmountDueCents:     sub,
		IssuedAt:           &issued,
		DueAt:              &due,
		NetPaymentTermDays: 30,
	}
	return inv, items
}

// textGap is how close two pieces of text on one line may come, in points.
const textGap = 4.0

// isWatermark: the VOID or DRAFT mark sits behind the text on purpose.
func isWatermark(r pdftest.Run) bool { return r.Size >= 72 }

// isTableChrome: what a page break adds in the description column.
func isTableChrome(r pdftest.Run) bool {
	return r.Text == "Description" || strings.HasSuffix(r.Text, "(continued)")
}

// descriptionLines finds each line item's description, in order, in the
// description column, and returns the lines it was drawn on. The test
// fails if one is not printed whole (#1228).
func descriptionLines(t *testing.T, pages []pdftest.Page, items []domain.InvoiceLineItem) [][]pdftest.Line {
	t.Helper()
	descs := make([]string, len(items))
	for i, item := range items {
		descs[i] = item.Description
	}
	return pdftest.FindWrapped(t, pages, descColX, descs, isTableChrome)
}

// Where the description column starts, and where it ends: the Qty column
// starts there.
const (
	descColX   = 40.0
	descColEnd = 280.0
)

// checkPages asserts what every invoice PDF must satisfy, whatever its
// length: all text inside a page and none drawn over other text; each
// line item's description printed whole, in order, inside its column, its
// lines one under another on one page (a one-line description exactly
// once); the table header above the first row on every page that has
// rows, and on no other page; and Subtotal, Total and Amount Due (with its
// amount) together on the last page, below the last row. A multi-page PDF
// numbers its pages and marks each page after the first as continued; a
// one-page PDF has no number.
func checkPages(t *testing.T, pages []pdftest.Page, items []domain.InvoiceLineItem, amountDue string) {
	t.Helper()
	if len(pages) == 0 {
		t.Fatal("no pages")
	}
	for _, m := range pdftest.Misplaced(pages, edgeGap, bodyFloor, isPageNumber) {
		t.Error(m)
	}
	for _, m := range pdftest.Overlapping(pages, textGap, isWatermark) {
		t.Error(m)
	}

	type at struct {
		page int
		y    float64
	}
	var prev at
	rows := descriptionLines(t, pages, items)
	for pi, p := range pages {
		hdr := p.Find("Description")
		if len(hdr) == 0 {
			continue
		}
		under := false
		for _, lines := range rows {
			for _, l := range lines {
				under = under || (l.Page == pi && l.Y < hdr[0].Y)
			}
		}
		if !under && len(items) > 0 {
			t.Errorf("page %d: the table header has no line under it", pi+1)
		}
	}
	for i, lines := range rows {
		item := items[i]
		if len(lines) == 1 {
			found := 0
			for _, p := range pages {
				found += len(p.Find(item.Description))
			}
			if found != 1 {
				t.Fatalf("line %q found %d times, want once", item.Description, found)
			}
		}
		for j, l := range lines {
			if l.X+l.W > descColEnd {
				t.Errorf("line %q: %q ends at x %.1f, past the description column (%.0f)", item.Description, l.Text, l.X+l.W, descColEnd)
			}
			if j > 0 && (l.Page != lines[0].Page || l.Y >= lines[j-1].Y) {
				t.Errorf("line %q: %q (page %d, y %.1f) is not under the line before it on the same page", item.Description, l.Text, l.Page+1, l.Y)
			}
		}
		cur := at{lines[0].Page, lines[0].Y}
		if i > 0 && (cur.page < prev.page || (cur.page == prev.page && cur.y >= prev.y)) {
			t.Errorf("line %q (page %d, y %.1f) is not after the line before it (page %d, y %.1f)", item.Description, cur.page+1, cur.y, prev.page+1, prev.y)
		}
		if i == 0 || cur.page != prev.page {
			hdr := pages[cur.page].Find("Description")
			if len(hdr) != 1 || hdr[0].Y <= cur.y {
				t.Errorf("page %d: no table header above its first line %q", cur.page+1, item.Description)
			}
		}
		last := lines[len(lines)-1]
		prev = at{last.Page, last.Y}
	}

	last := pages[len(pages)-1]
	for _, label := range []string{"Subtotal", "Total", "Amount Due"} {
		for pi, p := range pages[:len(pages)-1] {
			if p.Has(label) {
				t.Errorf("%q is on page %d, not with the other totals on the last page", label, pi+1)
			}
		}
		runs := last.Find(label)
		if len(runs) != 1 {
			t.Fatalf("%q appears %d times on the last page, want once", label, len(runs))
		}
		if len(items) > 0 && prev.page == len(pages)-1 && runs[0].Y >= prev.y {
			t.Errorf("%q (y %.1f) is not below the last line (y %.1f)", label, runs[0].Y, prev.y)
		}
	}
	due := last.Find("Amount Due")[0]
	amountOK := false
	for _, r := range last.Find(amountDue) {
		if r.Y == due.Y {
			amountOK = true
		}
	}
	if !amountOK {
		t.Errorf("Amount Due row does not show %s on the last page", amountDue)
	}
	if !hasPrefix(last, "Generated on ") {
		t.Error("the Generated on footer is not on the last page")
	}

	n := len(pages)
	for pi, p := range pages {
		var numbers []string
		for _, r := range p.Runs {
			if strings.HasPrefix(r.Text, "Page ") {
				numbers = append(numbers, r.Text)
			}
		}
		want := []string{fmt.Sprintf("Page %d of %d", pi+1, n)}
		if n == 1 {
			want = nil
		}
		if fmt.Sprint(numbers) != fmt.Sprint(want) {
			t.Errorf("page %d: page numbers %q, want %q", pi+1, numbers, want)
		}
		if pi > 0 && !p.Has("Invoice HC-202604-0042 (continued)") {
			t.Errorf("page %d: no continued marker", pi+1)
		}
	}
}

// TestRenderPDF_PageBreaks renders 1 to 60 line items. 1 fits on a page;
// 36 is where the totals used to fall off; 60 is where rows did too.
// Every length in between moves the page break to a different place in the
// totals and footer, which is where a block could split.
func TestRenderPDF_PageBreaks(t *testing.T) {
	for n := 1; n <= 60; n++ {
		t.Run(fmt.Sprintf("lines=%d", n), func(t *testing.T) {
			inv, items := longInvoice(n, domain.InvoiceFinalized)
			out, err := RenderPDF(context.Background(), inv, items, BillToInfo{Name: "Acme Corp"}, nil, CompanyInfo{Name: "Seller Ltd"})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			pages := pdftest.Read(t, out)
			checkPages(t, pages, items, formatCentsIn("$", inv.AmountDueCents))
			switch {
			case n == 1 && len(pages) != 1:
				t.Errorf("1 line: %d pages, want 1", len(pages))
			case n >= 36 && len(pages) < 2:
				t.Errorf("%d lines: %d page, want more than one", n, len(pages))
			}
		})
	}
}

// TestRenderPDF_PageBreaks_FullTotals fills every optional part of the
// totals and what follows (discount, tax by jurisdiction, a credit note
// before and after payment, prepaid credits, amount paid, legends, memo)
// and checks no part leaves the page at any break point.
func TestRenderPDF_PageBreaks_FullTotals(t *testing.T) {
	paid := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	for n := 20; n <= 60; n++ {
		t.Run(fmt.Sprintf("lines=%d", n), func(t *testing.T) {
			inv, items := longInvoice(n, domain.InvoiceFinalized)
			for i := range items {
				items[i].TaxAmountCents = 100
				items[i].TaxJurisdiction = []string{"US-CA", "US-NY", "US-TX"}[i%3]
				items[i].TaxRate = 7.25
			}
			items[0].TaxabilityReason = "product_exempt"
			inv.DiscountCents = 500
			inv.TaxAmountCents = int64(n) * 100
			inv.TaxName = "Sales Tax"
			inv.TaxExemptReason = "Nonprofit certificate 123"
			inv.CreditsAppliedCents = 700
			inv.AmountPaidCents = 900
			inv.AmountDueCents = 4321
			inv.Memo = "Thank you for your business."
			inv.PaidAt = &paid
			cns := []CreditNoteInfo{
				{Number: "CN-0001", Reason: "Duplicate line", Amount: 1000},
				{Number: "CN-0002", Reason: "Outage credit", Amount: 2000, CreditAmountCents: 2000},
				{Number: "CN-0003", Reason: "Refund", Amount: 3000, RefundAmountCents: 3000, RefundStatus: string(domain.RefundSucceeded), TaxAmountCents: 300},
			}
			out, err := RenderPDF(context.Background(), inv, items, BillToInfo{Name: "Acme Corp"}, cns, CompanyInfo{Name: "Seller Ltd"})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			pages := pdftest.Read(t, out)
			checkPages(t, pages, items, "$43.21")
			last := pages[len(pages)-1]
			for _, s := range []string{"Discount", "Prepaid credits", "Amount Paid", "CN-0001 - Duplicate line", "POST-PAYMENT ADJUSTMENTS", "Tax-exempt: Nonprofit certificate 123", "Thank you for your business."} {
				found := false
				for _, p := range pages {
					found = found || p.Has(s)
				}
				if !found {
					t.Errorf("%q is on no page", s)
				}
			}
			if !last.Has("Thank you for your business.") {
				t.Error("the memo is not on the last page")
			}
		})
	}
}

// TestRenderPDF_PageBreaks_TallTotals: totals taller than a page (60 tax
// jurisdictions, one row each) cannot stay on one page, so they break row
// by row. Nothing may leave the page, and Amount Due still ends the totals.
func TestRenderPDF_PageBreaks_TallTotals(t *testing.T) {
	inv, items := longInvoice(60, domain.InvoiceFinalized)
	for i := range items {
		items[i].TaxAmountCents = 100
		items[i].TaxJurisdiction = fmt.Sprintf("US-Z%02d", i+1)
	}
	inv.TaxAmountCents = 6000
	out, err := RenderPDF(context.Background(), inv, items, BillToInfo{Name: "Acme Corp"}, nil, CompanyInfo{Name: "Seller Ltd"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	pages := pdftest.Read(t, out)
	for _, m := range pdftest.Misplaced(pages, edgeGap, bodyFloor, isPageNumber) {
		t.Error(m)
	}
	for i := 1; i <= 60; i++ {
		found := 0
		for _, p := range pages {
			found += len(p.Find(fmt.Sprintf("US-Z%02d", i)))
		}
		if found != 1 {
			t.Errorf("jurisdiction row US-Z%02d found %d times, want once", i, found)
		}
	}
	last := pages[len(pages)-1]
	if !last.Has("Amount Due") || !last.Has(formatCentsIn("$", inv.AmountDueCents)) {
		t.Error("Amount Due is not on the last page")
	}
}

// TestRenderPDF_PageBreaks_ManyAdjustments: dozens of post-payment credit
// notes make what follows the table taller than a page. It then breaks
// between adjustments: each adjustment's lines stay on one page, the
// legend and the footer stay whole, and nothing leaves the page. A page
// holds 21 adjustments, so sweeping 23 counts puts the last break at every
// point relative to the legend and the footer.
func TestRenderPDF_PageBreaks_ManyAdjustments(t *testing.T) {
	for k := 30; k <= 52; k++ {
		t.Run(fmt.Sprintf("adjustments=%d", k), func(t *testing.T) {
			inv, items := longInvoice(10, domain.InvoiceFinalized)
			// All three legends: five lines, taller than the space a page
			// of adjustments leaves at its foot.
			items[0].TaxabilityReason = "customer_exempt"
			items[1].TaxabilityReason = "product_exempt"
			inv.TaxReverseCharge = true
			inv.TaxExemptReason = "Nonprofit certificate 123"
			inv.Memo = "Thank you for your business."
			var cns []CreditNoteInfo
			for i := 1; i <= k; i++ {
				cns = append(cns, CreditNoteInfo{Number: fmt.Sprintf("CN-%04d", i), Reason: "Credit", Amount: 100, CreditAmountCents: 100, TaxAmountCents: 10})
			}
			out, err := RenderPDF(context.Background(), inv, items, BillToInfo{Name: "Acme Corp"}, cns, CompanyInfo{Name: "Seller Ltd"})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			pages := pdftest.Read(t, out)
			for _, m := range pdftest.Misplaced(pages, edgeGap, bodyFloor, isPageNumber) {
				t.Error(m)
			}
			pageOf := func(s string) []int {
				var at []int
				for pi, p := range pages {
					for range p.Find(s) {
						at = append(at, pi)
					}
				}
				return at
			}
			if at := pageOf("Amount Due"); len(at) != 1 {
				t.Fatalf("Amount Due on pages %v, want one", at)
			}
			for i := 1; i <= k; i++ {
				head := pageOf(fmt.Sprintf("CN-%04d - Credit", i))
				if len(head) != 1 {
					t.Fatalf("CN-%04d found %d times, want once", i, len(head))
				}
				// Each adjustment's channel and tax lines read the same for
				// every CN here, so check the page has at least as many of
				// them as adjustment headers.
				p := pages[head[0]]
				heads, chans := 0, len(p.Find("$1.00 to credit"))
				for _, r := range p.Runs {
					if strings.HasPrefix(r.Text, "CN-") {
						heads++
					}
				}
				if chans != heads {
					t.Errorf("page %d: %d adjustment headers but %d channel lines", head[0]+1, heads, chans)
				}
			}
			legend := pageOf("Tax-exempt: Nonprofit certificate 123")
			memo := pageOf("Thank you for your business.")
			if len(legend) != 1 || len(memo) != 1 || memo[0] != len(pages)-1 {
				t.Errorf("legend on pages %v and memo on pages %v; want each once, the memo on the last page", legend, memo)
			}
		})
	}
}

// TestRenderPDF_PageBreaks_WatermarkEveryPage: a voided or draft invoice
// carries its watermark on every page, so no page of it reads as live.
func TestRenderPDF_PageBreaks_WatermarkEveryPage(t *testing.T) {
	for status, mark := range map[domain.InvoiceStatus]string{domain.InvoiceVoided: "VOID", domain.InvoiceDraft: "DRAFT"} {
		inv, items := longInvoice(60, status)
		out, err := RenderPDF(context.Background(), inv, items, BillToInfo{Name: "Acme Corp"}, nil, CompanyInfo{Name: "Seller Ltd"})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		pages := pdftest.Read(t, out)
		if len(pages) < 2 {
			t.Fatalf("%s: %d page, want more than one", status, len(pages))
		}
		for pi, p := range pages {
			if !p.Has(mark) {
				t.Errorf("%s: page %d has no %s watermark", status, pi+1, mark)
			}
		}
	}
}

func hasPrefix(p pdftest.Page, prefix string) bool {
	for _, r := range p.Runs {
		if strings.HasPrefix(r.Text, prefix) {
			return true
		}
	}
	return false
}

// TestCurrencySymbols_HaveGlyphs: a symbol the embedded font cannot draw
// prints as nothing, so the amount loses its currency. THB's ฿ did that.
func TestCurrencySymbols_HaveGlyphs(t *testing.T) {
	for code, sym := range currencySymbols {
		if missing := pdftest.MissingGlyphs(t, sym); len(missing) > 0 {
			t.Errorf("%s symbol %q: the PDF font has no glyph for %q", code, sym, missing)
		}
	}
}

// TestRenderPDF_THBAmountHasCurrency renders a THB invoice and reads the
// amount back with its currency.
func TestRenderPDF_THBAmountHasCurrency(t *testing.T) {
	inv, items := longInvoice(1, domain.InvoiceFinalized)
	inv.Currency = "THB"
	inv.AmountDueCents = 123456
	out, err := RenderPDF(context.Background(), inv, items, BillToInfo{Name: "Acme Corp"}, nil, CompanyInfo{Name: "Seller Ltd"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if p := pdftest.Read(t, out)[0]; !p.Has("THB 1,234.56") {
		t.Errorf("Amount Due does not read %q", "THB 1,234.56")
	}
}
