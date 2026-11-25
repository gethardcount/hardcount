// The no-card notice fires only for a subscription that will try to charge
// a card.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { customerNeedsPaymentMethod, subscriptionWillAutoCharge } from '../src/lib/paymentReadiness.ts'

test('active subscriptions auto-charge; paused collection and ended ones do not', () => {
  assert.equal(subscriptionWillAutoCharge({ status: 'active' }), true)
  assert.equal(subscriptionWillAutoCharge({ status: 'active', pause_collection: { behavior: 'keep_as_draft' } as never }), false)
  assert.equal(subscriptionWillAutoCharge({ status: 'canceled' }), false)
})

test('an active subscription with a scheduled cancel still bills its period', () => {
  const scheduled = { status: 'active' as const, cancel_at_period_end: true, cancel_effective_at: '2026-11-01T00:00:00Z' }
  assert.equal(subscriptionWillAutoCharge(scheduled), true)
})

test('customer needs a card only when no card and some subscription will charge', () => {
  const paused = { status: 'active' as const, pause_collection: { behavior: 'keep_as_draft' } as never }
  assert.equal(customerNeedsPaymentMethod([paused, { status: 'canceled' }], []), false)
  assert.equal(customerNeedsPaymentMethod([paused, { status: 'active' }], []), true)
  assert.equal(customerNeedsPaymentMethod([{ status: 'active' }], [{ is_default: true }]), false)
})
