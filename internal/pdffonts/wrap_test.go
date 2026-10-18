package pdffonts

import (
	"fmt"
	"strings"
	"testing"

	"github.com/signintech/gopdf"
)

func newDoc(t *testing.T) *gopdf.GoPdf {
	t.Helper()
	doc := &gopdf.GoPdf{}
	doc.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	doc.AddPage()
	if err := RegisterNotoSans(doc); err != nil {
		t.Fatal(err)
	}
	if err := doc.SetFont(FamilyRegular, "", 9); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestWrapText(t *testing.T) {
	doc := newDoc(t)
	words := "Claude Sonnet 4 output tokens, batch API, workspace research-eu-west1, project atlas-retrieval, April 1 to April 30 2026"
	unbroken := strings.Repeat("0123456789", 18)
	for _, tc := range []struct {
		name  string
		text  string
		width float64
		join  string // how the lines read back as text
		want  []string
	}{
		{name: "fits on one line", text: "Usage line 01", width: 232, want: []string{"Usage line 01"}},
		{name: "breaks between words", text: words, width: 232, join: " "},
		{name: "breaks inside a word wider than a line", text: unbroken, width: 232, join: ""},
		{name: "a newline starts a line", text: "first\nsecond", width: 232, want: []string{"first", "second"}},
		{name: "empty", text: "", width: 232, want: []string{""}},
		{name: "only spaces", text: "   ", width: 232, want: []string{""}},
		// A width narrower than any character still ends, one per line.
		{name: "width under one character", text: "abc", width: 0, want: []string{"a", "b", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, err := WrapText(doc, tc.text, tc.width)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != nil {
				if fmt.Sprintf("%q", lines) != fmt.Sprintf("%q", tc.want) {
					t.Fatalf("lines %q, want %q", lines, tc.want)
				}
				return
			}
			if len(lines) < 2 {
				t.Fatalf("lines %q, want the text wrapped", lines)
			}
			if got := strings.Join(lines, tc.join); got != tc.text {
				t.Errorf("lines read back as %q, want %q", got, tc.text)
			}
			for _, l := range lines {
				w, err := doc.MeasureTextWidth(l)
				if err != nil {
					t.Fatal(err)
				}
				if w > tc.width {
					t.Errorf("line %q is %.1fpt wide, more than %.0f", l, w, tc.width)
				}
				if strings.HasPrefix(l, " ") {
					t.Errorf("line %q starts with a space", l)
				}
			}
		})
	}
}
