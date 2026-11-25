// What the Next Invoice Preview card says beside (or instead of) its lines:
// the preview's warnings, which the server writes in plain sentences, such
// as usage that matched no price. A preview with no lines is never an empty
// table with no words.
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
