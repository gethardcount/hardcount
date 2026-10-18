package pdffonts

import (
	"fmt"
	"strings"

	"github.com/signintech/gopdf"
)

// WrapText splits text into the lines it takes when no line may be wider
// than width points in pdf's current font. Lines break between words, and
// inside a word only when the word alone is wider than a line. A newline
// in text starts a new line. Text with nothing to print is one empty line,
// so a caller always has a first line to draw.
//
// The invoice and credit-note PDFs wrap with it, so a long line
// description or totals label prints whole instead of being cut or
// running into the next column.
func WrapText(pdf *gopdf.GoPdf, text string, width float64) ([]string, error) {
	if strings.TrimSpace(text) == "" {
		return []string{""}, nil
	}
	// gopdf's splitter never finishes when one character is wider than the
	// line: it keeps starting empty lines. A line as wide as the widest
	// character always takes at least one.
	for _, r := range text {
		w, err := pdf.MeasureTextWidth(string(r))
		if err != nil {
			return nil, fmt.Errorf("wrap text: %w", err)
		}
		width = max(width, w)
	}
	lines, err := pdf.SplitTextWithWordWrap(text, width)
	if err != nil {
		return nil, fmt.Errorf("wrap text: %w", err)
	}
	// A break that falls just before a space leaves the space at the start
	// of the next line.
	for i := 1; i < len(lines); i++ {
		lines[i] = strings.TrimLeft(lines[i], " ")
	}
	return lines, nil
}
