// The Stripe card used to say "Stripe verification failed: {json}" for a
// refused key and for a network failure alike. The notice now follows the
// kind the server records.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { stripeVerifyNotice } from '../src/lib/stripeVerify.ts'

test('a refused key asks for the current keys', () => {
  const n = stripeVerifyNotice('rejected')
  assert.equal(n.title, "Stripe didn't accept these keys")
  assert.match(n.body, /paste them below/)
  assert.equal(n.toastTitle, "Keys saved, but Stripe didn't accept them")
})

test('Stripe not reached: says the keys were not tested, not that they are wrong', () => {
  const n = stripeVerifyNotice('unavailable')
  assert.equal(n.title, "Couldn't check keys with Stripe")
  assert.doesNotMatch(n.body + n.title, /accept/)
  assert.match(n.body, /weren't tested/)
  assert.equal(n.toastTitle, 'Keys saved, not checked yet')
})

test('a row from before the kind was recorded reads as refused', () => {
  assert.deepEqual(stripeVerifyNotice(undefined), stripeVerifyNotice('rejected'))
})
