// Package pdftest reads back what Velox's PDF renderers drew, for tests.
//
// Read returns each page's text with its position. Position matters: text
// drawn below a page's bottom edge is still in the file, so finding a
// string in the file does not prove a reader can see it. A test must check
// that the string sits inside a page.
//
// MissingGlyphs reports the characters the embedded fonts cannot draw, so a
// test can prove a string the renderers print never shows up blank.
package pdftest

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
	"github.com/signintech/gopdf"

	"github.com/sagarsuperuser/velox/internal/pdffonts"
)

// Run is the text of one text-drawing operation. X and Y are in points from
// the page's bottom-left corner; Y is the text's baseline.
type Run struct {
	X, Y float64
	Text string
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
			page.Runs = append(page.Runs, Run{X: g.X, Y: g.Y, Text: s})
		}
		pages = append(pages, page)
	}
	return pages
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
