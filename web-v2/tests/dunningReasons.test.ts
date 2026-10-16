// The Dunning page printed reason tokens as-is, and showed a card-less run's
// start cause in a red "Last failure reason" box for a payment that never
// happened. Reasons now read as words.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { dunningReasonLabel } from '../src/lib/dunningReasons.ts'

test('start causes', () => {
  assert.equal(dunningReasonLabel('no_payment_method'), 'No payment method on file')
  assert.equal(dunningReasonLabel('payment_failed'), 'Card payment failed')
})

test('joined escalation outcomes', () => {
  assert.equal(
    dunningReasonLabel('subscription_paused+invoice_marked_uncollectible'),
    'Subscription paused · Invoice written off',
  )
})

test('resolve reasons, including the "invoice <resolution>" form', () => {
  assert.equal(dunningReasonLabel('payment_recovered'), 'Payment recovered')
  assert.equal(dunningReasonLabel('invoice_paid'), 'Invoice paid')
  assert.equal(dunningReasonLabel('invoice invoice_voided'), 'Invoice voided')
})

test('free text (a card decline) is left as written; an unknown token is humanised', () => {
  assert.equal(dunningReasonLabel('Your card was declined.'), 'Your card was declined.')
  assert.equal(dunningReasonLabel('brand_new_token'), 'Brand new token')
  assert.equal(dunningReasonLabel(''), '')
})
