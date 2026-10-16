// The Stripe card used to say "Stripe verification failed: {json}" for a
// refused key and for a network failure alike. The notice now follows the
// kind the server records.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { stripeVerifyNotice } from '../src/lib/stripeVerify.ts'

test('a refused key asks for the current keys, red, with Stripe named in Details', () => {
  const n = stripeVerifyNotice('rejected')
  assert.equal(n.tone, 'red')
  assert.equal(n.title, "Stripe didn't accept these keys")
  assert.match(n.body, /paste them below/)
  assert.equal(n.detailsLabel, "Stripe's response")
  assert.equal(n.toastLevel, 'error')
  assert.equal(n.toastTitle, "Keys saved, but Stripe didn't accept them")
})

test('not checked: says the keys were not tested, not that they are wrong', () => {
  const n = stripeVerifyNotice('unavailable')
  assert.equal(n.tone, 'amber')
  assert.equal(n.title, "Couldn't check keys with Stripe")
  assert.doesNotMatch(n.body + n.title, /accept/)
  assert.match(n.body, /weren't tested/)
  assert.equal(n.toastLevel, 'warning')
  assert.equal(n.toastTitle, 'Keys saved, not checked yet')
})

test('not checked: true when Stripe answered 429/5xx, and Details does not claim Stripe wrote it', () => {
  const n = stripeVerifyNotice('unavailable')
  // The kind covers network failures (our words, not Stripe's) and 429/5xx
  // (Stripe did answer), so neither "didn't answer" nor "Stripe said" holds.
  assert.doesNotMatch(n.body, /didn't answer|not reached|couldn't reach/i)
  assert.doesNotMatch(n.detailsLabel, /Stripe/)
})

test('a row from before the kind was recorded reads as refused, but Details stays neutral', () => {
  const legacy = stripeVerifyNotice(undefined)
  const rejected = stripeVerifyNotice('rejected')
  assert.equal(legacy.title, rejected.title)
  assert.equal(legacy.body, rejected.body)
  assert.equal(legacy.tone, 'red')
  // Old rows stored stripe-go's JSON or a network error, not Stripe's sentence.
  assert.equal(legacy.detailsLabel, 'Details')
})
