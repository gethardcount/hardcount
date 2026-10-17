package creditnote

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/testutil/pdftest"
)

// The credit-note PDF used to call AddPage once and move down 18pt per line
// with no check, so a credit note for a long invoice lost its later lines
// and its Credit Total below the bottom edge. These tests read the rendered
// PDF back and check where each row landed.

// Where text may sit, in points from the page edges. "Page N of M" has its
// baseline about 26pt up from the bottom; everything else must stay above
// that band, or it collides with the number or runs off the page.
const (
	edgeGap   = 20.0
	bodyFloor = 40.0
)

func isPageNumber(r pdftest.Run) bool { return strings.HasPrefix(r.Text, "Page ") }

func longCreditNote(n int, status domain.CreditNoteStatus) (domain.CreditNote, []domain.CreditNoteLineItem) {
	issued := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	var items []domain.CreditNoteLineItem
	var total int64
	for i := 1; i <= n; i++ {
		amount := int64(i) * 1180
		items = append(items, domain.CreditNoteLineItem{
			Description:     fmt.Sprintf("Credit line %02d", i),
			Quantity:        int64(i),
			UnitAmountCents: 1180,
			AmountCents:     amount,
		})
		total += amount
	}
	tax := total * 18 / 118
	cn := domain.CreditNote{
		ID:                "vlx_cn_long",
		CreditNoteNumber:  "CN-202604-0042",
		InvoiceID:         "vlx_inv_long",
		CustomerID:        "cus_1",
		Status:            status,
		Reason:            "Service outage credit for incident 2026-04-10",
		SubtotalCents:     total - tax,
		TaxAmountCents:    tax,
		TotalCents:        total,
		RefundAmountCents: total,
		RefundStatus:      domain.RefundSucceeded,
		Currency:          "USD",
		IssuedAt:          &issued,
	}
	return cn, items
}

var longOrig = OriginalInvoiceInfo{
	Number:       "VLX-202604-0042",
	Currency:     "USD",
	TaxCountry:   "US",
	TaxName:      "Sales Tax",
	TaxRate:      18,
	ExemptReason: "Nonprofit certificate 123",
}

// checkPages asserts what every credit-note PDF must satisfy, whatever its
// length: all text inside a page; each line exactly once and in order; the
// table header above the first line on every page that has lines; and the
// totals, the settlement and the footer together on the last page, below
// the last line. A multi-page PDF numbers its pages and marks each page
// after the first as continued; a one-page PDF has no number.
func checkPages(t *testing.T, pages []pdftest.Page, items []domain.CreditNoteLineItem, creditTotal string) {
	t.Helper()
	if len(pages) == 0 {
		t.Fatal("no pages")
	}
	for _, m := range pdftest.Misplaced(pages, edgeGap, bodyFloor, isPageNumber) {
		t.Error(m)
	}

	type at struct {
		page int
		y    float64
	}
	var prev at
	for i, item := range items {
		var found []at
		for pi, p := range pages {
			for _, r := range p.Find(item.Description) {
				found = append(found, at{pi, r.Y})
			}
		}
		if len(found) != 1 {
			t.Fatalf("line %q found %d times, want once", item.Description, len(found))
		}
		cur := found[0]
		if i > 0 && (cur.page < prev.page || (cur.page == prev.page && cur.y >= prev.y)) {
			t.Errorf("line %q (page %d, y %.1f) is not after the line before it (page %d, y %.1f)", item.Description, cur.page+1, cur.y, prev.page+1, prev.y)
		}
		if i == 0 || cur.page != prev.page {
			hdr := pages[cur.page].Find("Description")
			if len(hdr) != 1 || hdr[0].Y <= cur.y {
				t.Errorf("page %d: no table header above its first line %q", cur.page+1, item.Description)
			}
		}
		prev = cur
	}

	last := pages[len(pages)-1]
	for _, label := range []string{"Total excluding tax", "Sales Tax (18%) [US]", "Credit Total", "SETTLEMENT", "Tax-exempt: Nonprofit certificate 123"} {
		for pi, p := range pages[:len(pages)-1] {
			if p.Has(label) {
				t.Errorf("%q is on page %d, not with the other totals on the last page", label, pi+1)
			}
		}
		runs := last.Find(label)
		if len(runs) != 1 {
			t.Fatalf("%q appears %d times on the last page, want once", label, len(runs))
		}
		if prev.page == len(pages)-1 && runs[0].Y >= prev.y {
			t.Errorf("%q (y %.1f) is not below the last line (y %.1f)", label, runs[0].Y, prev.y)
		}
	}
	total := last.Find("Credit Total")[0]
	amountOK := false
	for _, r := range last.Find(creditTotal) {
		if r.Y == total.Y {
			amountOK = true
		}
	}
	if !amountOK {
		t.Errorf("Credit Total row does not show %s on the last page", creditTotal)
	}
	generated := false
	for _, r := range last.Runs {
		generated = generated || strings.HasPrefix(r.Text, "Generated on ")
	}
	if !generated {
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
		if pi > 0 && !p.Has("Credit note CN-202604-0042 (continued)") {
			t.Errorf("page %d: no continued marker", pi+1)
		}
	}
}

