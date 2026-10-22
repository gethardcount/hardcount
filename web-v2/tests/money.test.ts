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
  dunningAmountDue,
  formatCents,
  formatCentsBare,
  formatCompactCents,
  getCurrencySymbol,
  outstandingText,
  prorationText,
  setActiveCurrency,
  sumByCurrency,
} from '../src/lib/money.ts'

beforeEach(() => setActiveCurrency('USD'))

test('an explicit currency wins over the workspace default', () => {
  assert.equal(formatCents(120000, 'EUR'), '€1,200.00')
  assert.equal(formatCents(120000, 'GBP'), '£1,200.00')
})

// A EUR credit read "$20.00" on the Credits page and a EUR credit note was
// added into a dollar total: formatCents(cents) fell back to the workspace
// default without a word. There is no default now, so the type checker lists
// every call that has no currency, and a call that slips past it fails loudly
// instead of printing the wrong symbol.
test('formatCents and getCurrencySymbol have no default currency', () => {
  // @ts-expect-error a currency is required
  assert.throws(() => formatCents(120000))
  // @ts-expect-error a currency is required
  assert.throws(() => getCurrencySymbol())
})

test('an amount whose currency was never recorded is shown without a symbol', () => {
  assert.equal(formatCentsBare(120050), '1,200.50')
  assert.equal(formatCentsBare(-5), '-0.05')
})

test('sums per currency never add one currency into another', () => {
  assert.deepEqual(
    sumByCurrency([
      { currency: 'USD', amount_cents: 1000 },
      { currency: 'EUR', amount_cents: 5000 },
      { currency: 'usd', amount_cents: 500 },
      { currency: 'GBP', amount_cents: 0 },
    ]),
    [
      { currency: 'EUR', amount_cents: 5000 },
      { currency: 'USD', amount_cents: 1500 },
    ],
  )
  assert.deepEqual(sumByCurrency([]), [])
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
  assert.equal(formatCents(amountInputToCents('0.1'), 'USD'), '$0.10')
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
})

test('proration toasts use the subscription currency', () => {
  setActiveCurrency('CAD')
  assert.equal(prorationText({ type: 'invoice', amount_cents: 1200 }, 'USD'), 'Proration invoice created for $12.00')
  assert.equal(prorationText({ type: 'adjustment', amount_cents: -1200 }, 'EUR'), 'Open invoice reduced by €12.00')
  assert.equal(prorationText({ type: 'credit', amount_cents: -1200 }, 'GBP'), '£12.00 credited to customer balance')
})

// The Home revenue chart's Y axis wrote a literal "$": a GBP workspace read
// "$" on the axis beside a "£5,000.00" tooltip and an "in GBP only" hint. It
// also divided by 100_00 for a thousand, so £5,000 read "50.0K".
test('a compact chart amount names its currency and its size', () => {
  assert.equal(formatCompactCents(500000, 'GBP'), '£5.0K')
  assert.equal(formatCompactCents(25000000, 'EUR'), '€250K')
  assert.equal(formatCompactCents(4200, 'USD'), '$42')
  assert.equal(formatCompactCents(-500000, 'GBP'), '-£5.0K')
  assert.equal(formatCompactCents(100000, 'USD'), '$1.0K')
  assert.equal(formatCompactCents(0, 'CAD'), 'CA$0')
})
