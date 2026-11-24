// Pins what each Home card says it counts, and how a card shows money in
// several currencies.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import {
  CREDIT_HELD_DEFINITION, CREDIT_SOLD_DEFINITION, PAID_DEFINITION, USAGE_BILLED_DEFINITION,
  amountsCardValue, currencyScopedHint,
} from '../src/lib/homeKpis.ts'

test('each card says what it counts, in the words the server computes it with', () => {
  assert.equal(CREDIT_HELD_DEFINITION, 'Credit your customers can still spend, in each currency.')
  // A top-up sold at a discount grants more than was paid.
  assert.equal(CREDIT_SOLD_DEFINITION, 'Credit bought with top-ups paid in this period. A discounted top-up counts the credit it gave, not its price.')
  // Credit applied to an invoice is not counted again as a payment.
  assert.match(PAID_DEFINITION, /Credit is counted once, when it was bought\.$/)
  assert.equal(USAGE_BILLED_DEFINITION, 'Usage on invoices issued in this period. Void invoices are not counted.')
})

test('no definition uses the subscription words Home no longer shows', () => {
  for (const d of [CREDIT_HELD_DEFINITION, CREDIT_SOLD_DEFINITION, PAID_DEFINITION, USAGE_BILLED_DEFINITION]) {
    assert.doesNotMatch(d, /MRR|ARR|churn|subscription|credits\b/i)
  }
})

test('a money card names every currency and never adds two together', () => {
  const both = [{ currency: 'USD', amount_cents: 10000 }, { currency: 'EUR', amount_cents: 2500 }]
  assert.equal(amountsCardValue(both, 'USD'), '$100.00 + €25.00')
  assert.equal(amountsCardValue([{ currency: 'USD', amount_cents: 10000 }], 'USD'), '$100.00')
})

test('a money card with nothing shows zero in the workspace currency', () => {
  assert.equal(amountsCardValue([], 'EUR'), '€0.00')
})

// Paid sums the workspace default currency only (internal/analytics
// overview.go). The hint says which currency the figure covers.
test('the Paid hint names the one currency it covers', () => {
  assert.equal(currencyScopedHint('3 invoices paid', 'EUR'), '3 invoices paid · in EUR only')
})
