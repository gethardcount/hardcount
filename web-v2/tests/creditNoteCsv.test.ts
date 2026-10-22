// The Credit Notes export wrote bare amounts: a EUR note and a USD note sat
// side by side with nothing to tell them apart.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { CREDIT_NOTE_CSV_HEADERS, creditNoteCsvRow } from '../src/lib/creditNoteCsv.ts'

const note = {
  credit_note_number: 'CN-0001',
  status: 'issued',
  currency: 'EUR',
  refund_amount_cents: 0,
  refund_status: '',
  credit_amount_cents: 5000,
  out_of_band_amount_cents: 0,
  reason: 'duplicate',
  total_cents: 5000,
  issued_at: '2026-10-22T00:00:00Z',
  created_at: '2026-10-21T00:00:00Z',
}

test('each credit note row names its currency', () => {
  const row = creditNoteCsvRow(note, 'Acme', 'INV-0001')
  assert.equal(row.length, CREDIT_NOTE_CSV_HEADERS.length)
  const at = (h: string) => row[CREDIT_NOTE_CSV_HEADERS.indexOf(h)]
  assert.equal(at('Currency'), 'EUR')
  assert.equal(at('Amount'), '50.00')
  assert.equal(at('Customer'), 'Acme')
  assert.equal(at('Invoice'), 'INV-0001')
})
