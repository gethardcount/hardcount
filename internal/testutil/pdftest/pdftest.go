// Package pdftest reads back what Hardcount's PDF renderers drew, for tests.
//
// Read returns each page's text with its position. Position matters: text
// drawn below a page's bottom edge is still in the file, so finding a
// string in the file does not prove a reader can see it. A test must check
// that the string sits inside a page.
//
// Overlapping reports text drawn over other text, such as a long label
// running into its amount.
//
// MissingGlyphs reports the characters the embedded fonts cannot draw, so a
// test can prove a string the renderers print never shows up blank.
package pdftest

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
	"github.com/signintech/gopdf"

	"github.com/gethardcount/hardcount/internal/pdffonts"
)

// Run is the text of one text-drawing operation. X and Y are in points from
// the page's bottom-left corner; Y is the text's baseline.
type Run struct {
	X, Y float64
	Text string
	// Font is the font family the run was drawn in (pdffonts.FamilyRegular
	// or pdffonts.FamilyBold) and Size its size in points.
	Font string
	Size float64
	// W is the run's width in points. The reader reports no widths for the
	// fonts the renderers embed, so Read measures each run in the embedded
	// font it was drawn in, at its size, as the renderers measure text.
	W float64
}

// Page is one page's size and its runs, in the order they were drawn.
type Page struct {
	Width, Height float64
	Runs          []Run
}

// Find returns the runs whose text is exactly s.
func (p Page) Find(s string) []Run {
	var out []Run
	for _, r := range p.Runs {
		if r.Text == s {
			out = append(out, r)
		}
	}
	return out
}

// Has reports whether some run's text is exactly s.
func (p Page) Has(s string) bool { return len(p.Find(s)) > 0 }

// Misplaced describes each run that is not where a reader can see it: off
// the page sideways, within edge points of the top, or with its baseline
// under floor points from the bottom. footer names the runs that belong in
// that bottom band (such as a page number); they need only clear edge.
func Misplaced(pages []Page, edge, floor float64, footer func(Run) bool) []string {
	var out []string
	for pi, p := range pages {
		for _, r := range p.Runs {
			bottom := floor
			if footer(r) {
				bottom = edge
			}
			if r.X < 0 || r.X > p.Width || r.Y > p.Height-edge || r.Y < bottom {
				out = append(out, fmt.Sprintf("page %d: %q drawn at (%.1f, %.1f) on a %.0fx%.0f page", pi+1, r.Text, r.X, r.Y, p.Width, p.Height))
			}
		}
	}
	return out
}

// Overlapping describes each pair of runs on one page drawn over each
// other: their lines share height, and sideways they come closer than gap
// points. A label that runs into its amount shows up here, and so does a
// wrapped line drawn over the next row. skip names runs that sit behind the
// text on purpose, such as a watermark.
func Overlapping(pages []Page, gap float64, skip func(Run) bool) []string {
	var out []string
	for pi, p := range pages {
		for i, a := range p.Runs {
			if skip(a) {
				continue
			}
			for _, b := range p.Runs[i+1:] {
				if skip(b) {
					continue
				}
				// A line of text takes about a quarter of its size below the
				// baseline (descenders) and three quarters above (capitals).
				if a.Y-a.Size/4 >= b.Y+b.Size*3/4 || b.Y-b.Size/4 >= a.Y+a.Size*3/4 {
					continue
				}
				if a.X+a.W+gap <= b.X || b.X+b.W+gap <= a.X {
					continue
				}
				out = append(out, fmt.Sprintf("page %d: %q (x %.1f to %.1f, y %.1f) and %q (x %.1f to %.1f, y %.1f) are closer than %.0fpt",
					pi+1, a.Text, a.X, a.X+a.W, a.Y, b.Text, b.X, b.X+b.W, b.Y, gap))
			}
		}
	}
	return out
}

// Line is one run and the index of the page it is on.
type Line struct {
	Page int
	Run
}

