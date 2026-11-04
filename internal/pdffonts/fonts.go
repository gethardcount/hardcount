// Package pdffonts embeds the TTF fonts that Hardcount's PDF renderers use
// (invoice, credit note). Keeping the fonts in one package avoids a copy of
// the ~1 MB font files in each domain package. The compiled binary carries
// the fonts, so rendering works the same on any machine.
//
// Noto Sans is the default family. The embedded subset covers:
//   - Latin (basic and extended), Greek, Cyrillic and IPA
//   - Currency symbols ($, €, £, ₹, ¥, ₩, ₱, ₫, …)
//   - Basic punctuation, including the em and en dash, the bullet (•) and
//     the middle dot (·)
//
// It does not cover these (checked with fontTools):
//   - The Arrows block (→ U+2192, ↳ U+21B3, and so on)
//   - Checkmarks (✓ U+2713, ✗ U+2717)
//   - The Devanagari, Arabic, Hebrew, Thai and CJK scripts
//   - The baht sign (฿ U+0E3F), which sits in the Thai block, so THB
//     amounts print as "THB 1,234.00". This was checked with gopdf's
//     IsCurrFontContainGlyph, and the renderers' symbol tests pin it.
//
// PDF renderers must use ASCII-safe glyphs for layout chrome: "to" instead
// of "→", no arrow prefixes, and so on. Customer-facing content (names,
// memos) in CJK, Devanagari or Arabic renders as missing-glyph boxes. If a
// tenant needs full multi-script support, swap in the full Noto Sans
// family, which has a separate TTF per script. Noto CJK alone is 16 MB, so
// the basic subset is the default and the upgrade path is documented.
package pdffonts

import (
	_ "embed"
	"fmt"

	"github.com/signintech/gopdf"
)

//go:embed NotoSans-Regular.ttf
var regularTTF []byte

//go:embed NotoSans-Bold.ttf
var boldTTF []byte

// Family names the renderers pass to SetFont. They are exported so callers
// do not repeat the magic strings.
const (
	FamilyRegular = "noto"
	FamilyBold    = "noto-bold"
)

// RegisterNotoSans loads the embedded Regular and Bold faces into pdf
// under FamilyRegular and FamilyBold. It is safe to call once per
// *gopdf.GoPdf, right after Start().
func RegisterNotoSans(pdf *gopdf.GoPdf) error {
	if err := pdf.AddTTFFontData(FamilyRegular, regularTTF); err != nil {
		return fmt.Errorf("load regular font: %w", err)
	}
	if err := pdf.AddTTFFontData(FamilyBold, boldTTF); err != nil {
		return fmt.Errorf("load bold font: %w", err)
	}
	return nil
}
