// Pins what the Next Invoice Preview card says: the preview's warnings, and
// words in place of a table with no lines.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { invoicePreviewNotes, NO_INVOICE_EXPECTED } from '../src/lib/invoicePreviewNotes.ts'

const NO_PRICE = 'meter "e2e_calls": 120 call matched no price of the plan — not billed'

test('no lines: the warning says why, in place of the table', () => {
  assert.deepEqual(invoicePreviewNotes({ lines: [], warnings: [NO_PRICE] }), { empty: true, notes: [NO_PRICE] })
})

test('no lines and no warning: no invoice is expected', () => {
  assert.deepEqual(invoicePreviewNotes({ lines: [], warnings: [] }), { empty: true, notes: [NO_INVOICE_EXPECTED] })
  assert.deepEqual(invoicePreviewNotes({ lines: [], warnings: null }), { empty: true, notes: [NO_INVOICE_EXPECTED] })
})

test('lines with warnings: every warning is shown under the table', () => {
  const segments = 'Estimate shows each base fee for the whole window; the plan, quantity, or item set changed during it, so the actual invoice may prorate base fees per segment.'
  assert.deepEqual(invoicePreviewNotes({ lines: [{}], warnings: [segments, '  '] }), { empty: false, notes: [segments] })
  assert.deepEqual(invoicePreviewNotes({ lines: [{}], warnings: [] }), { empty: false, notes: [] })
})
