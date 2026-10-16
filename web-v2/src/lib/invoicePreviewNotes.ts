// What the Next Invoice Preview card says beside (or instead of) its lines.
//
// The card showed only the lines and the subtotal, never the preview's
// warnings. A trial set to cancel at its end previews no lines, so the card
// was an empty table with no words; a trialing in-advance plan with meters
// showed only the base fee, with nothing saying usage comes on a later
// invoice. The server's warnings already say both in plain sentences.
export const NO_INVOICE_EXPECTED = 'No invoice is expected for this period.'

// invoicePreviewNotes: `empty` is true when the preview has no lines (the
// card then shows `notes` in place of the table). `notes` are the server's
// warnings; with no lines and no warnings, the one note is
// NO_INVOICE_EXPECTED.
export function invoicePreviewNotes(p: { lines: readonly unknown[]; warnings?: readonly string[] | null }): { empty: boolean; notes: string[] } {
  const warnings = (p.warnings ?? []).map(w => w.trim()).filter(w => w !== '')
  const empty = p.lines.length === 0
  if (empty && warnings.length === 0) return { empty, notes: [NO_INVOICE_EXPECTED] }
  return { empty, notes: warnings }
}