// FindWrapped finds texts, in order, among the runs drawn at x, and returns
// the lines each one was drawn on. A renderer wraps a long text onto more
// lines, so a text may span several runs in a row; they are joined back
// with the space the wrap dropped, or with none where a word too long for a
// line broke inside. skip names runs at x that belong to no text, such as a
// table header repeated on a new page. The test fails at the first text
// that is not printed whole.
func FindWrapped(t testing.TB, pages []Page, x float64, texts []string, skip func(Run) bool) [][]Line {
	t.Helper()
	var col []Line
	for pi, p := range pages {
		for _, r := range p.Runs {
			if math.Abs(r.X-x) < 0.01 && r.Text != "" && !skip(r) {
				col = append(col, Line{Page: pi, Run: r})
			}
		}
	}
	out := make([][]Line, 0, len(texts))
	k := 0
	for _, text := range texts {
		for k < len(col) && !strings.HasPrefix(text, col[k].Text) {
			k++
		}
		if k == len(col) {
			t.Fatalf("pdftest: %q: no line at x %.1f starts it (after the texts before it)", text, x)
		}
		got, lines := col[k].Text, []Line{col[k]}
		for k++; got != text; k++ {
			if k == len(col) {
				t.Fatalf("pdftest: %q is printed only up to %q", text, got)
			}
			switch next := col[k].Text; {
			case strings.HasPrefix(text, got+" "+next):
				got += " " + next
			case strings.HasPrefix(text, got+next):
				got += next
			default:
				t.Fatalf("pdftest: %q is printed only up to %q; the next line at x %.1f reads %q", text, got, x, next)
			}
			lines = append(lines, col[k])
		}
		out = append(out, lines)
	}
	return out
}

// Read parses a PDF and returns its pages.
//
// The reader reports one item per glyph. For the fonts the renderers embed
// it does not move X between the glyphs of one operation, so consecutive
// glyphs at the same X and Y are joined into one run. If a later version of
// the reader starts moving X, runs come back one glyph long and every
// exact-text lookup fails loudly rather than passing.
func Read(t testing.TB, pdfBytes []byte) []Page {
	t.Helper()
	r, err := pdf.NewReader(bytes.NewReader(pdfBytes), int64(len(pdfBytes)))
	if err != nil {
		t.Fatalf("pdftest: open pdf: %v", err)
	}
	measure := newMeasurer(t)
	pages := make([]Page, 0, r.NumPage())
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		// MediaBox may be set on the page or inherited from a parent.
		var mb pdf.Value
		for v := p.V; !v.IsNull() && mb.IsNull(); v = v.Key("Parent") {
			mb = v.Key("MediaBox")
		}
		if mb.Len() != 4 {
			t.Fatalf("pdftest: page %d has no MediaBox", i)
		}
		page := Page{
			Width:  mb.Index(2).Float64() - mb.Index(0).Float64(),
			Height: mb.Index(3).Float64() - mb.Index(1).Float64(),
		}
		for _, g := range p.Content().Text {
			// The reader adds U+FFFD at the end of each text array.
			s := strings.ReplaceAll(g.S, "�", "")
			if n := len(page.Runs); n > 0 && page.Runs[n-1].X == g.X && page.Runs[n-1].Y == g.Y {
				page.Runs[n-1].Text += s
				continue
			}
			page.Runs = append(page.Runs, Run{X: g.X, Y: g.Y, Text: s, Font: g.Font, Size: g.FontSize})
		}
		for ri := range page.Runs {
			page.Runs[ri].W = measure(page.Runs[ri])
		}
		pages = append(pages, page)
	}
	return pages
}

// newMeasurer returns a function that measures a run's width in the
// embedded font it was drawn in, at its size.
func newMeasurer(t testing.TB) func(Run) float64 {
	t.Helper()
	doc := &gopdf.GoPdf{}
	doc.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	doc.AddPage()
	if err := pdffonts.RegisterNotoSans(doc); err != nil {
		t.Fatalf("pdftest: register fonts: %v", err)
	}
	return func(r Run) float64 {
		t.Helper()
		if err := doc.SetFont(r.Font, "", r.Size); err != nil {
			t.Fatalf("pdftest: run %q: set font %q size %.1f: %v", r.Text, r.Font, r.Size, err)
		}
		w, err := doc.MeasureTextWidth(r.Text)
		if err != nil {
			t.Fatalf("pdftest: measure %q: %v", r.Text, err)
		}
		return w
	}
}

// MissingGlyphs returns the runes of s that the embedded regular or bold
// face has no glyph for. The renderers draw such a rune as nothing.
func MissingGlyphs(t testing.TB, s string) []rune {
	t.Helper()
	doc := &gopdf.GoPdf{}
	doc.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	doc.AddPage()
	if err := pdffonts.RegisterNotoSans(doc); err != nil {
		t.Fatalf("pdftest: register fonts: %v", err)
	}
	var missing []rune
	for _, r := range s {
		for _, family := range []string{pdffonts.FamilyRegular, pdffonts.FamilyBold} {
			if err := doc.SetFont(family, "", 10); err != nil {
				t.Fatalf("pdftest: set font %s: %v", family, err)
			}
			ok, err := doc.IsCurrFontContainGlyph(r)
			if err != nil {
				t.Fatalf("pdftest: glyph check %q: %v", r, err)
			}
			if !ok {
				missing = append(missing, r)
				break
			}
		}
	}
	return missing
}