// TestRenderPDF_PageBreaks renders 1 to 60 lines. 1 fits on a page; 60
// used to lose its later lines and its Credit Total. Every length in
// between moves the break to a different place in the totals block.
func TestRenderPDF_PageBreaks(t *testing.T) {
	t.Parallel()
	bt := BillToInfo{Name: "Acme Corp", Country: "US"}
	company := CompanyInfo{Name: "Seller Ltd", Country: "US"}
	for n := 1; n <= 60; n++ {
		t.Run(fmt.Sprintf("lines=%d", n), func(t *testing.T) {
			cn, items := longCreditNote(n, domain.CreditNoteIssued)
			out, err := RenderPDF(context.Background(), cn, items, longOrig, bt, company)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			pages := pdftest.Read(t, out)
			checkPages(t, pages, items, cnFormatCents(cn.TotalCents, "$"))
			switch {
			case n == 1 && len(pages) != 1:
				t.Errorf("1 line: %d pages, want 1", len(pages))
			case n >= 36 && len(pages) < 2:
				t.Errorf("%d lines: %d page, want more than one", n, len(pages))
			}
		})
	}
}

// TestRenderPDF_PageBreaks_LongReason: the reason has no length limit, so
// a long one must break across pages rather than run off the first.
func TestRenderPDF_PageBreaks_LongReason(t *testing.T) {
	t.Parallel()
	cn, items := longCreditNote(5, domain.CreditNoteIssued)
	var words []string
	for i := 1; i <= 900; i++ {
		words = append(words, fmt.Sprintf("w%03d", i))
	}
	cn.Reason = strings.Join(words, " ")
	out, err := RenderPDF(context.Background(), cn, items, longOrig, BillToInfo{Name: "Acme Corp"}, CompanyInfo{Name: "Seller Ltd"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	pages := pdftest.Read(t, out)
	if len(pages) < 2 {
		t.Fatalf("%d page, want the reason to run onto a second", len(pages))
	}
	checkPages(t, pages, items, cnFormatCents(cn.TotalCents, "$"))
	var text strings.Builder
	for _, p := range pages {
		for _, r := range p.Runs {
			text.WriteString(r.Text + " ")
		}
	}
	if !strings.Contains(text.String(), "w900") {
		t.Error("the end of the reason is on no page")
	}
}

// TestRenderPDF_PageBreaks_VoidEveryPage: a voided credit note carries its
// watermark on every page.
func TestRenderPDF_PageBreaks_VoidEveryPage(t *testing.T) {
	t.Parallel()
	cn, items := longCreditNote(60, domain.CreditNoteVoided)
	out, err := RenderPDF(context.Background(), cn, items, longOrig, BillToInfo{Name: "Acme Corp"}, CompanyInfo{Name: "Seller Ltd"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	pages := pdftest.Read(t, out)
	if len(pages) < 2 {
		t.Fatalf("%d page, want more than one", len(pages))
	}
	for pi, p := range pages {
		if !p.Has("VOID") {
			t.Errorf("page %d has no VOID watermark", pi+1)
		}
	}
}

// TestCurrencySymbols_HaveGlyphs: a symbol the embedded font cannot draw
// prints as nothing, so the amount loses its currency. THB's ฿ did that.
func TestCurrencySymbols_HaveGlyphs(t *testing.T) {
	t.Parallel()
	for code, sym := range cnCurrencySymbols {
		if missing := pdftest.MissingGlyphs(t, sym); len(missing) > 0 {
			t.Errorf("%s symbol %q: the PDF font has no glyph for %q", code, sym, missing)
		}
	}
}

// TestRenderPDF_THBAmountHasCurrency renders a THB credit note and reads
// the total back with its currency.
func TestRenderPDF_THBAmountHasCurrency(t *testing.T) {
	t.Parallel()
	cn, items := longCreditNote(1, domain.CreditNoteIssued)
	cn.Currency = "THB"
	cn.TotalCents = 123456
	out, err := RenderPDF(context.Background(), cn, items, longOrig, BillToInfo{Name: "Acme Corp"}, CompanyInfo{Name: "Seller Ltd"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if p := pdftest.Read(t, out)[0]; !p.Has("THB 1,234.56") {
		t.Errorf("Credit Total does not read %q", "THB 1,234.56")
	}
}
