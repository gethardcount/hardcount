// Pins "an invoice's amount is shown in the invoice's currency". The bug class
// renders perfectly: a €1,200.00 invoice read "$1,200.00" in Record offline
// payment, Dunning resolve and the credit-note dialog because the call sites
// fell back to the workspace default. Every assertion here runs with the
// default set to USD, so a fallback shows up as a "$".
import { test, beforeEach } from 'vitest'
import assert from 'node:assert/strict'
import {
  amountInputToCents,
  amountsByCurrencyText,
  atRiskText,
  formatCents,
  formatCompactCents,
  outstandingText,
  prorationText,
  setActiveCurrency,
} from '../src/lib/money.ts'
import { dunningAmountDue } from '../src/lib/invoiceTerminal.ts'

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

test('a run on a voided invoice is chasing nothing, whatever the join says', () => {
  // amount_due survives a void, so the joined figure still read "$29.00"
  // under Amount Due on the Dunning list (sweep A7 / E1).
  assert.equal(
    dunningAmountDue({ invoice_amount_due_cents: 2900, invoice_currency: 'USD' }, { status: 'voided', amount_due_cents: 2900, currency: 'USD' }),
    '$0.00',
  )
  // Negative control: an open invoice still shows what is due.
  assert.equal(
    dunningAmountDue({ invoice_amount_due_cents: 2900, invoice_currency: 'USD' }, { status: 'finalized', amount_due_cents: 2900, currency: 'USD' }),
    '$29.00',
  )
})

test('formatCompactCents: the chart axis reads $1.2K for $1,200, in the given currency', () => {
  // The Home revenue axis divided by 100 * 100 instead of 100 * 1000, so
  // $1,200 read "$12.0K" (sweep E4).
  assert.equal(formatCompactCents(120000, 'USD'), '$1.2K')
  assert.equal(formatCompactCents(1500000, 'USD'), '$15K')
  assert.equal(formatCompactCents(25000000, 'USD'), '$250K')
  assert.equal(formatCompactCents(9900, 'USD'), '$99')
  assert.equal(formatCompactCents(0, 'USD'), '$0')
  // The tenant's currency, never a hard-coded "$".
  assert.equal(formatCompactCents(120000, 'EUR'), '€1.2K')
  assert.equal(formatCompactCents(120000, 'XYZ'), 'XYZ 1.2K')
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

// The customer page and the Dunning card after the workspace default moved
// from USD to CAD: USD invoices' amounts read CA$. These run with the default
// set to CAD, so a fallback to the default shows up as "CA$".
test('an aggregate over invoices lists each currency, never the default', () => {
  setActiveCurrency('CAD')
  assert.equal(amountsByCurrencyText([{ currency: 'USD', amount_cents: 73900 }]), '$739.00')
  assert.equal(
    amountsByCurrencyText([
      { currency: 'USD', amount_cents: 73900 },
      { currency: 'EUR', amount_cents: 4900 },
    ]),
    '$739.00 + €49.00',
  )
  // Zero entries are dropped; nothing owed is null, not "CA$0.00".
  assert.equal(amountsByCurrencyText([{ currency: 'USD', amount_cents: 0 }]), null)
  assert.equal(amountsByCurrencyText([]), null)
  // Beyond three currencies the tail is counted.
  assert.equal(
    amountsByCurrencyText([
      { currency: 'USD', amount_cents: 100 },
      { currency: 'EUR', amount_cents: 200 },
      { currency: 'GBP', amount_cents: 300 },
      { currency: 'JPY', amount_cents: 400 },
    ]),
    '$1.00 + €2.00 + 2 other currencies',
  )
})

test('At risk shows every currency with runs, not the default-only figure', () => {
  setActiveCurrency('CAD')
  assert.equal(
    atRiskText({ at_risk_cents: 0, currency: 'CAD', at_risk_by_currency: [{ currency: 'USD', amount_cents: 73900 }] }),
    '$739.00',
  )
  // Nothing at risk: zero in the default currency.
  assert.equal(atRiskText({ at_risk_cents: 0, currency: 'CAD', at_risk_by_currency: [] }), 'CA$0.00')
  // A server without the list: the default-only figure, in its currency.
  assert.equal(atRiskText({ at_risk_cents: 1500, currency: 'EUR' }), '€15.00')
})

test('proration toasts use the subscription currency', () => {
  setActiveCurrency('CAD')
  assert.equal(prorationText({ type: 'invoice', amount_cents: 1200 }, 'USD'), 'Proration invoice created for $12.00')
  assert.equal(prorationText({ type: 'adjustment', amount_cents: -1200 }, 'EUR'), 'Open invoice reduced by €12.00')
  assert.equal(prorationText({ type: 'credit', amount_cents: -1200 }, 'GBP'), '£12.00 credited to customer balance')
})
