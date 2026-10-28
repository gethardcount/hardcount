package creditnote

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/testutil/pdftest"
)

// A line description over 50 characters used to print as its first 47 and
// "..." (#1228), and the totals block was 140pt wide with its amounts
// right-aligned 75pt short of the Amount column, so "Total excluding tax"
// ran into amounts of $10,000 or more (#1178). These tests read the
// rendered PDF back. checkPages finds every description whole and checks
// no text is drawn over other text.

// longDesc is 120 characters, an AI usage line of the kind #1228 names.
const longDesc = "Claude Sonnet 4 output tokens, batch API, workspace research-eu-west1, project atlas-retrieval, April 1 to April 30 2026"

// TestRenderPDF_LongDescriptionWraps: a 120-character description and a
// 180-character one with no space print whole, wrapped inside their
// column, with the line's amount beside the first line. A short
// description stays on one line.
func TestRenderPDF_LongDescriptionWraps(t *testing.T) {
	t.Parallel()
	if n := utf8.RuneCountInString(longDesc); n != 120 {
		t.Fatalf("longDesc is %d characters, want 120", n)
	}
	cn, items := longCreditNote(3, domain.CreditNoteIssued)
	items[0].Description = longDesc
	items[1].Description = "ref " + strings.Repeat("0123456789", 18)
	// items[2] keeps "Credit line 03", short enough for one line.
	out, err := RenderPDF(context.Background(), cn, items, longOrig, BillToInfo{Name: "Acme Corp"}, CompanyInfo{Name: "Seller Ltd"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	pages := pdftest.Read(t, out)
	checkPages(t, pages, items, cnFormatCents(cn.TotalCents, "$"))

	rows := descriptionLines(t, pages, items)
	for i, wrapped := range []bool{true, true, false} {
		if got := len(rows[i]); wrapped != (got > 1) {
			t.Errorf("line %q takes %d lines; wrapped: %v, want %v", items[i].Description, got, got > 1, wrapped)
		}
		amount := cnFormatCents(items[i].AmountCents, "$")
		first := rows[i][0]
		beside := false
		for _, r := range pages[first.Page].Find(amount) {
			beside = beside || (r.Y == first.Y && r.X > descColEnd)
		}
		if !beside {
			t.Errorf("line %q: its amount %s is not beside its first line", items[i].Description, amount)
		}
	}
}

// TestRenderPDF_PageBreaks_WrappedRows: rows of three lines each, from 1
// to 40 of them. A row grows with its wrapped description, and the page
// break uses that height, so a row never splits over two pages and the
// next row never prints over its last lines. Every count puts the break at
// a different line of a row.
func TestRenderPDF_PageBreaks_WrappedRows(t *testing.T) {
	t.Parallel()
	for n := 1; n <= 40; n++ {
		t.Run(fmt.Sprintf("rows=%d", n), func(t *testing.T) {
			cn, items := longCreditNote(n, domain.CreditNoteIssued)
			for i := range items {
				items[i].Description = fmt.Sprintf("Row %02d: %s, %s", i+1, longDesc, "cache reads included")
			}
			out, err := RenderPDF(context.Background(), cn, items, longOrig, BillToInfo{Name: "Acme Corp"}, CompanyInfo{Name: "Seller Ltd"})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			pages := pdftest.Read(t, out)
			checkPages(t, pages, items, cnFormatCents(cn.TotalCents, "$"))
			for i, lines := range descriptionLines(t, pages, items) {
				if len(lines) < 3 {
					t.Fatalf("row %d takes %d lines, want 3 or more", i+1, len(lines))
				}
			}
			if n == 40 && len(pages) < 3 {
				t.Errorf("40 rows of 3 lines: %d pages, want 3 or more", len(pages))
			}
		})
	}
}

// words is n short words, about 9 to a wrapped line.
func words(n int) string {
	w := make([]string, n)
	for i := range w {
		w[i] = fmt.Sprintf("w%03d", i+1)
	}
	return strings.Join(w, " ")
}

// TestRenderPDF_PageBreaks_TallFirstRow: the table header stays with the
// first row, by the row's real height. A first row of about 43 to 58
// lines fits on a page, but past some height not in what page 1 has left
// under the details and the reason; header and row then open page 2
// together, and no header is left alone on page 1.
func TestRenderPDF_PageBreaks_TallFirstRow(t *testing.T) {
	t.Parallel()
	stayed, moved := false, false
	for n := 380; n <= 520; n += 10 {
		t.Run(fmt.Sprintf("words=%d", n), func(t *testing.T) {
			cn, items := longCreditNote(2, domain.CreditNoteIssued)
			items[0].Description = words(n)
			out, err := RenderPDF(context.Background(), cn, items, longOrig, BillToInfo{Name: "Acme Corp"}, CompanyInfo{Name: "Seller Ltd"})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			pages := pdftest.Read(t, out)
			checkPages(t, pages, items, cnFormatCents(cn.TotalCents, "$"))
			first := descriptionLines(t, pages, items)[0]
			if len(first) > 58 {
				t.Fatalf("the first row takes %d lines, more than a page holds; this sweep is for rows that fit", len(first))
			}
			stayed = stayed || first[0].Page == 0
			moved = moved || first[0].Page == 1
		})
	}
	if !stayed || !moved {
		t.Errorf("first rows stayed on page 1: %v, moved to page 2: %v; the sweep must cross the break", stayed, moved)
	}
}

// TestRenderPDF_DescriptionTallerThanPage: a description has no length
// limit. One taller than a whole page starts where it is and carries on
// over the page break, under the repeated table header, and the lines
// after it follow.
func TestRenderPDF_DescriptionTallerThanPage(t *testing.T) {
	t.Parallel()
	cn, items := longCreditNote(3, domain.CreditNoteIssued)
	items[1].Description = words(900)
	out, err := RenderPDF(context.Background(), cn, items, longOrig, BillToInfo{Name: "Acme Corp"}, CompanyInfo{Name: "Seller Ltd"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	pages := pdftest.Read(t, out)
	if len(pages) < 2 {
		t.Fatalf("%d page, want the description to run onto a second", len(pages))
	}
	for _, m := range pdftest.Misplaced(pages, edgeGap, bodyFloor, isPageNumber) {
		t.Error(m)
	}
	for _, m := range pdftest.Overlapping(pages, textGap, isWatermark) {
		t.Error(m)
	}
	rows := descriptionLines(t, pages, items)
	long := rows[1]
	if rows[0][0].Page != 0 || long[0].Page != 0 {
		t.Errorf("the lines start on pages %d and %d, want both on page 1", rows[0][0].Page+1, long[0].Page+1)
	}
	for j, l := range long {
		if l.X+l.W > descColEnd {
			t.Errorf("%q ends at x %.1f, past the description column (%.0f)", l.Text, l.X+l.W, descColEnd)
		}
		if j > 0 && l.Page == long[j-1].Page && l.Y >= long[j-1].Y {
			t.Errorf("%q (page %d, y %.1f) is not under the line before it", l.Text, l.Page+1, l.Y)
		}
		if j > 0 && l.Page != long[j-1].Page {
			hdr := pages[l.Page].Find("Description")
			if len(hdr) != 1 || hdr[0].Y <= l.Y {
				t.Errorf("page %d: no table header above the description's next line", l.Page+1)
			}
		}
	}
	end := long[len(long)-1]
	next := rows[2][0]
	if next.Page < end.Page || (next.Page == end.Page && next.Y >= end.Y) {
		t.Errorf("the line after the long one (page %d, y %.1f) is not after its last line (page %d, y %.1f)", next.Page+1, next.Y, end.Page+1, end.Y)
	}
	if last := pages[len(pages)-1]; !last.Has("Credit Total") {
		t.Error("Credit Total is not on the last page")
	}
}

// TestRenderPDF_TotalsDoNotOverlap checks no totals label runs into its
// amount. Small amounts are the control: they fit before and after #1178.
// Amounts of $10,000 and more, and a long tax name, used to overlap; with
// both at once the tax label wraps.
func TestRenderPDF_TotalsDoNotOverlap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		scale   int64
		taxName string
	}{
		{"small amounts", 1, "Sales Tax"},
		{"amounts of $10,000 and more", 1000, "Sales Tax"},
		{"long tax name", 1, "Goods and Services Tax"},
		{"long tax name and large amounts", 1000, "Goods and Services Tax"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cn, items := longCreditNote(3, domain.CreditNoteIssued)
			cn.SubtotalCents = 12345 * tc.scale
			cn.TaxAmountCents = 2222 * tc.scale
			cn.TotalCents = cn.SubtotalCents + cn.TaxAmountCents
			cn.RefundAmountCents = cn.TotalCents
			orig := longOrig
			orig.TaxName = tc.taxName
			out, err := RenderPDF(context.Background(), cn, items, orig, BillToInfo{Name: "Acme Corp"}, CompanyInfo{Name: "Seller Ltd"})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			pages := pdftest.Read(t, out)
			for _, m := range pdftest.Misplaced(pages, edgeGap, bodyFloor, isPageNumber) {
				t.Error(m)
			}
			for _, m := range pdftest.Overlapping(pages, textGap, isWatermark) {
				t.Error(m)
			}
			labels := pdftest.FindWrapped(t, pages, 340, []string{"Total excluding tax", tc.taxName + " (18%) [US]", "Credit Total"}, isTableChrome)
			for i, amount := range []int64{cn.SubtotalCents, cn.TaxAmountCents, cn.TotalCents} {
				want := cnFormatCents(amount, "$")
				first := labels[i][0]
				beside := false
				for _, r := range pages[first.Page].Find(want) {
					beside = beside || r.Y == first.Y
				}
				if !beside {
					t.Errorf("%q: its amount %s is not beside it", labels[i][0].Text, want)
				}
			}
		})
	}
}
