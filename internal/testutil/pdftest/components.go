package pdftest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"github.com/ledongthuc/pdf"
)

// MissingComponents checks the fonts a PDF embeds. Each holds only the
// glyphs the document draws (a subset). A composite glyph is drawn from other
// glyphs, and a component can itself be composite: in Noto Sans, é is e plus
// a combining acute, and the combining acute is drawn from the acute accent.
// A viewer draws a letter without each component the subset leaves out, while
// text extraction still reads the letter, so Read cannot see the gap.
//
// It returns a line for each component with no outline in its font. nested
// counts the composite glyphs with a composite component, so a test can show
// it checked that case. The test fails if a font cannot be read.
func MissingComponents(t testing.TB, pdfBytes []byte) (missing []string, nested int) {
	t.Helper()
	r, err := pdf.NewReader(bytes.NewReader(pdfBytes), int64(len(pdfBytes)))
	if err != nil {
		t.Fatalf("pdftest: open pdf: %v", err)
	}
	seen := map[string]bool{}
	for i := 1; i <= r.NumPage(); i++ {
		fonts := r.Page(i).Resources().Key("Font")
		for _, key := range fonts.Keys() {
			font := fonts.Key(key)
			name := font.Key("BaseFont").Name()
			if seen[name] {
				continue
			}
			seen[name] = true
			file := font.Key("DescendantFonts").Index(0).Key("FontDescriptor").Key("FontFile2")
			if file.Kind() != pdf.Stream {
				t.Fatalf("pdftest: font %s embeds no TrueType file", name)
			}
			ttf, err := io.ReadAll(file.Reader())
			if err != nil {
				t.Fatalf("pdftest: read font %s: %v", name, err)
			}
			m, n, err := checkComponents(ttf)
			if err != nil {
				t.Fatalf("pdftest: font %s: %v", name, err)
			}
			for _, line := range m {
				missing = append(missing, name+": "+line)
			}
			nested += n
		}
	}
	return missing, nested
}

// checkComponents reads a TrueType font's glyf and loca tables and reports
// each component glyph with no outline (TrueType reference manual, "glyf").
func checkComponents(ttf []byte) (missing []string, nested int, err error) {
	if len(ttf) < 12 {
		return nil, 0, fmt.Errorf("font file is %d bytes", len(ttf))
	}
	tables := map[string][]byte{}
	for i := range int(binary.BigEndian.Uint16(ttf[4:])) {
		rec := 12 + 16*i
		if rec+16 > len(ttf) {
			return nil, 0, fmt.Errorf("table directory runs past the file")
		}
		off := int(binary.BigEndian.Uint32(ttf[rec+8:]))
		n := int(binary.BigEndian.Uint32(ttf[rec+12:]))
		if off+n > len(ttf) {
			return nil, 0, fmt.Errorf("table %q runs past the file", ttf[rec:rec+4])
		}
		tables[string(ttf[rec:rec+4])] = ttf[off : off+n]
	}
	head, loca, glyf := tables["head"], tables["loca"], tables["glyf"]
	if len(head) < 54 || len(loca) == 0 || glyf == nil {
		return nil, 0, fmt.Errorf("font has no head, loca or glyf table")
	}
	// loca holds the offset of each glyph in glyf, plus the end of the last.
	// indexToLocFormat 0 stores offset/2 in 16 bits, 1 stores it in 32 bits.
	var offsets []int
	if binary.BigEndian.Uint16(head[50:]) == 0 {
		for i := 0; i+2 <= len(loca); i += 2 {
			offsets = append(offsets, 2*int(binary.BigEndian.Uint16(loca[i:])))
		}
	} else {
		for i := 0; i+4 <= len(loca); i += 4 {
			offsets = append(offsets, int(binary.BigEndian.Uint32(loca[i:])))
		}
	}
	outline := func(g int) []byte {
		if g+1 >= len(offsets) || offsets[g] >= offsets[g+1] || offsets[g+1] > len(glyf) {
			return nil
		}
		return glyf[offsets[g]:offsets[g+1]]
	}
	for g := range len(offsets) - 1 {
		hasNested := false
		for _, c := range components(outline(g)) {
			data := outline(c)
			if len(data) == 0 {
				missing = append(missing, fmt.Sprintf("glyph %d: component glyph %d has no outline", g, c))
				continue
			}
			hasNested = hasNested || len(components(data)) > 0
		}
		if hasNested {
			nested++
		}
	}
	return missing, nested, nil
}

// components returns the glyph ids a composite glyph is drawn from, or nil
// for a simple or empty glyph. A glyph with a negative contour count is
// composite. After its 10-byte header, each component record holds flags,
// a glyph id, two offsets and an optional transform; the flags give their
// sizes and say whether another record follows.
func components(glyph []byte) []int {
	const (
		argsAreWords   = 0x0001
		haveScale      = 0x0008
		moreComponents = 0x0020
		haveXYScale    = 0x0040
		haveTwoByTwo   = 0x0080
	)
	if len(glyph) < 10 || int16(binary.BigEndian.Uint16(glyph)) >= 0 {
		return nil
	}
	var ids []int
	for at := 10; at+4 <= len(glyph); {
		flags := binary.BigEndian.Uint16(glyph[at:])
		ids = append(ids, int(binary.BigEndian.Uint16(glyph[at+2:])))
		at += 4 + 2
		if flags&argsAreWords != 0 {
			at += 2
		}
		switch {
		case flags&haveScale != 0:
			at += 2
		case flags&haveXYScale != 0:
			at += 4
		case flags&haveTwoByTwo != 0:
			at += 8
		}
		if flags&moreComponents == 0 {
			break
		}
	}
	return ids
}
