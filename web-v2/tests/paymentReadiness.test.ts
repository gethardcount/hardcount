// The no-card notice fires only for a subscription that will try to charge
// a card. A trial set to cancel at or before its end never charges
// (trials are free, ADR-069), so it must not claim renewals.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { customerNeedsPaymentMethod, subscriptionWillAutoCharge } from '../src/lib/paymentReadiness.ts'

const TRIAL_END = '2026-11-01T00:00:00Z'

test('active and trialing subscriptions auto-charge; paused collection and ended ones do not', () => {
  assert.equal(subscriptionWillAutoCharge({ status: 'active' }), true)
  assert.equal(subscriptionWillAutoCharge({ status: 'trialing', trial_end_at: TRIAL_END }), true)
  assert.equal(subscriptionWillAutoCharge({ status: 'active', pause_collection: { behavior: 'keep_as_draft' } as never }), false)
  assert.equal(subscriptionWillAutoCharge({ status: 'canceled' }), false)
})

test('a trial that cancels at or before trial end never charges', () => {
  assert.equal(subscriptionWillAutoCharge({ status: 'trialing', trial_end_at: TRIAL_END, cancel_effective_at: TRIAL_END }), false)
  assert.equal(subscriptionWillAutoCharge({ status: 'trialing', trial_end_at: TRIAL_END, cancel_effective_at: '2026-10-20T00:00:00Z' }), false)
  // Cancel after the trial: the first paid period is billed first.
  assert.equal(subscriptionWillAutoCharge({ status: 'trialing', trial_end_at: TRIAL_END, cancel_effective_at: '2026-12-01T00:00:00Z' }), true)
  // An active subscription with a scheduled cancel still bills its period.
  assert.equal(subscriptionWillAutoCharge({ status: 'active', cancel_effective_at: TRIAL_END }), true)
})

test('customer needs a card only when no card and some subscription will charge', () => {
  const freeTrial = { status: 'trialing', trial_end_at: TRIAL_END, cancel_effective_at: TRIAL_END }
  assert.equal(customerNeedsPaymentMethod([freeTrial], []), false)
  assert.equal(customerNeedsPaymentMethod([freeTrial, { status: 'active' }], []), true)
  assert.equal(customerNeedsPaymentMethod([{ status: 'active' }], [{ is_default: true }]), false)
})
