// Pins what the Next Invoice Preview card says. The defect: it never showed
// the preview's warnings, so a trial set to cancel at its end read as an
// empty table with no words.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { invoicePreviewNotes, NO_INVOICE_EXPECTED } from '../src/lib/invoicePreviewNotes.ts'

const CANCEL = 'A cancel is scheduled for the end of the trial, so no invoice will be issued.'

test('no lines: the warning says why, in place of the table', () => {
  assert.deepEqual(invoicePreviewNotes({ lines: [], warnings: [CANCEL] }), { empty: true, notes: [CANCEL] })
})

test('no lines and no warning: no invoice is expected', () => {
  assert.deepEqual(invoicePreviewNotes({ lines: [], warnings: [] }), { empty: true, notes: [NO_INVOICE_EXPECTED] })
  assert.deepEqual(invoicePreviewNotes({ lines: [], warnings: null }), { empty: true, notes: [NO_INVOICE_EXPECTED] })
})

test('lines with warnings: every warning is shown under the table', () => {
  const usage = 'This is the invoice issued when the trial ends. Usage for this period is on the invoice at the end of the period.'
  assert.deepEqual(invoicePreviewNotes({ lines: [{}], warnings: [usage, '  '] }), { empty: false, notes: [usage] })
  assert.deepEqual(invoicePreviewNotes({ lines: [{}], warnings: [] }), { empty: false, notes: [] })
})
