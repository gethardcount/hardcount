// Pins "an invoice's amount is shown in the invoice's currency". The bug class
// renders perfectly: a €1,200.00 invoice read "$1,200.00" in Record offline
// payment, Dunning resolve and the credit-note dialog because the call sites
// fell back to the workspace default. Every assertion here runs with the
// default set to USD, so a fallback shows up as a "$".
import { test, beforeEach } from 'node:test'
import assert from 'node:assert/strict'
import {
  amountInputToCents,
  dunningAmountDue,
  formatCents,
  outstandingText,
  setActiveCurrency,
} from '../src/lib/money.ts'

beforeEach(() => setActiveCurrency('USD'))

test('an explicit currency wins over the workspace default', () => {
  assert.equal(formatCents(120000, 'EUR'), '€1,200.00')
  assert.equal(formatCents(120000, 'GBP'), '£1,200.00')
  // Negative control: without a currency, the default is used.
  assert.equal(formatCents(120000), '$1,200.00')
})

test('an unknown currency code is shown as the code, never as the default symbol', () => {
  assert.equal(formatCents(500, 'XYZ'), 'XYZ 5.00')
})

test('amountInputToCents is what both the form and its confirm use', () => {
  assert.equal(amountInputToCents('50'), 5000)
  assert.equal(amountInputToCents('19.99'), 1999)
  assert.equal(amountInputToCents(''), 0)
  assert.equal(amountInputToCents('abc'), 0)
  // The confirm shows these cents, so it can't disagree with what is sent.
  assert.equal(formatCents(amountInputToCents('0.1')), '$0.10')
})

test('dunning amount uses the run invoice currency', () => {
  assert.equal(
    dunningAmountDue({ invoice_amount_due_cents: 4200, invoice_currency: 'EUR' }),
    '€42.00',
  )
})

test('dunning amount falls back to the loaded invoice, in its currency', () => {
  // The join came back empty (amount 0) — the invoice on the page decides.
  assert.equal(
    dunningAmountDue({ invoice_amount_due_cents: 0 }, { amount_due_cents: 990, currency: 'GBP' }),
    '£9.90',
  )
  assert.equal(dunningAmountDue({}), null)
})

test('outstanding names every currency it sums', () => {
  assert.equal(
    outstandingText(5, [
      { currency: 'USD', amount_cents: 20000 },
      { currency: 'EUR', amount_cents: 30000 },
    ]),
    '5 open invoices · $200.00 + €300.00 outstanding',
  )
})

test('outstanding in a non-default currency only is still shown, in that currency', () => {
  // outstanding_ar (default currency only) was 0 here, so the row used to vanish.
  assert.equal(
    outstandingText(1, [{ currency: 'EUR', amount_cents: 1000 }]),
    '1 open invoice · €10.00 outstanding',
  )
})

test('outstanding with more than three currencies counts the rest', () => {
  assert.equal(
    outstandingText(4, [
      { currency: 'USD', amount_cents: 100 },
      { currency: 'EUR', amount_cents: 200 },
      { currency: 'GBP', amount_cents: 300 },
      { currency: 'JPY', amount_cents: 400 },
    ]),
    '4 open invoices · $1.00 + €2.00 + 2 other currencies outstanding',
  )
})

test('outstanding skips zero rows and says only the count when nothing is owed', () => {
  assert.equal(outstandingText(2, [{ currency: 'USD', amount_cents: 0 }]), '2 open invoices')
})
