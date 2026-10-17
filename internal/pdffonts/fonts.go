// Package pdffonts embeds the TTF fonts used by Velox's PDF renderers
// (invoice, credit note). Centralizing the fonts avoids duplicating the
// ~1 MB font binaries per domain package and removes the runtime.Caller
// filesystem trick the first renderer shipped with — the compiled binary
// carries the fonts and renders work identically on any machine.
//
// Noto Sans is the default family. The EMBEDDED subset covers:
//   - Latin (basic + extended), Greek, Cyrillic, IPA
//   - Currency symbols ($, €, £, ₹, ¥, ₩, ₱, ₫, …)
//   - Basic punctuation including em/en dash, bullet (•), middle dot (·)
//
// What it does NOT cover (verified 2026-05-25 via fontTools):
//   - Arrows block (→ U+2192, ↳ U+21B3, etc.)
//   - Checkmarks (✓ U+2713, ✗ U+2717)
//   - Devanagari / Arabic / Hebrew / Thai / CJK scripts
//   - The baht sign (฿ U+0E3F), which sits in the Thai block, so THB
//     amounts print as "THB 1,234.00" (checked 2026-10-17 with gopdf's
//     IsCurrFontContainGlyph; the renderers' symbol tests pin it)
//
// PDF renderers MUST use ASCII-safe glyphs for layout chrome (use "to"
// instead of "→", drop arrow prefixes, etc.). Customer-facing content
// (names, memos) in CJK / Devanagari / Arabic will render as
// missing-glyph boxes. If a tenant needs full multi-script support,
// swap in the full Noto Sans family (separate TTFs per script — Noto
// CJK is 16 MB alone, so we keep the basic subset by default and
// document the upgrade path).
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

// Family names the renderers pass to SetFont. Exported so callers don't
// duplicate the magic strings.
const (
	FamilyRegular = "noto"
	FamilyBold    = "noto-bold"
)

// RegisterNotoSans loads the embedded Regular and Bold faces into pdf
// under FamilyRegular and FamilyBold. Safe to call once per *gopdf.GoPdf
// right after Start().
func RegisterNotoSans(pdf *gopdf.GoPdf) error {
	if err := pdf.AddTTFFontData(FamilyRegular, regularTTF); err != nil {
		return fmt.Errorf("load regular font: %w", err)
	}
	if err := pdf.AddTTFFontData(FamilyBold, boldTTF); err != nil {
		return fmt.Errorf("load bold font: %w", err)
	}
	return nil
}
