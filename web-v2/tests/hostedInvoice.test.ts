// The public invoice page speaks to the customer: no server messages, no
// repeated totals, and an honest line when online payment isn't set up.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { hostedTotalRows, loadErrorCopy, payErrorCopy, payUnavailableCopy } from '../src/lib/hostedInvoice.ts'

const base = { status: 'finalized', subtotal_cents: 56600, discount_cents: 0, tax_amount_cents: 0, total_amount_cents: 56600, credits_applied_cents: 0, amount_paid_cents: 0 }

test('a plain invoice shows only the amount due, not three equal rows', () => {
  assert.deepEqual(hostedTotalRows(base), [])
})

test('tax or a discount brings back subtotal and total', () => {
  assert.deepEqual(hostedTotalRows({ ...base, tax_amount_cents: 1000, total_amount_cents: 57600 }), ['subtotal', 'tax', 'total'])
  assert.deepEqual(hostedTotalRows({ ...base, discount_cents: 600, total_amount_cents: 56000 }), ['subtotal', 'discount', 'total'])
})

test('credits or a payment show the total they come off', () => {
  assert.deepEqual(hostedTotalRows({ ...base, credits_applied_cents: 50000 }), ['total', 'credits'])
  assert.deepEqual(hostedTotalRows({ ...base, status: 'paid', amount_paid_cents: 56600 }), ['total', 'paid'])
})

test('pay errors never echo the server message', () => {
  assert.match(payErrorCopy('stripe_error', 'Acme', 502), /couldn.t start the payment\. Nothing was charged/)
  assert.match(payErrorCopy('online_payment_unavailable', 'Acme', 409), /Contact Acme to pay another way/)
  assert.match(payErrorCopy('online_payment_unavailable', '', 409), /Contact the sender of this invoice/)
  assert.match(payErrorCopy('payment_in_flight', 'Acme', 409), /already being processed/)
  assert.match(payErrorCopy('not_payable', 'Acme', 409), /can no longer be paid online/)
  assert.match(payErrorCopy(undefined, 'Acme'), /try again in a few minutes/)
})

test('no Stripe keys: the page names who to contact', () => {
  assert.equal(payUnavailableCopy('Acme'), "Online payment isn't set up for this invoice. Contact Acme to arrange payment.")
})

test('a bad link reads as an invalid link, not "invoice not found"', () => {
  assert.equal(loadErrorCopy(404).title, 'This link isn’t valid')
  assert.equal(loadErrorCopy(500).title, 'We couldn’t load this invoice')
})
