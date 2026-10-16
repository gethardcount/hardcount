// Pins the Home KPI baselines: each trend compares two server numbers with
// one definition. The bug class: Failed payments compared against a
// hard-coded 0, and Active customers against "active minus new", which read
// -1 on real data.
import { test } from 'vitest'
import assert from 'node:assert/strict'
import { customersTrend, failedPaymentsTrend, subscriptionsHint } from '../src/lib/homeKpis.ts'

const base = {
  live_customers: 3,
  live_customers_prev: 2,
  active_subscriptions: 3,
  trialing_subscriptions: 1,
  failed_payments: 1,
  failed_payments_prev: 4,
}

test('failed payments compare against the previous window from the server', () => {
  assert.deepEqual(failedPaymentsTrend(base), { current: 1, previous: 4 })
})

test('customers compare live now with live at the window start', () => {
  assert.deepEqual(customersTrend(base), { current: 3, previous: 2 })
})

test('a baseline is never negative', () => {
  assert.deepEqual(customersTrend({ ...base, live_customers_prev: -1 }), { current: 3, previous: 0 })
  assert.deepEqual(failedPaymentsTrend({ ...base, failed_payments_prev: Number.NaN }), { current: 1, previous: 0 })
})

test('the hint names trialing subscriptions only when there are some', () => {
  assert.equal(subscriptionsHint(base), '3 active subscriptions · 1 trialing subscription')
  assert.equal(subscriptionsHint({ ...base, active_subscriptions: 2, trialing_subscriptions: 1 }), '2 active subscriptions · 1 trialing subscription')
  assert.equal(subscriptionsHint({ ...base, active_subscriptions: 1, trialing_subscriptions: 2 }), '1 active subscription · 2 trialing subscriptions')
  assert.equal(subscriptionsHint({ ...base, trialing_subscriptions: 0 }), '3 active subscriptions')
  assert.equal(subscriptionsHint({ ...base, active_subscriptions: 1, trialing_subscriptions: 0 }), '1 active subscription')
})
